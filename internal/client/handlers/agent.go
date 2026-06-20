package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lujiang/lujiang/internal/agent"
	"github.com/lujiang/lujiang/internal/client/store"
	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/tunnelmux"
)

// AgentManager 维护 client 侧活跃的 agent 会话。
//
// 设计（含 watcher fan-out）：
//   - 每个 session 在自己的 yamux stream 上回写事件；任意数量的额外 stream
//     可以通过 agent.resume attach 到同一 session 拿到 live 事件。
//   - session 的所有 event 同时落 SQLite，便于断线重连时按 last_acked_seq 重放。
//   - session 的生命周期与 backend 子进程绑定，而非任何一条 WS 连接。
//     浏览器断 WS 不杀 agent；新 WS 走 resume 拿历史 + tail 新事件。
type AgentManager struct {
	log   *slog.Logger
	store *store.Store

	mu       sync.Mutex
	sessions map[string]*agentSession
}

type agentSession struct {
	id        string
	backend   agent.Backend
	sess      *agent.Session
	createdAt time.Time

	// seq 给每条 emit 的事件分配单调递增编号，并同步落 SQLite。
	seq atomic.Int64

	cancel context.CancelFunc

	// done 在 consumeEvents 退出时关闭，用于 pump 检测 session 是否终止。
	doneOnce sync.Once
	done     chan struct{}

	// finished 在 consumeEvents 退出前置位，配合 mu 保护 addWatcher 不挂到已死 session。
	finished atomic.Bool

	// watcher fan-out：每个 attach 的 stream 一个 buffered channel，
	// emit 写入；session 结束时全部关闭，pump 据此退出。
	watchersMu    sync.Mutex
	watchers      map[int64]chan proto.AgentEvent
	nextWatcherID int64
}

// NewAgentManager 构造空 Manager。
func NewAgentManager(log *slog.Logger, st *store.Store) *AgentManager {
	return &AgentManager{log: log, store: st, sessions: map[string]*agentSession{}}
}

// HandleStart 实现 agent.start：spawn backend、回写 sessionId、随后在 stream 上
// 单向发送事件帧。
//
// 关键：agent 子进程跑在独立 goroutine 里（consumeEvents），与 stream 解耦。
// stream 关闭（WS 断开）只让 pump 退出，不杀 agent；事件继续落 SQLite，
// 新 WS 可以通过 agent.resume 继续 tail。
//
// 多轮对话（req.ContinueSessionID 非空）：复用已有 session id，seq 从 DB 的
// last_seq 续编号，--resume 参数从 DB 的 provider_session_id 自动推导。sidebar
// 只看到一条 session，timeline 看到全部 turn。
func (m *AgentManager) HandleStart(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.AgentStartReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	if req.Prompt == "" {
		return writeAgentError(stream, fmt.Errorf("prompt is empty"))
	}

	sessID := req.ContinueSessionID
	var startSeq int64
	var providerFromDB string
	isContinue := sessID != ""
	if isContinue {
		if m.store == nil {
			return writeAgentError(stream, fmt.Errorf("store not configured; continue unsupported"))
		}
		exists, _, lastSeq, err := m.store.SessionInfo(context.Background(), sessID)
		if err != nil {
			return writeAgentError(stream, fmt.Errorf("continue session info: %w", err))
		}
		if !exists {
			return writeAgentError(stream, fmt.Errorf("continue session not found: %s", sessID))
		}
		startSeq = lastSeq
		providerFromDB, _ = m.store.GetProviderSessionID(context.Background(), sessID)
	} else {
		sessID = newSessionID()
	}

	backend, err := agent.New(req.Backend, agent.Config{
		Logger: m.log,
		Env:    req.Env,
	})
	if err != nil {
		return writeAgentError(stream, err)
	}

	// --resume 参数：显式 ResumeFrom > DB 推导。多轮对话默认走 DB。
	resumeFrom := req.ResumeFrom
	if resumeFrom == "" {
		resumeFrom = providerFromDB
	}

	runCtx, cancel := context.WithCancel(context.Background())
	sess, err := backend.Execute(runCtx, req.Prompt, agent.ExecOptions{
		Cwd:            req.Cwd,
		Model:          req.Model,
		PermissionMode: req.PermissionMode,
		ResumeFrom:     resumeFrom,
	})
	if err != nil {
		cancel()
		_ = backend.Close()
		return writeAgentError(stream, fmt.Errorf("agent execute: %w", err))
	}

	entry := &agentSession{
		id:        sessID,
		backend:   backend,
		sess:      sess,
		createdAt: time.Now(),
		cancel:    cancel,
		done:      make(chan struct{}),
		watchers:  map[int64]chan proto.AgentEvent{},
	}
	entry.seq.Store(startSeq)

	m.mu.Lock()
	if old, live := m.sessions[sessID]; live {
		// 复用 sessID 的场景：denied 权限点"允许"触发 startSession 重试时，
		// 旧 Claude 可能 (a) 已退出（finished=true）但 consumeEvents 还没释放，
		// 或 (b) 仍在跑（finished=false，卡在 denied 等用户回复，永远等不到）。
		// 两种情况都要让新 entry 接管：把旧的从 map 摘掉（owner 校验防误杀
		// 后续新 entry），cancel + close 杀掉旧 Claude 进程。
		// 旧 consumeEvents 后续 release(id, old) 因 owner 校验会 no-op。
		m.log.Info("agent session takeover", "id", sessID, "old_finished", old.finished.Load())
		delete(m.sessions, sessID)
		if old.cancel != nil {
			old.cancel()
		}
		_ = old.backend.Close()
	}
	m.sessions[sessID] = entry
	m.mu.Unlock()

	if isContinue {
		m.log.Info("agent session continued", "id", sessID, "backend", req.Backend,
			"start_seq", startSeq, "resume_from", resumeFrom)
		if m.store != nil {
			_ = m.store.MarkSessionRunning(context.Background(), sessID)
		}
	} else {
		m.log.Info("agent session started", "id", sessID, "backend", req.Backend, "cwd", req.Cwd)
		if m.store != nil {
			_ = m.store.CreateSession(context.Background(), store.Session{
				ID:        sessID,
				Backend:   req.Backend,
				Cwd:       req.Cwd,
				CreatedAt: entry.createdAt,
				Title:     titleFromPrompt(req.Prompt),
			})
		}
	}

	if err := WriteJSONRes(stream, proto.AgentStartRes{SessionID: sessID}); err != nil {
		m.release(sessID, entry)
		return err
	}

	// 顺序很关键：
	//   1. 同步注册 watcher（addWatcher 拿锁、塞 channel）
	//   2. emit 开场事件（session.created / user.prompt / 续轮 status=running）
	//      → broadcast 看到 watcher，事件安全入 channel
	//   3. 起 consumeEvents goroutine 处理 backend 事件流
	//   4. 阻塞 pump channel → stream，直到 stream 关闭或 session 结束
	// 顺序反了的话开场事件会被 broadcast 默默丢弃（watchers map 空），UI 永远
	// 看不到用户的 prompt 文本。
	wid, ch := entry.addWatcher(256)
	if ch != nil {
		defer entry.removeWatcher(wid)
	}

	if !isContinue {
		m.emit(entry, proto.AgentEvent{
			Type:      proto.EvSessionCreated,
			SessionID: sessID,
			Ts:        time.Now().UnixMilli(),
		})
	}
	m.emit(entry, proto.AgentEvent{
		Type:      proto.EvUserPrompt,
		SessionID: sessID,
		Ts:        time.Now().UnixMilli(),
		Text:      req.Prompt,
	})
	if isContinue {
		m.emit(entry, proto.AgentEvent{
			Type:      proto.EvSessionStatus,
			SessionID: sessID,
			Ts:        time.Now().UnixMilli(),
			Status:    "running",
		})
	}

	go m.consumeEvents(entry, sess)

	// 阻塞转发直到 channel 关闭（session 结束）或 stream 写失败（浏览器断开）。
	if ch != nil {
		for ev := range ch {
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if err := tunnelmux.WriteFrame(stream, payload); err != nil {
				break
			}
		}
	}
	return nil
}

// titleFromPrompt 把首条 prompt 转成 sidebar 用的 title：压扁换行、限长 60 字符。
// 空 prompt 返回空（ListSessions 兜底用 backend+cwd+time）。
func titleFromPrompt(p string) string {
	if p == "" {
		return ""
	}
	// 压扁所有空白（含换行）成单空格。
	squeezed := strings.Builder{}
	prevSpace := false
	for _, r := range p {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' {
			if !prevSpace {
				squeezed.WriteRune(' ')
				prevSpace = true
			}
			continue
		}
		squeezed.WriteRune(r)
		prevSpace = false
	}
	out := strings.TrimSpace(squeezed.String())
	const max = 60
	// 按 rune 截断，避免中文截半。
	runes := []rune(out)
	if len(runes) > max {
		out = string(runes[:max]) + "…"
	}
	return out
}

// emit 给所有 watcher 写一帧 AgentEvent；seq 自增并落库。
//
// 不直接写任何具体 stream —— pump 协程负责把 watcher ch 的内容转成 stream 帧。
func (m *AgentManager) emit(entry *agentSession, ev proto.AgentEvent) {
	ev.Seq = entry.seq.Add(1)
	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMilli()
	}
	if m.store != nil {
		_ = m.store.AppendEvent(context.Background(), entry.id, ev)
		// session.status 携带 provider session id 时，同步写入 sessions 行，
		// 让 ListSessions 能把 provider_session_id 暴露给浏览器续对话用。
		if ev.Type == proto.EvSessionStatus && ev.Status == "running" && ev.ProviderSessionID != "" {
			_ = m.store.UpdateProviderSessionID(context.Background(), entry.id, ev.ProviderSessionID)
		}
	}
	entry.broadcast(ev)
}

func (entry *agentSession) broadcast(ev proto.AgentEvent) {
	entry.watchersMu.Lock()
	defer entry.watchersMu.Unlock()
	for _, ch := range entry.watchers {
		select {
		case ch <- ev:
		default:
			// 慢 watcher（buffer 满）→ 丢一条。前端 resume 会按 last_seq 兜底重放。
		}
	}
}

// addWatcher 注册一个新 watcher。返回 (id, ch)；session 已结束时 ch 为 nil。
func (entry *agentSession) addWatcher(buf int) (int64, chan proto.AgentEvent) {
	if entry.finished.Load() {
		return 0, nil
	}
	entry.watchersMu.Lock()
	defer entry.watchersMu.Unlock()
	if entry.finished.Load() { // double-check under lock
		return 0, nil
	}
	id := entry.nextWatcherID
	entry.nextWatcherID++
	ch := make(chan proto.AgentEvent, buf)
	entry.watchers[id] = ch
	return id, ch
}

// removeWatcher 删除并返回 channel（不 close —— 调用方负责处理 drain / close）。
func (entry *agentSession) removeWatcher(id int64) chan proto.AgentEvent {
	entry.watchersMu.Lock()
	defer entry.watchersMu.Unlock()
	ch, ok := entry.watchers[id]
	if !ok {
		return nil
	}
	delete(entry.watchers, id)
	return ch
}

// closeAllWatchers 删除并关闭所有 watcher channel。session 结束时调用，
// 让所有 pump 立即感知到 "没有更多事件" 并退出。
func (entry *agentSession) closeAllWatchers() {
	entry.watchersMu.Lock()
	defer entry.watchersMu.Unlock()
	for id, ch := range entry.watchers {
		delete(entry.watchers, id)
		close(ch)
	}
}

// consumeEvents 阻塞转发 backend 事件直到 Events 关闭，然后做收尾：
//   - 发 session.idle（让前端看到终止信号）
//   - 标记 finished、关闭所有 watcher
//   - release 释放 backend 子进程
func (m *AgentManager) consumeEvents(entry *agentSession, sess *agent.Session) {
	defer entry.doneOnce.Do(func() { close(entry.done) })

	gotResult := false
	for {
		select {
		case ev, ok := <-sess.Events:
			if !ok {
				// 自然结束：发 idle，然后收尾。
				m.emit(entry, proto.AgentEvent{
					Type:      proto.EvSessionIdle,
					SessionID: entry.id,
					Ts:        time.Now().UnixMilli(),
				})
				entry.finished.Store(true)
				entry.closeAllWatchers()
				m.release(entry.id, entry)
				return
			}
			m.emit(entry, translateEvent(ev, entry.id))
		case res, ok := <-sess.Result:
			if !ok || gotResult {
				continue
			}
			gotResult = true
			m.emit(entry, proto.AgentEvent{
				Type:      proto.EvSessionStatus,
				SessionID: entry.id,
				Ts:        time.Now().UnixMilli(),
				Status:    res.Status,
				Error:     res.Error,
			})
			if m.store != nil {
				_ = m.store.FinishSession(context.Background(), entry.id, res.Status, time.Now())
			}
			// Result 是单发，但 Events 还会继续收到（直到 claude exit）；
			// 继续等 Events。
		}
	}
}

// pumpToStream / attachPump 已内联进 HandleStart —— emit 顺序与 watcher 注册
// 必须紧挨着，拆出去容易出现"emit 时 watcher 还没挂上"的时序 bug。

// HandleResume 实现 agent.resume：

// HandleResume 实现 agent.resume：
//  1. 从 store 重放历史事件（seq > last_seq）
//  2. 如果 session 还活，attach 为 watcher 继续接收新事件
//
// UI 端已经按 seq 去重，replay / live 之间偶尔重叠的事件会被自动丢弃，
// 所以这里不需要做服务端去重。
func (m *AgentManager) HandleResume(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.AgentResumeReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	if req.SessionID == "" {
		return writeAgentError(stream, fmt.Errorf("session_id is empty"))
	}
	if m.store == nil {
		return writeAgentError(stream, fmt.Errorf("client store not configured; resume unsupported"))
	}

	ctx := context.Background()
	exists, ended, lastSeq, err := m.store.SessionInfo(ctx, req.SessionID)
	if err != nil {
		return writeAgentError(stream, fmt.Errorf("session info: %w", err))
	}
	if !exists {
		return writeAgentError(stream, fmt.Errorf("session not found: %s", req.SessionID))
	}

	m.mu.Lock()
	entry, live := m.sessions[req.SessionID]
	m.mu.Unlock()

	// 回 AgentResumeRes，告诉 server 我们要重放多少条 / session 是否还活。
	res := proto.AgentResumeRes{
		SessionID: req.SessionID,
		Live:      live,
		Done:      ended,
	}
	if err := WriteJSONRes(stream, res); err != nil {
		return err
	}

	// 重放历史。
	count := 0
	since := req.LastSeq
	if since < 0 {
		since = 0
	}
	err = m.store.ReplayEvents(ctx, req.SessionID, since, func(ev proto.AgentEvent) error {
		payload, _ := json.Marshal(ev)
		if err := tunnelmux.WriteFrame(stream, payload); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		m.log.Warn("agent resume replay stopped", "session", req.SessionID, "err", err, "replayed", count)
		return err
	}

	// session 仍活：attach watcher 继续 tail 新事件。
	if live && !ended && entry != nil {
		wid, ch := entry.addWatcher(256)
		if ch == nil {
			// session 在我们 replay 期间刚好结束。再做一次 replay 兜底收尾事件。
			_ = m.store.ReplayEvents(ctx, req.SessionID, lastSeq, func(ev proto.AgentEvent) error {
				payload, _ := json.Marshal(ev)
				_ = tunnelmux.WriteFrame(stream, payload)
				return nil
			})
			m.log.Info("agent resume replay-only (session finished during resume)",
				"session", req.SessionID, "replayed", count)
			return nil
		}
		defer entry.removeWatcher(wid)

		for ev := range ch {
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if err := tunnelmux.WriteFrame(stream, payload); err != nil {
				return nil
			}
		}
	}

	// session 已结束（store 里 ended_at 非空，且当前不在跑）：合成一个
	// session.idle 事件发到 stream，但不落库（不增 seq）。两个目的：
	//   1. server pumpAgentEventsToWS 看到 terminated → 用 NormalClosure
	//      (1000) 关 WS，而不是 StatusInternalError (1011)。
	//   2. 浏览器 onmessage 走 EvSessionIdle 分支 → sessionState.done=true、
	//      setBusy(false)。
	// 不发这个事件的话，浏览器 onclose 收到 1011，sessionState.done 还是
	// openSession 里设的 false，触发自动 resumeSession()，无限循环。
	if ended {
		status := "completed"
		if st, err := m.store.SessionStatus(ctx, req.SessionID); err == nil && st != "" {
			status = st
		}
		ev := proto.AgentEvent{
			Type:      proto.EvSessionIdle,
			Ts:        time.Now().UnixMilli(),
			SessionID: req.SessionID,
			Status:    status,
		}
		if payload, err := json.Marshal(ev); err == nil {
			_ = tunnelmux.WriteFrame(stream, payload)
		}
	}

	m.log.Info("agent resume done", "session", req.SessionID,
		"replayed", count, "live", live, "done", ended, "last_seq", lastSeq)
	return nil
}

// HandleList 列出客户端持久化的 session（最近 N 条），叠加 live 状态。
func (m *AgentManager) HandleList(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.AgentListReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	if m.store == nil {
		return writeAgentError(stream, fmt.Errorf("client store not configured"))
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}

	sessions, err := m.store.ListSessions(context.Background(), limit, req.Cwd)
	if err != nil {
		return writeAgentError(stream, fmt.Errorf("list sessions: %w", err))
	}

	// 用 live session map 标注哪些仍在跑。
	m.mu.Lock()
	liveSet := make(map[string]bool, len(m.sessions))
	for id := range m.sessions {
		liveSet[id] = true
	}
	m.mu.Unlock()

	out := proto.AgentListRes{}
	for _, s := range sessions {
		info := proto.AgentSessionInfo{
			ID:                s.ID,
			Backend:           s.Backend,
			Cwd:               s.Cwd,
			CreatedAt:         s.CreatedAt.UnixMilli(),
			LastSeq:           s.LastSeq,
			Status:            s.Status,
			Live:              liveSet[s.ID],
			ProviderSessionID: s.ProviderSessionID,
			Title:             s.Title,
		}
		if !s.EndedAt.IsZero() {
			info.EndedAt = s.EndedAt.UnixMilli()
		}
		out.Sessions = append(out.Sessions, info)
	}
	return WriteJSONRes(stream, out)
}

// HandleInterrupt 实现 agent.interrupt 短 RPC。
func (m *AgentManager) HandleInterrupt(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.AgentInterruptReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	m.mu.Lock()
	entry, ok := m.sessions[req.SessionID]
	m.mu.Unlock()
	if !ok {
		return writeAgentError(stream, fmt.Errorf("session not found: %s", req.SessionID))
	}
	if err := entry.backend.Interrupt(context.Background()); err != nil {
		return writeAgentError(stream, err)
	}
	return WriteJSONRes(stream, struct{}{})
}

// HandlePermissionReply 实现 agent.permission.reply 短 RPC。
func (m *AgentManager) HandlePermissionReply(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.AgentPermissionReplyReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	m.mu.Lock()
	entry, ok := m.sessions[req.SessionID]
	m.mu.Unlock()
	if !ok {
		return writeAgentError(stream, fmt.Errorf("session not found: %s", req.SessionID))
	}
	if err := entry.backend.RespondPermission(context.Background(), req.RequestID, req.Decision, req.UpdatedInput); err != nil {
		return writeAgentError(stream, err)
	}
	return WriteJSONRes(stream, struct{}{})
}

// release 释放一个 session 的资源（map 摘除 + cancel + backend close）。
// entry 必须是 caller 持有的指针 —— 复用 sessID 的新 session 会把 map 里的
// entry 换成新的；老的 consumeEvents 跑到 release 时如果还用 id 查 map 就会
// 误杀新 entry。传指针做 owner 校验，只有 map 里仍是这个 entry 才摘。
func (m *AgentManager) release(id string, entry *agentSession) {
	m.mu.Lock()
	cur, ok := m.sessions[id]
	if ok && cur == entry {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if entry.cancel != nil {
		entry.cancel()
	}
	_ = entry.backend.Close()
	m.log.Info("agent session released", "id", id)
}

// ── 翻译：agent.Event → proto.AgentEvent ──

func translateEvent(ev agent.Event, sessID string) proto.AgentEvent {
	out := proto.AgentEvent{
		SessionID: sessID,
		Ts:        time.Now().UnixMilli(),
	}
	switch ev.Type {
	case agent.EventText:
		out.Type = proto.EvTextDelta
		out.Text = ev.Content
	case agent.EventThinking:
		out.Type = proto.EvReasoningDelta
		out.Reason = ev.Content
	case agent.EventToolUse:
		out.Type = proto.EvToolCalled
		out.Tool = ev.Tool
		out.CallID = ev.CallID
		if ev.Input != nil {
			out.Input, _ = json.Marshal(ev.Input)
		}
	case agent.EventToolResult:
		out.Type = proto.EvToolSuccess
		out.CallID = ev.CallID
		out.Output = ev.Output
	case agent.EventStatus:
		out.Type = proto.EvSessionStatus
		out.Status = ev.Status
		// Claude backend 把 provider session id 放在 Event.Content（status=running）。
		// 提到外层 ProviderSessionID 字段，方便浏览器续对话时回传。
		if ev.Status == "running" && ev.Content != "" {
			out.ProviderSessionID = ev.Content
		}
	case agent.EventError:
		out.Type = proto.EvSessionError
		out.Error = ev.Content
	case agent.EventPermission:
		out.Type = proto.EvPermissionAsked
		out.Permission = &proto.AgentPermissionAsked{
			RequestID: ev.RequestID,
			Action:    ev.Tool,
			Resources: ev.Resources,
		}
	case agent.EventLog:
		out.Type = "log"
	}
	return out
}

func newSessionID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(int(sessionCounter.Add(1)))
}

var sessionCounter atomic.Int64

func writeAgentError(stream net.Conn, err error) error {
	_ = WriteJSONRes(stream, proto.AgentError{Error: err.Error()})
	return err
}

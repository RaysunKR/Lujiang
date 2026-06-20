package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// codexBackend 通过 `codex app-server --listen stdio://` 调起 Codex CLI。
// 协议：JSON-RPC 2.0 over stdio。我们做：
//   - initialize / initialized 握手
//   - thread/start 开新对话线程
//   - turn/start 发 prompt
//   - 流式接收 responses 里的事件，翻译为 Lujiang Event
//
// 简化点（vs multica）：不实现 semantic-inactivity timeout / MCP config 管理 /
// 版本探测 / 自定义 args guard。这些是 multica 多租户场景才需要的特性。
//
// 协议参考：multica-main/server/pkg/agent/codex.go。
type codexBackend struct {
	cfg Config
}

func newCodexBackend(cfg Config) *codexBackend {
	return &codexBackend{cfg: cfg}
}

func (b *codexBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath, err := resolveExecPath(b.cfg, "codex")
	if err != nil {
		return nil, fmt.Errorf("codex executable not found: %w", err)
	}

	runCtx, cancel := runContext(ctx, 0)
	args := []string{"app-server", "--listen", "stdio://"}

	cmd, stdout, stderrT, err := spawnCLI(spawnArgs{
		ctx:       runCtx,
		execPath:  execPath,
		args:      args,
		cwd:       opts.Cwd,
		env:       buildEnv(b.cfg.Env),
		waitDelay: 10 * time.Second,
	})
	if err != nil {
		cancel()
		return nil, err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codex stdin pipe: %w", err)
	}

	b.cfg.Logger.Info("codex started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	eventCh := make(chan Event, 256)
	resultCh := make(chan Result, 1)

	go func() {
		defer close(eventCh)
		defer close(resultCh)
		defer cancel()

		// ctx 取消时关 stdout/stdin，让 scanner / codex 退出。
		go func() {
			<-runCtx.Done()
			_ = stdout.Close()
			closeOnce(stdin)
		}()

		c := &codexConn{
			stdin:   stdin,
			writeMu: &sync.Mutex{},
			nextID:  1,
			pending: map[int]chan json.RawMessage{},
		}
		// codexReader 把所有 incoming JSON-RPC 消息分发：response 给 pending
		// request，notification 给 eventCh 翻译。
		notifs := make(chan json.RawMessage, 256)
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			defer close(notifs)
			codexReader(stdout, c, notifs, eventCh)
		}()

		startTime := time.Now()
		var output strings.Builder
		var sessionID string
		finalStatus := "completed"
		var finalError string

		// 1. initialize
		initResp, err := c.request(runCtx, "initialize", map[string]any{
			"clientInfo": map[string]any{
				"name":    "lujiang",
				"title":   "Lujiang",
				"version": "0.1.0",
			},
			"capabilities": map[string]any{
				"experimentalApi": true,
			},
		})
		if err != nil {
			finalStatus = "failed"
			finalError = withStderrTail(fmt.Sprintf("codex initialize: %v", err), "codex", stderrT.Tail())
			resultCh <- Result{Status: finalStatus, Error: finalError, DurationMs: time.Since(startTime).Milliseconds()}
			return
		}
		_ = initResp
		c.notify("initialized", nil)
		trySend(eventCh, Event{Type: EventStatus, Status: "running"})

		// 2. thread/start
		threadResp, err := c.request(runCtx, "thread/start", map[string]any{})
		if err != nil {
			finalStatus = "failed"
			finalError = withStderrTail(fmt.Sprintf("codex thread/start: %v", err), "codex", stderrT.Tail())
			resultCh <- Result{Status: finalStatus, Error: finalError, DurationMs: time.Since(startTime).Milliseconds()}
			return
		}
		var ts struct {
			ThreadID string `json:"threadID"`
			Session  struct {
				ID string `json:"id"`
			} `json:"session"`
		}
		_ = json.Unmarshal(threadResp, &ts)
		if ts.Session.ID != "" {
			sessionID = ts.Session.ID
		} else {
			sessionID = ts.ThreadID
		}

		// 3. turn/start + 等 turnDone
		turnDone := make(chan struct{})
		var turnErr error
		go func() {
			defer close(turnDone)
			_, err := c.request(runCtx, "turn/start", map[string]any{
				"threadId": ts.ThreadID,
				"input":    []map[string]any{{"type": "text", "text": prompt}},
			})
			if err != nil {
				turnErr = err
			}
		}()

		// 4. 读 notification 流到事件；turnDone 后退出。
		var lastTurnErr string
	readLoop:
		for {
			select {
			case raw, ok := <-notifs:
				if !ok {
					break readLoop
				}
				var n codexNotification
				if json.Unmarshal(raw, &n) != nil {
					continue
				}
				switch n.Method {
				case "responses/output_text_delta":
					if p := codexParseOutputTextDelta(n.Params); p != "" {
						output.WriteString(p)
						trySend(eventCh, Event{Type: EventText, Content: p})
					}
				case "responses/reasoning_text_delta":
					if p := codexParseReasoningDelta(n.Params); p != "" {
						trySend(eventCh, Event{Type: EventThinking, Content: p})
					}
				case "responses/message":
					text, sessID := codexParseMessage(n.Params)
					if sessID != "" {
						sessionID = sessID
					}
					if text != "" {
						output.WriteString(text)
						trySend(eventCh, Event{Type: EventText, Content: text})
					}
				case "responses/function_call":
					name, callID, args := codexParseFunctionCall(n.Params)
					if name != "" {
						trySend(eventCh, Event{
							Type:   EventToolUse,
							Tool:   name,
							CallID:  callID,
							Input:  args,
						})
					}
				case "responses/function_call_output":
					callID, out := codexParseFunctionCallOutput(n.Params)
					trySend(eventCh, Event{
						Type:   EventToolResult,
						CallID:  callID,
						Output: out,
					})
				case "responses/completed":
					// 一次性 turn 完成。break 由 turnDone 配合 ctx cancel 处理。
				case "responses/failed":
					lastTurnErr = codexParseError(n.Params)
					trySend(eventCh, Event{Type: EventError, Content: lastTurnErr})
				}
			case <-turnDone:
				if turnErr != nil {
					lastTurnErr = turnErr.Error()
				}
				break readLoop
			case <-runCtx.Done():
				break readLoop
			}
		}

		// 5. cleanup
		closeOnce(stdin)
		<-readerDone
		exitErr := cmd.Wait()
		duration := time.Since(startTime)

		switch {
		case runCtx.Err() == context.Canceled:
			finalStatus = "aborted"
			finalError = "execution cancelled"
		case lastTurnErr != "":
			finalStatus = "failed"
			finalError = lastTurnErr
		case exitErr != nil && finalStatus == "completed":
			finalStatus = "failed"
			finalError = fmt.Sprintf("codex exited: %v", exitErr)
		}
		if finalError != "" {
			finalError = withStderrTail(finalError, "codex", stderrT.Tail())
		}

		b.cfg.Logger.Info("codex finished", "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

		resultCh <- Result{
			Status:     finalStatus,
			Output:     output.String(),
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			SessionID:  sessionID,
		}
	}()

	return &Session{
		Events:  eventCh,
		Result:  resultCh,
		cancel:  cancel,
		backend: b,
	}, nil
}

func (b *codexBackend) RespondPermission(ctx context.Context, reqID, decision string, updatedInput map[string]any) error {
	// Codex 用 sandbox/approval policy 在 CLI 启动时一次性配置；无运行时 seam。
	return fmt.Errorf("codex: permission seam not supported")
}

func (b *codexBackend) Interrupt(ctx context.Context) error { return nil }
func (b *codexBackend) Close() error                       { return nil }

// ── JSON-RPC ──

type codexConn struct {
	stdin   io.WriteCloser
	writeMu *sync.Mutex
	nextID  int64
	pending map[int]chan json.RawMessage
	pendMu  sync.Mutex
}

func (c *codexConn) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := int(atomic.AddInt64(&c.nextID, 1))
	ch := make(chan json.RawMessage, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
	}()

	var paramsField any = params
	if params == nil {
		paramsField = struct{}{}
	}
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  paramsField,
	}
	if err := c.write(msg); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		// resp 是 raw response。检查 error 字段。
		var probe struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(resp, &probe)
		if probe.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, probe.Error.Message)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *codexConn) notify(method string, params any) {
	var paramsField any = params
	if params == nil {
		paramsField = struct{}{}
	}
	_ = c.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  paramsField,
	})
}

func (c *codexConn) write(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(data)
	return err
}

// codexReader 读 stdout NDJSON：response 分发给 pending[id]，notification 进 notifs。
func codexReader(stdout io.Reader, c *codexConn, notifs chan<- json.RawMessage, eventCh chan<- Event) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var probe struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
		}
		if json.Unmarshal([]byte(line), &probe) != nil {
			continue
		}
		if probe.ID != nil && probe.Method == "" {
			// response
			c.pendMu.Lock()
			ch, ok := c.pending[*probe.ID]
			c.pendMu.Unlock()
			if ok {
				select {
				case ch <- json.RawMessage(line):
				default:
				}
			}
		} else if probe.Method != "" {
			// notification
			select {
			case notifs <- json.RawMessage(line):
			default:
				// 满 → 丢；codex 输出快时丢 notification 不致命。
			}
		}
	}
}

type codexNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// codex notification 参数解析助手。所有 params 形如 {"response":{...}}
// （codex 把单个 response item 包在 response key 下）。
func codexUnwrapResponse(p json.RawMessage) json.RawMessage {
	var w struct {
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(p, &w) == nil && len(w.Response) > 0 {
		return w.Response
	}
	return p
}

func codexParseOutputTextDelta(p json.RawMessage) string {
	r := codexUnwrapResponse(p)
	var v struct {
		Delta string `json:"delta"`
	}
	_ = json.Unmarshal(r, &v)
	return v.Delta
}

func codexParseReasoningDelta(p json.RawMessage) string {
	r := codexUnwrapResponse(p)
	var v struct {
		Delta string `json:"delta"`
	}
	_ = json.Unmarshal(r, &v)
	return v.Delta
}

func codexParseMessage(p json.RawMessage) (text, sessionID string) {
	r := codexUnwrapResponse(p)
	var v struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(r, &v)
	if v.SessionID != "" {
		sessionID = v.SessionID
	}
	var sb strings.Builder
	for _, c := range v.Content {
		if c.Type == "output_text" || c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	text = sb.String()
	return
}

func codexParseFunctionCall(p json.RawMessage) (name, callID string, args map[string]any) {
	r := codexUnwrapResponse(p)
	var v struct {
		Type    string          `json:"type"`
		Name    string          `json:"name"`
		CallID  string          `json:"callId"`
		Args    json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(r, &v)
	name = v.Name
	callID = v.CallID
	if v.Args != nil {
		_ = json.Unmarshal(v.Args, &args)
	}
	return
}

func codexParseFunctionCallOutput(p json.RawMessage) (callID, output string) {
	r := codexUnwrapResponse(p)
	var v struct {
		Type   string `json:"type"`
		CallID string `json:"callId"`
		Output string `json:"output"`
	}
	_ = json.Unmarshal(r, &v)
	return v.CallID, v.Output
}

func codexParseError(p json.RawMessage) string {
	r := codexUnwrapResponse(p)
	var v struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r, &v)
	if v.Error.Message != "" {
		return v.Error.Message
	}
	return "codex turn failed"
}

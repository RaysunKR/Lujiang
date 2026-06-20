package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/server/tunnel"
	"github.com/lujiang/lujiang/internal/tunnelmux"
)

// sessionWSHandler 把浏览器的 WebSocket 桥接到客户端的 agent.start stream。
//
// URL：GET /api/session/{clientID}/ws?backend=...&cwd=...&prompt=...&model=...&permission_mode=...
//
// 浏览器侧用 POST 风格不行（WS 没有 body），所以参数走 query string。
// 交互流程：
//   - WS 建立后，server 在客户端隧道上开一条 stream 发 agent.start；
//     读 AgentStartRes 拿到 sessionID。
//   - client 在该 stream 上单向发 AgentEvent 帧；server 原样作为 WS 文本消息转发。
//   - 浏览器发控制消息（{"type":"interrupt"} / {"type":"permission.reply",...}）
//     时，server 在另一条短 stream 上发对应 RPC。
//   - session 结束（client 关闭 stream）→ 关闭 WS。
func sessionWSHandler(reg *tunnel.Registry, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := parseSessionPath(r.URL.Path)
		if !ok {
			writeError(w, http.StatusBadRequest, "expected /api/session/{clientID}/ws")
			return
		}
		cc, ok := reg.Lookup(clientID)
		if !ok {
			writeError(w, http.StatusNotFound, "client not online")
			return
		}

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")

		q := r.URL.Query()

		// Resume 模式：query 带 session_id + last_seq → 在客户端开 agent.resume stream，
		// 把 SQLite 里 seq > last_seq 的事件全部回放给浏览器。
		if sid := q.Get("session_id"); sid != "" {
			lastSeq, _ := parseSeq(q.Get("last_seq"))
			stream, res, err := openAgentResume(cc, proto.AgentResumeReq{SessionID: sid, LastSeq: lastSeq})
			if err != nil {
				log.Warn("agent resume", "err", err, "client", clientID)
				_ = c.Close(websocket.StatusInternalError, "agent resume failed")
				return
			}
			defer stream.Close()
			log.Info("session ws resumed", "client", clientID, "session", sid, "replayed", res.Replayed, "live", res.Live, "done", res.Done)

			ctx := r.Context()
			go pumpAgentEventsToWS(ctx, c, stream, sid, log)
			bridgeWSToAgentControl(ctx, c, cc, sid, log)
			return
		}

		req := proto.AgentStartReq{
			Backend:           firstNonEmpty(q.Get("backend"), "claude"),
			Cwd:               q.Get("cwd"),
			Prompt:            q.Get("prompt"),
			Model:             q.Get("model"),
			PermissionMode:    q.Get("permission_mode"),
			ResumeFrom:        q.Get("resume_from"),
			ContinueSessionID: q.Get("continue_session_id"),
		}

		stream, sessionID, err := openAgentStart(cc, req)
		if err != nil {
			log.Warn("agent start", "err", err, "client", clientID)
			_ = c.Close(websocket.StatusInternalError, "agent start failed")
			return
		}
		defer stream.Close()
		log.Info("session ws attached", "client", clientID, "session", sessionID)

		ctx := r.Context()

		go pumpAgentEventsToWS(ctx, c, stream, sessionID, log)

		bridgeWSToAgentControl(ctx, c, cc, sessionID, log)
	}
}

func openAgentStart(cc *tunnel.ClientConn, req proto.AgentStartReq) (net.Conn, string, error) {
	s, err := cc.OpenStream()
	if err != nil {
		return nil, "", fmt.Errorf("open stream: %w", err)
	}
	hdr, _ := proto.StreamHeader{Op: proto.OpAgentStart}.MarshalLine()
	if _, err := s.Write(hdr); err != nil {
		_ = s.Close()
		return nil, "", fmt.Errorf("write header: %w", err)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		_ = s.Close()
		return nil, "", err
	}
	if err := tunnelmux.WriteFrame(s, payload); err != nil {
		_ = s.Close()
		return nil, "", fmt.Errorf("write request: %w", err)
	}
	respPayload, err := tunnelmux.ReadFrame(s)
	if err != nil {
		_ = s.Close()
		return nil, "", fmt.Errorf("read response: %w", err)
	}
	var perr proto.AgentError
	if json.Unmarshal(respPayload, &perr) == nil && perr.Error != "" {
		_ = s.Close()
		return nil, "", fmt.Errorf("agent.start: %s", perr.Error)
	}
	var res proto.AgentStartRes
	if err := json.Unmarshal(respPayload, &res); err != nil {
		_ = s.Close()
		return nil, "", fmt.Errorf("decode start res: %w", err)
	}
	return s, res.SessionID, nil
}

func pumpAgentEventsToWS(ctx context.Context, c *websocket.Conn, stream net.Conn, sessionID string, log *slog.Logger) {
	// 跟踪是否见过 session.idle / session.error —— 见过意味着 agent 自然结束，
	// 之后 stream EOF 是预期；没见过就 EOF = 客户端掉线。
	sawTerminated := false
	for {
		payload, err := tunnelmux.ReadFrame(stream)
		if err != nil {
			closeCode := websocket.StatusInternalError
			closeMsg := "client stream lost"
			if sawTerminated {
				closeCode = websocket.StatusNormalClosure
				closeMsg = "session ended"
			}
			log.Info("session stream closed", "session", sessionID, "err", err, "ws_close", closeCode, "saw_terminated", sawTerminated)
			_ = c.Close(closeCode, closeMsg)
			return
		}
		// 嗅探事件类型：session.idle / session.error 表示 agent 走到了终态。
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &probe) == nil {
			if probe.Type == proto.EvSessionIdle || probe.Type == proto.EvSessionError {
				sawTerminated = true
			}
		}
		if err := c.Write(ctx, websocket.MessageText, payload); err != nil {
			return
		}
	}
}

// bridgeWSToAgentControl 处理浏览器发的控制消息。
// P5 支持 {"type":"interrupt"}；P6 会加 permission.reply。
func bridgeWSToAgentControl(ctx context.Context, c *websocket.Conn, cc *tunnel.ClientConn, sessionID string, log *slog.Logger) {
	for {
		mt, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if mt != websocket.MessageText {
			continue
		}
		var ctrl struct {
			Type         string                 `json:"type"`
			RequestID    string                 `json:"request_id"`
			Decision     string                 `json:"decision"`
			UpdatedInput map[string]interface{} `json:"updated_input"`
		}
		if json.Unmarshal(data, &ctrl) != nil {
			continue
		}
		switch ctrl.Type {
		case "interrupt":
			if err := sendAgentInterrupt(cc, sessionID); err != nil {
				log.Debug("agent interrupt", "err", err, "session", sessionID)
			}
		case "permission.reply":
			if err := sendAgentPermissionReply(cc, sessionID, ctrl.RequestID, ctrl.Decision, ctrl.UpdatedInput); err != nil {
				log.Debug("agent permission reply", "err", err, "session", sessionID)
			}
		}
	}
}

func sendAgentInterrupt(cc *tunnel.ClientConn, sessionID string) error {
	s, err := cc.OpenStream()
	if err != nil {
		return err
	}
	defer s.Close()
	hdr, _ := proto.StreamHeader{Op: proto.OpAgentInterrupt}.MarshalLine()
	if _, err := s.Write(hdr); err != nil {
		return err
	}
	payload, _ := json.Marshal(proto.AgentInterruptReq{SessionID: sessionID})
	if err := tunnelmux.WriteFrame(s, payload); err != nil {
		return err
	}
	_, _ = tunnelmux.ReadFrame(s)
	return nil
}

func sendAgentPermissionReply(cc *tunnel.ClientConn, sessionID, reqID, decision string, updated map[string]any) error {
	s, err := cc.OpenStream()
	if err != nil {
		return err
	}
	defer s.Close()
	hdr, _ := proto.StreamHeader{Op: proto.OpAgentPermissionReply}.MarshalLine()
	if _, err := s.Write(hdr); err != nil {
		return err
	}
	payload, _ := json.Marshal(proto.AgentPermissionReplyReq{
		SessionID:    sessionID,
		RequestID:    reqID,
		Decision:     decision,
		UpdatedInput: updated,
	})
	if err := tunnelmux.WriteFrame(s, payload); err != nil {
		return err
	}
	_, _ = tunnelmux.ReadFrame(s)
	return nil
}

// sessionRouter 按 path 末端分发：以 /ws 结尾 → WS handler；否则视为 list。
func sessionRouter(reg *tunnel.Registry, log *slog.Logger) http.Handler {
	ws := http.HandlerFunc(sessionWSHandler(reg, log))
	list := http.HandlerFunc(sessionListHandler(reg, log))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 简化判断：path 末段是不是 "ws"。
		trimmed := strings.TrimRight(r.URL.Path, "/")
		if strings.HasSuffix(trimmed, "/ws") {
			ws.ServeHTTP(w, r)
			return
		}
		// GET /api/session/{clientID} 才允许；其它方法 / 形态走 WS handler（它会拒绝）。
		if r.Method == http.MethodGet {
			list.ServeHTTP(w, r)
			return
		}
		ws.ServeHTTP(w, r)
	})
}

func parseSessionPath(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/api/session/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "ws" {
		return "", false
	}
	return parts[0], true
}

// parseSessionListPath 处理 GET /api/session/{clientID}（不带 /ws）。
func parseSessionListPath(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/api/session/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// sessionListHandler 处理 GET /api/session/{clientID}：列出该客户端持久化的 session。
func sessionListHandler(reg *tunnel.Registry, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := parseSessionListPath(r.URL.Path)
		if !ok {
			writeError(w, http.StatusBadRequest, "expected /api/session/{clientID}")
			return
		}
		cc, ok := reg.Lookup(clientID)
		if !ok {
			writeError(w, http.StatusNotFound, "client not online")
			return
		}

		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconvAtoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}

		s, err := openAgentList(cc, proto.AgentListReq{Limit: limit, Cwd: r.URL.Query().Get("cwd")})
		if err != nil {
			log.Warn("agent list", "err", err, "client", clientID)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
	}
}

func openAgentList(cc *tunnel.ClientConn, req proto.AgentListReq) (proto.AgentListRes, error) {
	s, err := cc.OpenStream()
	if err != nil {
		return proto.AgentListRes{}, fmt.Errorf("open stream: %w", err)
	}
	defer s.Close()
	hdr, _ := proto.StreamHeader{Op: proto.OpAgentList}.MarshalLine()
	if _, err := s.Write(hdr); err != nil {
		return proto.AgentListRes{}, fmt.Errorf("write header: %w", err)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return proto.AgentListRes{}, err
	}
	if err := tunnelmux.WriteFrame(s, payload); err != nil {
		return proto.AgentListRes{}, fmt.Errorf("write request: %w", err)
	}
	respPayload, err := tunnelmux.ReadFrame(s)
	if err != nil {
		return proto.AgentListRes{}, fmt.Errorf("read response: %w", err)
	}
	var perr proto.AgentError
	if json.Unmarshal(respPayload, &perr) == nil && perr.Error != "" {
		return proto.AgentListRes{}, fmt.Errorf("agent.list: %s", perr.Error)
	}
	var res proto.AgentListRes
	if err := json.Unmarshal(respPayload, &res); err != nil {
		return proto.AgentListRes{}, fmt.Errorf("decode list res: %w", err)
	}
	return res, nil
}

// strconvAtoi 是 strconv.Atoi 的薄包装，便于上层不直接 import strconv。
func strconvAtoi(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseSeq(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// openAgentResume 在客户端隧道上开 agent.resume stream，写 req、读 AgentResumeRes。
// 该 stream 之后由 client 单向写历史事件帧。
func openAgentResume(cc *tunnel.ClientConn, req proto.AgentResumeReq) (net.Conn, proto.AgentResumeRes, error) {
	s, err := cc.OpenStream()
	if err != nil {
		return nil, proto.AgentResumeRes{}, fmt.Errorf("open stream: %w", err)
	}
	hdr, _ := proto.StreamHeader{Op: proto.OpAgentResume}.MarshalLine()
	if _, err := s.Write(hdr); err != nil {
		_ = s.Close()
		return nil, proto.AgentResumeRes{}, fmt.Errorf("write header: %w", err)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		_ = s.Close()
		return nil, proto.AgentResumeRes{}, err
	}
	if err := tunnelmux.WriteFrame(s, payload); err != nil {
		_ = s.Close()
		return nil, proto.AgentResumeRes{}, fmt.Errorf("write request: %w", err)
	}
	respPayload, err := tunnelmux.ReadFrame(s)
	if err != nil {
		_ = s.Close()
		return nil, proto.AgentResumeRes{}, fmt.Errorf("read response: %w", err)
	}
	var perr proto.AgentError
	if json.Unmarshal(respPayload, &perr) == nil && perr.Error != "" {
		_ = s.Close()
		return nil, proto.AgentResumeRes{}, fmt.Errorf("agent.resume: %s", perr.Error)
	}
	var res proto.AgentResumeRes
	if err := json.Unmarshal(respPayload, &res); err != nil {
		_ = s.Close()
		return nil, proto.AgentResumeRes{}, fmt.Errorf("decode resume res: %w", err)
	}
	return s, res, nil
}

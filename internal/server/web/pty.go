package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/server/tunnel"
	"github.com/lujiang/lujiang/internal/tunnelmux"
)

// ptyWSHandler 把浏览器的 WebSocket 桥接到客户端的 pty.create stream。
//
// URL：GET /api/pty/{clientID}/ws?shell=...&cwd=...&cols=...&rows=...
//
// 协议（browser ↔ server）：
//   - 连接建立即触发 server 在客户端隧道上开一条 yamux stream，发 pty.create。
//   - 浏览器发的二进制消息原样 frame-and-forward 到 client。
//   - 浏览器发的文本消息是控制 JSON，目前支持 {"type":"resize","cols":N,"rows":N}
//     和 {"type":"close"}，server 在另一条短 stream 上转发。
//   - client 发回来的帧（原始字节）原样作为二进制消息回写 WS。
func ptyWSHandler(reg *tunnel.Registry, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := parsePTYPath(r.URL.Path)
		if !ok {
			writeError(w, http.StatusBadRequest, "expected /api/pty/{clientID}/ws")
			return
		}
		cc, ok := reg.Lookup(clientID)
		if !ok {
			writeError(w, http.StatusNotFound, "client not online")
			return
		}

		// 同源 + cookie 已经够；InsecureSkipVerify 让 dev 模式下从其他端口也能连。
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")

		q := r.URL.Query()
		cols, _ := strconv.Atoi(q.Get("cols"))
		rows, _ := strconv.Atoi(q.Get("rows"))
		req := proto.PTYCreateReq{
			Shell: q.Get("shell"),
			Cwd:   q.Get("cwd"),
			Cols:  cols,
			Rows:  rows,
		}

		stream, ptyID, err := openPTYCreate(cc, req)
		if err != nil {
			log.Warn("pty create", "err", err, "client", clientID)
			_ = c.Close(websocket.StatusInternalError, "pty create failed")
			return
		}
		defer stream.Close()
		log.Info("pty ws attached", "client", clientID, "pty", ptyID)

		ctx := r.Context()

		// client → browser。
		go pumpPTYToWS(ctx, c, stream, ptyID, log)

		// browser → client。Read 阻塞，断开时退出。
		bridgeWSToPTY(ctx, c, cc, ptyID, stream, log)
	}
}

func openPTYCreate(cc *tunnel.ClientConn, req proto.PTYCreateReq) (net.Conn, string, error) {
	s, err := cc.OpenStream()
	if err != nil {
		return nil, "", fmt.Errorf("open stream: %w", err)
	}
	hdrLine, _ := proto.StreamHeader{Op: proto.OpPTYCreate}.MarshalLine()
	if _, err := s.Write(hdrLine); err != nil {
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
	var perr proto.PTYError
	if json.Unmarshal(respPayload, &perr) == nil && perr.Error != "" {
		_ = s.Close()
		return nil, "", fmt.Errorf("pty.create: %s", perr.Error)
	}
	var res proto.PTYCreateRes
	if err := json.Unmarshal(respPayload, &res); err != nil {
		_ = s.Close()
		return nil, "", fmt.Errorf("decode create res: %w", err)
	}
	return s, res.PtyID, nil
}

// pumpPTYToWS 把 client 端 pty 输出原样转发为 WS 二进制消息。
// stream 关闭意味着 client 端 pty 进程结束了 → 主动关闭 WS。
func pumpPTYToWS(ctx context.Context, c *websocket.Conn, stream net.Conn, ptyID string, log *slog.Logger) {
	for {
		payload, err := tunnelmux.ReadFrame(stream)
		if err != nil {
			log.Info("pty stream closed", "pty", ptyID, "err", err)
			_ = c.Close(websocket.StatusNormalClosure, "pty exited")
			return
		}
		if err := c.Write(ctx, websocket.MessageBinary, payload); err != nil {
			return
		}
	}
}

// bridgeWSToPTY 读浏览器发的消息。二进制 = 数据，文本 = 控制 JSON。
// 退出时调用 pty.close 短 RPC（best-effort）。
func bridgeWSToPTY(ctx context.Context, c *websocket.Conn, cc *tunnel.ClientConn, ptyID string, stream net.Conn, log *slog.Logger) {
	var closeOnce sync.Once
	defer closeOnce.Do(func() {
		if err := sendPTYClose(cc, ptyID); err != nil {
			log.Debug("pty close", "err", err, "pty", ptyID)
		}
	})

	for {
		mt, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		switch mt {
		case websocket.MessageBinary:
			if err := tunnelmux.WriteFrame(stream, data); err != nil {
				return
			}
		case websocket.MessageText:
			var ctrl struct {
				Type string `json:"type"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
			}
			if json.Unmarshal(data, &ctrl) != nil {
				continue
			}
			switch ctrl.Type {
			case "resize":
				if err := sendPTYResize(cc, ptyID, ctrl.Cols, ctrl.Rows); err != nil {
					log.Debug("pty resize", "err", err, "pty", ptyID)
				}
			case "close":
				return
			}
		}
	}
}

func sendPTYResize(cc *tunnel.ClientConn, ptyID string, cols, rows int) error {
	s, err := cc.OpenStream()
	if err != nil {
		return err
	}
	defer s.Close()
	hdr, _ := proto.StreamHeader{Op: proto.OpPTYResize}.MarshalLine()
	if _, err := s.Write(hdr); err != nil {
		return err
	}
	payload, _ := json.Marshal(proto.PTYResizeReq{PtyID: ptyID, Cols: cols, Rows: rows})
	if err := tunnelmux.WriteFrame(s, payload); err != nil {
		return err
	}
	if _, err := tunnelmux.ReadFrame(s); err != nil {
		return err
	}
	return nil
}

func sendPTYClose(cc *tunnel.ClientConn, ptyID string) error {
	s, err := cc.OpenStream()
	if err != nil {
		return err
	}
	defer s.Close()
	hdr, _ := proto.StreamHeader{Op: proto.OpPTYClose}.MarshalLine()
	if _, err := s.Write(hdr); err != nil {
		return err
	}
	payload, _ := json.Marshal(proto.PTYCloseReq{PtyID: ptyID})
	if err := tunnelmux.WriteFrame(s, payload); err != nil {
		return err
	}
	if _, err := tunnelmux.ReadFrame(s); err != nil {
		return err
	}
	return nil
}

// parsePTYPath 解析 "/api/pty/{clientID}/ws"。
func parsePTYPath(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "/api/pty/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "ws" {
		return "", false
	}
	return parts[0], true
}

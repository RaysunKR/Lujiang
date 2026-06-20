package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/tunnelmux"
)

// Handler 处理客户端的反向 WSS 接入。
type Handler struct {
	Log      *slog.Logger
	Registry *Registry
	// IDForToken 校验 bearer token 并返回 clientID。
	IDForToken func(token string) (string, bool)
}

// ServeHTTP 实现 http.Handler。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	clientID, ok := h.IDForToken(token)
	if !ok {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		h.Log.Debug("ws accept failed", "err", err, "client", clientID)
		return
	}
	// 用一个独立 context 托管 NetConn 生命周期；HTTP handler 返回时通过 cancel 触发关闭。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	netConn := websocket.NetConn(ctx, c, websocket.MessageBinary)

	sess, err := tunnelmux.NewServerSession(netConn)
	if err != nil {
		h.Log.Error("yamux server init", "err", err, "client", clientID)
		c.Close(websocket.StatusInternalError, "yamux init failed")
		return
	}

	cc := &ClientConn{ID: clientID, Meta: proto.ClientMeta{ID: clientID}, Sess: sess}

	// 等待 register stream 拿到完整 ClientMeta。
	metaCh := make(chan proto.ClientMeta, 1)
	errCh := make(chan error, 1)
	go func() {
		m, err := readRegister(cc)
		if err != nil {
			errCh <- err
			return
		}
		metaCh <- m
	}()

	select {
	case m := <-metaCh:
		cc.Meta = m
	case err := <-errCh:
		h.Log.Warn("register stream failed", "err", err, "client", clientID)
	case <-time.After(15 * time.Second):
		h.Log.Warn("register stream timeout", "client", clientID)
	}

	h.Registry.Register(cc)
	h.Log.Info("client registered",
		"client", clientID,
		"hostname", cc.Meta.Hostname,
		"os", cc.Meta.OS,
		"arch", cc.Meta.Arch,
		"shells", cc.Meta.Shells,
	)
	defer func() {
		h.Registry.Unregister(clientID)
		h.Log.Info("client unregistered", "client", clientID)
	}()

	// 阻塞直到 yamux session 结束。
	<-sess.CloseChan()
}

func readRegister(cc *ClientConn) (proto.ClientMeta, error) {
	stream, err := cc.AcceptStream()
	if err != nil {
		return proto.ClientMeta{}, err
	}
	defer stream.Close()

	hdr, _, err := proto.ReadHeaderLine(stream)
	if err != nil {
		return proto.ClientMeta{}, err
	}
	if hdr.Op != proto.OpRegister {
		return proto.ClientMeta{}, errors.New("expected register op, got " + hdr.Op)
	}
	payload, err := tunnelmux.ReadFrame(stream)
	if err != nil {
		return proto.ClientMeta{}, err
	}
	var meta proto.ClientMeta
	if err := json.Unmarshal(payload, &meta); err != nil {
		return proto.ClientMeta{}, err
	}
	meta.ID = cc.ID
	// 回 ack。
	if _, err := stream.Write([]byte(`{"op":"registered"}` + "\n")); err != nil {
		return proto.ClientMeta{}, err
	}
	return meta, nil
}

func bearerToken(s string) string {
	const pfxBearer = "Bearer "
	const pfxBearerLower = "bearer "
	if len(s) >= len(pfxBearer) && s[:len(pfxBearer)] == pfxBearer {
		return s[len(pfxBearer):]
	}
	if len(s) >= len(pfxBearerLower) && s[:len(pfxBearerLower)] == pfxBearerLower {
		return s[len(pfxBearerLower):]
	}
	return ""
}

// Package dial 反向 dial 服务端、建立 yamux 会话、注册元数据，
// 并在断线时按指数退避重连。
package dial

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/lujiang/lujiang/internal/client/shell"
	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/tunnelmux"
)

// Config 是 dial 包需要的配置子集。
type Config struct {
	ID            string
	Token         string
	ServerURL     string
	Hostname      string // 留空则自动取 os.Hostname
	TLSSkipVerify bool   // 自签证书场景跳过校验
}

// Dialer 负责维持到服务端的长连接。
type Dialer struct {
	Log    *slog.Logger
	Cfg    Config
	// OnReady 在每次成功 register 后调用（用于注册 stream handler 等）。
	OnReady func(*yamux.Session) error
}

// Run 阻塞地维护到服务端的连接，断线自动重连，直到 ctx 取消。
func (d *Dialer) Run(ctx context.Context) error {
	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := d.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.Log.Warn("tunnel disconnected; retrying", "err", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (d *Dialer) runOnce(ctx context.Context) error {
	// 每次连接独立的子 context，避免外层 ctx 取消时没关闭 net.Conn。
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 接入时也带 token；通过自定义 header 绕过浏览器 Origin 校验。
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+d.Cfg.Token)

	d.Log.Info("dialing server", "url", d.Cfg.ServerURL)
	opts := &websocket.DialOptions{
		HTTPHeader: hdr,
	}
	if d.Cfg.TLSSkipVerify {
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}
	c, _, err := websocket.Dial(connCtx, d.Cfg.ServerURL, opts)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "client shutting down")

	netConn := websocket.NetConn(connCtx, c, websocket.MessageBinary)
	sess, err := tunnelmux.NewClientSession(netConn)
	if err != nil {
		return fmt.Errorf("yamux: %w", err)
	}
	defer sess.Close()

	if err := d.register(sess); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	d.Log.Info("registered with server", "client", d.Cfg.ID)

	// 把 session 交给上层注册 handler。
	if d.OnReady != nil {
		if err := d.OnReady(sess); err != nil {
			return fmt.Errorf("on_ready: %w", err)
		}
	}

	// 阻塞直到 session 关闭。
	<-sess.CloseChan()
	return nil
}

func (d *Dialer) register(sess *yamux.Session) error {
	stream, err := sess.Open()
	if err != nil {
		return err
	}
	defer stream.Close()

	hdr, err := proto.StreamHeader{Op: proto.OpRegister}.MarshalLine()
	if err != nil {
		return err
	}
	if _, err := stream.Write(hdr); err != nil {
		return err
	}

	meta := proto.ClientMeta{
		ID:       d.Cfg.ID,
		Hostname: d.Cfg.Hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Shells:   shell.Available(),
	}
	if meta.Hostname == "" {
		if h, err := os.Hostname(); err == nil {
			meta.Hostname = h
		}
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := tunnelmux.WriteFrame(stream, payload); err != nil {
		return err
	}

	// 等 ack。
	ackHdr, _, err := proto.ReadHeaderLine(stream)
	if err != nil {
		return err
	}
	if ackHdr.Op != proto.OpRegistered {
		return fmt.Errorf("unexpected ack op: %s", ackHdr.Op)
	}
	return nil
}

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)

// Package handlers 在客户端侧接收来自服务端（最终来自浏览器）的 yamux stream，
// 按 StreamHeader.Op 分发到具体 handler。
package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"

	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/tunnelmux"
)

// Handler 处理一条 yamux stream。Header 已被读取，handler 拥有 stream 的剩余生命周期。
type Handler func(stream net.Conn, hdr proto.StreamHeader) error

// Registry 把 op 字符串路由到 Handler。
type Registry struct {
	log      *slog.Logger
	handlers map[string]Handler
}

func NewRegistry(log *slog.Logger) *Registry {
	return &Registry{log: log, handlers: map[string]Handler{}}
}

func (r *Registry) Register(op string, h Handler) {
	r.handlers[op] = h
}

// Accepter 抽象 yamux.Session 的 Accept 能力。
type Accepter interface {
	Accept() (net.Conn, error)
}

// Serve 阻塞接收 stream 并分发；session 结束时返回。
func (r *Registry) Serve(sess Accepter) error {
	for {
		stream, err := sess.Accept()
		if err != nil {
			return err
		}
		go r.handle(stream)
	}
}

func (r *Registry) handle(stream net.Conn) {
	defer stream.Close()
	hdr, _, err := proto.ReadHeaderLine(stream)
	if err != nil {
		r.log.Debug("read header", "err", err)
		return
	}
	h, ok := r.handlers[hdr.Op]
	if !ok {
		_ = WriteJSONRes(stream, proto.FSError{Error: fmt.Sprintf("unknown op: %s", hdr.Op)})
		return
	}
	if err := h(stream, hdr); err != nil {
		r.log.Debug("handler error", "op", hdr.Op, "err", err)
	}
}

// ReadJSONReq 读一帧 JSON 请求体。
func ReadJSONReq(stream net.Conn, v any) error {
	payload, err := tunnelmux.ReadFrame(stream)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}

// WriteJSONRes 写一帧 JSON 响应体。
func WriteJSONRes(stream net.Conn, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tunnelmux.WriteFrame(stream, payload)
}

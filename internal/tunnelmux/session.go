package tunnelmux

import (
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

// keepAliveInterval 是 yamux 心跳间隔。30s 通常足以在大多数 NAT/反向代理
// 的 idle timeout（60-300s）之前发出活动。
const keepAliveInterval = 30 * time.Second

// NewServerSession 在已建立的 net.Conn 上创建 yamux 服务端会话。
func NewServerSession(c net.Conn) (*yamux.Session, error) {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = keepAliveInterval
	s, err := yamux.Server(c, cfg)
	if err != nil {
		return nil, fmt.Errorf("tunnelmux: yamux server: %w", err)
	}
	return s, nil
}

// NewClientSession 在已建立的 net.Conn 上创建 yamux 客户端会话。
func NewClientSession(c net.Conn) (*yamux.Session, error) {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = keepAliveInterval
	s, err := yamux.Client(c, cfg)
	if err != nil {
		return nil, fmt.Errorf("tunnelmux: yamux client: %w", err)
	}
	return s, nil
}

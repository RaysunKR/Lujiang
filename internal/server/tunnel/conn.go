package tunnel

import (
	"net"
	"sync"

	"github.com/hashicorp/yamux"
	"github.com/lujiang/lujiang/internal/proto"
)

// ClientConn 是服务端视角下的一台在线客户端。
type ClientConn struct {
	ID   string
	Meta proto.ClientMeta
	Sess *yamux.Session

	mu     sync.Mutex
	closed bool
}

// OpenStream 在隧道上打开一条新的 yamux stream，供浏览器 RPC 透传。
func (c *ClientConn) OpenStream() (net.Conn, error) {
	return c.Sess.Open()
}

// AcceptStream 由服务端从客户端接收一条 stream（注册 stream 等）。
func (c *ClientConn) AcceptStream() (net.Conn, error) {
	return c.Sess.Accept()
}

// Close 关闭整个客户端连接。
func (c *ClientConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.Sess.Close()
}

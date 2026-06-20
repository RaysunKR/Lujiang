// Package tunnel 维护在线客户端的内存注册表，并把 WSS 升级 + yamux 包装起来。
package tunnel

import "sync"

// Registry 是 clientID -> *ClientConn 的并发安全内存表。
// 服务端"无业务状态"指无持久化业务状态；注册表本身只在内存。
type Registry struct {
	mu      sync.RWMutex
	clients map[string]*ClientConn
}

func NewRegistry() *Registry {
	return &Registry{clients: make(map[string]*ClientConn)}
}

func (r *Registry) Register(c *ClientConn) {
	r.mu.Lock()
	r.clients[c.ID] = c
	r.mu.Unlock()
}

func (r *Registry) Lookup(id string) (*ClientConn, bool) {
	r.mu.RLock()
	c, ok := r.clients[id]
	r.mu.RUnlock()
	return c, ok
}

func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	delete(r.clients, id)
	r.mu.Unlock()
}

// Snapshot 返回当前所有在线客户端的副本。
func (r *Registry) Snapshot() []*ClientConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ClientConn, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, c)
	}
	return out
}

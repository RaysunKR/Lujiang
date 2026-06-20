package web

import (
	"encoding/json"
	"net/http"

	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/server/tunnel"
)

// handleClients 返回当前在线客户端列表。已由 RequireAuth 保护。
func handleClients(reg *tunnel.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		clients := reg.Snapshot()
		out := make([]proto.ClientMeta, 0, len(clients))
		for _, c := range clients {
			out = append(out, c.Meta)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

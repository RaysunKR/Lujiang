package main

import (
	"encoding/json"
	"net/http"

	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/server/tunnel"
)

// writeClientsJSON 输出当前在线客户端的元数据列表。P1 用于调试，
// P2 会被迁到 internal/server/web/clients.go 并加鉴权。
func writeClientsJSON(w http.ResponseWriter, reg *tunnel.Registry) {
	clients := reg.Snapshot()
	out := make([]proto.ClientMeta, 0, len(clients))
	for _, c := range clients {
		out = append(out, c.Meta)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

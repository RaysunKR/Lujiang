// Package web 提供浏览器面向的 HTTP/WS 路由。P2 覆盖鉴权 + 静态资源 + 客户端列表；
// P3+ 在此追加 fs/pty/session 路由。
package web

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/lujiang/lujiang/internal/server/auth"
	"github.com/lujiang/lujiang/internal/server/tunnel"
)

// Mux 装配所有浏览器路由并返回 *http.ServeMux。
func Mux(authn *auth.WebAuth, registry *tunnel.Registry, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// 公共路由：登录、健康。
	mux.HandleFunc("/api/login", handleLogin(authn))
	mux.HandleFunc("/api/logout", handleLogout(authn))
	mux.HandleFunc("/api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// 受保护 API：客户端列表等。
	mux.Handle("/api/me", authn.RequireAuth(http.HandlerFunc(handleMe(authn))))
	mux.Handle("/api/clients", authn.RequireAuth(http.HandlerFunc(handleClients(registry))))
	// 文件服务（P3）：/api/fs/{clientID}/{op}
	mux.Handle("/api/fs/", authn.RequireAuth(fsRouter(registry)))
	// PTY WebSocket（P4）：/api/pty/{clientID}/ws
	mux.Handle("/api/pty/", authn.RequireAuth(http.HandlerFunc(ptyWSHandler(registry, log))))
	// Agent session WebSocket（P5）：/api/session/{clientID}/ws
	// Agent session 列表（P8）：GET /api/session/{clientID}
	mux.Handle("/api/session/", authn.RequireAuth(sessionRouter(registry, log)))

	// 静态资源（embed.FS）。SPA fallback：非 /api 路径全部回到 index.html。
	assets := spaHandler{fs: distFS()}
	mux.Handle("/", assets)

	return stripTrailingSlash(mux)
}

// stripTrailingSlash 是个简单的 middleware 兜底（不影响 /）。
func stripTrailingSlash(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) > 1 && strings.HasSuffix(r.URL.Path, "/") {
			r.URL.Path = strings.TrimRight(r.URL.Path, "/")
		}
		h.ServeHTTP(w, r)
	})
}

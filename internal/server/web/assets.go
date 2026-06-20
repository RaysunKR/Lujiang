package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// distFS 返回 web/dist 子树作为 fs.FS。
// web/dist/index.html 占位文件确保 embed 在 dist 未构建时也能编译通过。
//
//go:embed dist
var embeddedDist embed.FS

func distFS() fs.FS {
	sub, err := fs.Sub(embeddedDist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// spaHandler 服务静态资源；找不到的路径回退到 index.html（SPA 路由）。
type spaHandler struct {
	fs fs.FS
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	f, err := h.fs.Open(path)
	if err != nil {
		// 兜底到 index.html。
		f2, err2 := h.fs.Open("index.html")
		if err2 != nil {
			http.NotFound(w, r)
			return
		}
		defer f2.Close()
		serveContent(w, r, "index.html", f2)
		return
	}
	defer f.Close()
	serveContent(w, r, path, f)
}

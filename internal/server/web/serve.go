package web

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

// serveContent 是 http.ServeFileFS 的最小实现，避免依赖 Go 1.22+ 的 ServeFileFS。
func serveContent(w http.ResponseWriter, r *http.Request, name string, f fs.File) {
	stat, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if stat.IsDir() {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ct := mime.TypeByExtension(ext); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.Copy(w, f)
}

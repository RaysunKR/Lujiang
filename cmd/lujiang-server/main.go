// lujiang-server 启动服务端：加载 config、暴露 /api/tunnel 给客户端接入，
// 暴露 /api/* 给浏览器（带 bcrypt 登录 + HMAC session cookie），embed.FS 内嵌前端。
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lujiang/lujiang/internal/proto"
	tlspkg "github.com/lujiang/lujiang/internal/server/tls"
	"github.com/lujiang/lujiang/internal/server/auth"
	"github.com/lujiang/lujiang/internal/server/config"
	"github.com/lujiang/lujiang/internal/server/tunnel"
	"github.com/lujiang/lujiang/internal/server/web"
)

func main() {
	cfgPath := flag.String("config", "configs/server.yaml", "path to server config")
	dev := flag.Bool("dev", false, "dev mode: HTTP only, no Secure cookie")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config load failed", "err", err)
		os.Exit(2)
	}

	secret := []byte(cfg.SessionSecret)
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			log.Error("generate session secret", "err", err)
			os.Exit(2)
		}
		log.Warn("session_secret not configured; generated random secret (existing cookies will invalidate on restart)")
	}
	users := make(map[string]string, len(cfg.WebUsers))
	for _, u := range cfg.WebUsers {
		users[u.Username] = u.PasswordHash
	}

	registry := tunnel.NewRegistry()
	tunnelH := &tunnel.Handler{
		Log:        log,
		Registry:   registry,
		IDForToken: cfg.IDForToken,
	}

	authn := auth.NewWebAuth(secret, !*dev, users)

	// 顶层 mux：把 tunnel + ping 走专用路径，其余全交给 web.Mux。
	mux := http.NewServeMux()
	mux.Handle("/api/tunnel", tunnelH)
	mux.HandleFunc("/api/ping/", func(w http.ResponseWriter, r *http.Request) {
		handlePing(w, r, registry, log)
	})
	mux.Handle("/", web.Mux(authn, registry, log))

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	useTLS := !*dev
	go func() {
		if !useTLS {
			log.Info("server listening (HTTP, dev mode)", "addr", cfg.Listen)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("listen failed", "err", err)
				os.Exit(1)
			}
			return
		}
		cert, key, err := tlspkg.LoadOrSelfSign(cfg.CertFile, cfg.KeyFile, cfg.AutoTLS)
		if err != nil {
			log.Error("tls setup failed", "err", err)
			os.Exit(1)
		}
		certPair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			log.Error("tls key pair", "err", err)
			os.Exit(1)
		}
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{certPair},
			MinVersion:   tls.VersionTLS12,
		}
		if cfg.CertFile == "" && cfg.AutoTLS {
			log.Info("using self-signed TLS cert; browser will warn until you trust it")
		}
		log.Info("server listening (HTTPS)", "addr", cfg.Listen)
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// handlePing 通过客户端隧道开一条 stream 写 ping，等 pong 回来（P1 调试端点）。
func handlePing(w http.ResponseWriter, r *http.Request, reg *tunnel.Registry, log *slog.Logger) {
	clientID := strings.TrimPrefix(r.URL.Path, "/api/ping/")
	if clientID == "" {
		http.Error(w, "client id required", http.StatusBadRequest)
		return
	}
	cc, ok := reg.Lookup(clientID)
	if !ok {
		http.Error(w, "client not online", http.StatusNotFound)
		return
	}
	stream, err := cc.OpenStream()
	if err != nil {
		log.Error("open ping stream", "err", err, "client", clientID)
		http.Error(w, "open stream failed", http.StatusInternalServerError)
		return
	}
	defer stream.Close()
	if err := stream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ping, err := proto.StreamHeader{Op: proto.OpPing}.MarshalLine()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := stream.Write(ping); err != nil {
		log.Error("write ping", "err", err, "client", clientID)
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	hdr, _, err := proto.ReadHeaderLine(stream)
	if err != nil {
		log.Error("read pong", "err", err, "client", clientID)
		http.Error(w, "read pong failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if hdr.Op != proto.OpPong {
		http.Error(w, "expected pong, got "+hdr.Op, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"client":"` + clientID + `","status":"ok"}`))
}

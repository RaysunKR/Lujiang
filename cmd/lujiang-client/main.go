// lujiang-client 启动客户端：加载 config、反向 dial 服务端、注册元数据，
// 并在每次隧道建立后启动 stream handler 分发器。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hashicorp/yamux"
	"github.com/lujiang/lujiang/internal/client/config"
	"github.com/lujiang/lujiang/internal/client/dial"
	"github.com/lujiang/lujiang/internal/client/handlers"
	"github.com/lujiang/lujiang/internal/client/store"
	"github.com/lujiang/lujiang/internal/proto"
)

func main() {
	cfgPath := flag.String("config", "configs/client.yaml", "path to client config")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config load failed", "err", err)
		os.Exit(2)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "client.db.d"), log)
	if err != nil {
		log.Error("store open failed", "err", err)
		os.Exit(2)
	}
	defer st.Close()

	reg := handlers.NewRegistry(log)
	reg.Register(proto.OpPing, handlers.HandlePing)
	reg.Register(proto.OpFSList, handlers.HandleList)
	reg.Register(proto.OpFSRead, handlers.HandleRead)
	reg.Register(proto.OpFSWrite, handlers.HandleWrite)
	reg.Register(proto.OpFSStat, handlers.HandleStat)
	reg.Register(proto.OpFSMkdir, handlers.HandleMkdir)
	reg.Register(proto.OpFSRemove, handlers.HandleRemove)
	reg.Register(proto.OpFSMove, handlers.HandleMove)

	ptyMgr := handlers.NewPTYManager(log)
	reg.Register(proto.OpPTYCreate, ptyMgr.HandleCreate)
	reg.Register(proto.OpPTYResize, ptyMgr.HandleResize)
	reg.Register(proto.OpPTYClose, ptyMgr.HandleClose)

	agentMgr := handlers.NewAgentManager(log, st)
	reg.Register(proto.OpAgentStart, agentMgr.HandleStart)
	reg.Register(proto.OpAgentResume, agentMgr.HandleResume)
	reg.Register(proto.OpAgentInterrupt, agentMgr.HandleInterrupt)
	reg.Register(proto.OpAgentPermissionReply, agentMgr.HandlePermissionReply)
	reg.Register(proto.OpAgentList, agentMgr.HandleList)

	d := &dial.Dialer{
		Log: log,
		Cfg: dial.Config{
			ID:            cfg.ID,
			Token:         cfg.Token,
			ServerURL:     cfg.ServerURL,
			TLSSkipVerify: cfg.TLSSkipVerify,
		},
		OnReady: func(sess *yamux.Session) error {
			log.Info("tunnel ready; serving handlers", "client", cfg.ID)
			go func() {
				if err := reg.Serve(sess); err != nil {
					log.Warn("handler registry ended", "err", err)
				}
			}()
			return nil
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := d.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("dialer exited", "err", err)
		os.Exit(1)
	}
}

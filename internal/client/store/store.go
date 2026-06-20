// Package store 包装 modernc.org/sqlite，提供 client 端的 session/message 持久化。
//
// 设计：
//   - 单 writer goroutine：所有写串行化，避免 SQLite "database is locked"。
//   - WAL 模式 + busy_timeout=5000，让多读单写更顺滑。
//   - 给定 dataDir（通常是 $HOME/.lujiang/），建 lujiang.db。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store 是 client 的持久化层。
type Store struct {
	db  *sql.DB
	log *slog.Logger

	writeCh chan writeOp
	done    chan struct{}
}

type writeOp struct {
	fn   func(tx *sql.Tx) error
	resp chan error
}

// Open 打开/创建 dataDir 下的 lujiang.db，启动 writer goroutine。
func Open(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := mkdirAll(dataDir); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	dsn := filepath.Join(dataDir, "lujiang.db")
	// _pragma 是 modernc.org/sqlite 的连接级 pragma 注入语法。
	dsnURL := "file:" + dsn + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsnURL)
	if err != nil {
		return nil, fmt.Errorf("store: open db: %w", err)
	}
	db.SetMaxOpenConns(8)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	// 启动时清理：上一次进程退出时还在 running 的 session 现在已是孤儿
	// （subprocess 随 client 一起死了），标记成 interrupted 让 sidebar
	// 显示与实际状态一致。
	if err := sweepStaleRunning(db); err != nil {
		log.Warn("store: sweep stale running sessions", "err", err)
	}

	s := &Store{
		db:      db,
		log:     log,
		writeCh: make(chan writeOp, 64),
		done:    make(chan struct{}),
	}
	go s.writeLoop()
	return s, nil
}

func (s *Store) writeLoop() {
	defer close(s.done)
	for op := range s.writeCh {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			op.resp <- err
			continue
		}
		if err := op.fn(tx); err != nil {
			_ = tx.Rollback()
			op.resp <- err
			continue
		}
		op.resp <- tx.Commit()
	}
}

// Write 把一个 tx-scoped 操作排进 writer 队列，阻塞等结果。
func (s *Store) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	op := writeOp{fn: fn, resp: make(chan error, 1)}
	select {
	case s.writeCh <- op:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-op.resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Query 在调用方 goroutine 跑只读查询。
func (s *Store) Query(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Close 关闭 db 与 writer。
func (s *Store) Close() error {
	close(s.writeCh)
	<-s.done
	return s.db.Close()
}

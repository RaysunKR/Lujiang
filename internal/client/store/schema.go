package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// schema_v1 是初次部署的 schema。后续变更走 migrations（按 version 顺序跑）。
const schemaV1 = `
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    backend     TEXT NOT NULL,
    cwd         TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    status      TEXT,
    last_seq    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS events (
    session_id  TEXT NOT NULL,
    seq         INTEGER NOT NULL,
    ts          INTEGER NOT NULL,
    type        TEXT NOT NULL,
    payload     TEXT NOT NULL,
    PRIMARY KEY (session_id, seq),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS events_session_seq ON events(session_id, seq);
`

// migration 是单步版本迁移：从 fromV 升到 fromV+1。
// 每个 migration 必须幂等（重复跑不报错），且自包含（不依赖事务外的状态）。
type migration struct {
	fromV int
	stmts []string
}

// migrations 按 fromV 升序排列。新加 migration 追加到尾部即可。
//
// 当前 schema 版本 = 3。
var migrations = []migration{
	{
		fromV: 1,
		stmts: []string{
			// ProviderSessionID：backend 自己的 session id（如 Claude 的 session-xxx）。
			// 用于多轮对话续接（resume_from）。
			"ALTER TABLE sessions ADD COLUMN provider_session_id TEXT",
		},
	},
	{
		fromV: 2,
		stmts: []string{
			// Title：会话标题，取首条 user prompt（截断）。空表示老 session 迁移
			// 过来还没有 title，sidebar 兜底显示 backend+cwd+time。
			"ALTER TABLE sessions ADD COLUMN title TEXT",
		},
	},
}

// schemaMeta 表存当前 schema 版本（仅一行 user_version=1 风格）。
// 不直接用 PRAGMA user_version 是因为 modernc.org/sqlite 在某些连接模式下
// PRAGMA 写入会被 reset；用普通表更稳。
const ensureMetaTable = `
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);
`

// migrate 跑所有迁移到最新版本。流程：
//  1. 建表（首次部署）→ schema_meta 写 version=1
//  2. 已有库 → 读 version，按顺序跑 migrations 升级
//
// 整个 migrate 在单连接里跑（Open 时 db 还没启用 writer goroutine），
// 所以不会和别的写竞争。
func migrate(db *sql.DB) error {
	if _, err := db.Exec(schemaV1); err != nil {
		return fmt.Errorf("apply v1 schema: %w", err)
	}
	if _, err := db.Exec(ensureMetaTable); err != nil {
		return fmt.Errorf("create schema_meta: %w", err)
	}

	cur, err := readVersion(db)
	if err != nil {
		return err
	}
	// 首次部署：写 v1。已有库：schema_meta 表是空的（旧版本部署）→ 默认 v1。
	if cur == 0 {
		cur = 1
		if err := writeVersion(db, cur); err != nil {
			return err
		}
	}

	for _, m := range migrations {
		if m.fromV < cur {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("migration v%d→v%d: %w", m.fromV, m.fromV+1, err)
		}
		if err := writeVersion(db, m.fromV+1); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, s := range m.stmts {
		if _, err := tx.Exec(s); err != nil {
			_ = tx.Rollback()
			// SQLite "duplicate column" / "no such table" 等幂等错误：迁移
			// 已被部分应用过；视为成功。靠错误信息匹配是最务实的做法。
			if isIdempotentErr(err) {
				continue
			}
			return err
		}
	}
	return tx.Commit()
}

func readVersion(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRow(`SELECT value FROM schema_meta WHERE key = 'version'`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

func writeVersion(db *sql.DB, v int) error {
	_, err := db.Exec(
		`INSERT INTO schema_meta(key, value) VALUES('version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		v,
	)
	return err
}

// isIdempotentErr 判断是否为可安全重试的"已经做过"错误。
// modernc.org/sqlite 的错误文本里包含这些子串时认为幂等。
func isIdempotentErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"duplicate column name",
		"already exists",
		"no such table",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// mkdirAll 是 filepath.MkdirAll 的薄包装，便于在 Open 里直接调。
func mkdirAll(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(dir, "store"), 0755)
}

// sweepStaleRunning 在启动时跑：把 DB 里 status='running' 或 ended_at NULL
// 但实际已经不可能在跑的 session 标记成 interrupted。
//
// 调用时机：migrate 之后、writer goroutine 启动之前（单连接，无竞争）。
// 这是 client 重启 / 崩溃后唯一的修复机会——进程内的 m.sessions 已经清空，
// 没人能给这些 session 收尾。
func sweepStaleRunning(db *sql.DB) error {
	res, err := db.Exec(
		`UPDATE sessions
		    SET status = 'interrupted',
		        ended_at = COALESCE(ended_at, strftime('%s','now') * 1000)
		  WHERE ended_at IS NULL
		     OR status = 'running'
		     OR status = ''`,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Default().Info("store: swept stale running sessions", "count", n)
	}
	return nil
}

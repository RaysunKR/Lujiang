package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lujiang/lujiang/internal/proto"
)

// Session 是 sessions 表的 Go 视图。
type Session struct {
	ID        string
	Backend   string
	Cwd       string
	CreatedAt time.Time
	EndedAt   time.Time
	Status    string
	LastSeq   int64
	// ProviderSessionID 是 backend 自己的 session id（如 Claude 的 session-xxx）。
	// 多轮对话时，下一条 prompt 用它作为 resume_from，保留 provider 端上下文。
	ProviderSessionID string
	// Title 是会话标题（首条 user prompt 截断版），sidebar 主显示。
	// 空表示老 session 迁移过来还没有 title，UI 兜底用 backend+cwd+time。
	Title string
}

// CreateSession 插入一行新 session。
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		var endedAt sql.NullInt64
		if !sess.EndedAt.IsZero() {
			endedAt.Int64 = sess.EndedAt.UnixMilli()
			endedAt.Valid = true
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions(id, backend, cwd, created_at, ended_at, status, last_seq, provider_session_id, title)
			 VALUES(?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			sess.ID, sess.Backend, sess.Cwd, sess.CreatedAt.UnixMilli(), endedAt,
			sql.NullString{String: sess.Status, Valid: sess.Status != ""},
			sql.NullString{String: sess.ProviderSessionID, Valid: sess.ProviderSessionID != ""},
			sql.NullString{String: sess.Title, Valid: sess.Title != ""})
		return err
	})
}

// UpdateProviderSessionID 在第一次拿到 backend session id（如 Claude 的 system
// frame）后写入；后续多轮对话用它做 resume_from。
// 幂等：重复调用只更新非空值。
func (s *Store) UpdateProviderSessionID(ctx context.Context, sessID, providerID string) error {
	if sessID == "" || providerID == "" {
		return nil
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET provider_session_id = ? WHERE id = ? AND (provider_session_id IS NULL OR provider_session_id = '')`,
			providerID, sessID)
		return err
	})
}

// GetProviderSessionID 读出 session 当前存着的 provider session id（多轮对话续
// --resume 时用）。空字符串 = 还没拿到 / 不存在。
func (s *Store) GetProviderSessionID(ctx context.Context, sessID string) (string, error) {
	var pid sql.NullString
	err := s.Query(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT provider_session_id FROM sessions WHERE id = ?`, sessID)
		return row.Scan(&pid)
	})
	if err != nil {
		return "", err
	}
	return pid.String, nil
}

// MarkSessionRunning 在多轮对话的后续 turn 把 session 行从 "completed" 状态
// 翻回 "running"：清 ended_at、写新 status。让 sidebar 在新一轮进行中显示对的
// 状态而不是上轮结束态。
func (s *Store) MarkSessionRunning(ctx context.Context, sessID string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET ended_at = NULL, status = 'running' WHERE id = ?`,
			sessID)
		return err
	})
}

// FinishSession 把 session 标记为结束并写入 status。
func (s *Store) FinishSession(ctx context.Context, id, status string, endedAt time.Time) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET ended_at = ?, status = ? WHERE id = ?`,
			endedAt.UnixMilli(), status, id)
		return err
	})
}

// AppendEvent 把一条 proto.AgentEvent 落库；seq 单调递增（每 session 独立）。
//
// 同时更新 sessions.last_seq，方便 resume 时按 last_acked_seq+1 重放。
func (s *Store) AppendEvent(ctx context.Context, sessID string, ev proto.AgentEvent) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		payload, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("store: marshal event: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events(session_id, seq, ts, type, payload) VALUES(?, ?, ?, ?, ?)`,
			sessID, ev.Seq, ev.Ts, ev.Type, string(payload)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET last_seq = ? WHERE id = ?`, ev.Seq, sessID)
		return err
	})
}

// ReplayEvents 取 session 从 sinceSeq+1 开始的所有事件，按 seq 升序。
func (s *Store) ReplayEvents(ctx context.Context, sessID string, sinceSeq int64, fn func(proto.AgentEvent) error) error {
	return s.Query(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT payload FROM events WHERE session_id = ? AND seq > ? ORDER BY seq ASC`,
			sessID, sinceSeq)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var ev proto.AgentEvent
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				continue
			}
			if err := fn(ev); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// SessionInfo 返回 session 的状态信息（用于 resume 时判断是否还活着）。
// ended_at>0 表示 session 已自然结束（不会再有新事件）。
func (s *Store) SessionInfo(ctx context.Context, sessID string) (exists, ended bool, lastSeq int64, err error) {
	err = s.Query(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT last_seq, ended_at FROM sessions WHERE id = ?`,
			sessID)
		var ls, endedAt sql.NullInt64
		if e := row.Scan(&ls, &endedAt); e != nil {
			if e == sql.ErrNoRows {
				return nil
			}
			return e
		}
		exists = true
		lastSeq = ls.Int64
		ended = endedAt.Valid && endedAt.Int64 > 0
		return nil
	})
	return
}

// SessionStatus 读 sessions.status（completed / failed / interrupted / running）。
// 用于 resume 已 done 的 session 时合成 session.idle 事件携带正确的终态。
// 不存在返回 ("", nil)。
func (s *Store) SessionStatus(ctx context.Context, sessID string) (string, error) {
	var status sql.NullString
	err := s.Query(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT status FROM sessions WHERE id = ?`, sessID)
		return row.Scan(&status)
	})
	if err == sql.ErrNoRows {
		return "", nil
	}
	return status.String, err
}

// ListSessions 返回最近 limit 条 session（按 created_at 倒序）。limit<=0 用默认 50。
// cwdFilter 非空时只返回同目录的 session；空 = 不过滤。
func (s *Store) ListSessions(ctx context.Context, limit int, cwdFilter string) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []Session
	err := s.Query(ctx, func(tx *sql.Tx) error {
		var (
			rows *sql.Rows
			err  error
		)
		if cwdFilter != "" {
			rows, err = tx.QueryContext(ctx,
				`SELECT id, backend, cwd, created_at, COALESCE(ended_at, 0), COALESCE(status, ''), last_seq, COALESCE(provider_session_id, ''), COALESCE(title, '')
				 FROM sessions WHERE cwd = ? ORDER BY created_at DESC LIMIT ?`,
				cwdFilter, limit,
			)
		} else {
			rows, err = tx.QueryContext(ctx,
				`SELECT id, backend, cwd, created_at, COALESCE(ended_at, 0), COALESCE(status, ''), last_seq, COALESCE(provider_session_id, ''), COALESCE(title, '')
				 FROM sessions ORDER BY created_at DESC LIMIT ?`,
				limit,
			)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id                string
				backend           string
				cwd               string
				createdAt         int64
				endedAt           int64
				status            string
				lastSeq           int64
				providerSessionID string
				title             string
			)
			if err := rows.Scan(&id, &backend, &cwd, &createdAt, &endedAt, &status, &lastSeq, &providerSessionID, &title); err != nil {
				return err
			}
			sess := Session{
				ID:                id,
				Backend:           backend,
				Cwd:               cwd,
				CreatedAt:         time.UnixMilli(createdAt),
				LastSeq:           lastSeq,
				Status:            status,
				ProviderSessionID: providerSessionID,
				Title:             title,
			}
			if endedAt > 0 {
				sess.EndedAt = time.UnixMilli(endedAt)
			}
			out = append(out, sess)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

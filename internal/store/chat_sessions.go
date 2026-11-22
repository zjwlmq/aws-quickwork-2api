package store

import (
	"context"
	"database/sql"
	"time"
)

// ChatSession holds routing and transcript fingerprints, never transcript text.
type ChatSession struct {
	ID, Owner, Scope, Model, ContextHash string
	ThreadID, PrefixHash, Phase          string
	AccountID, MessageCount, Version     int64
	CreatedAt, UpdatedAt, ExpiresAt      int64
}

const chatSessionColumns = `id,owner,scope,model,context_hash,thread_id,prefix_hash,phase,account_id,message_count,version,created_at,updated_at,expires_at`

func scanChatSession(row scanner) (*ChatSession, error) {
	var s ChatSession
	err := row.Scan(&s.ID, &s.Owner, &s.Scope, &s.Model, &s.ContextHash, &s.ThreadID, &s.PrefixHash, &s.Phase, &s.AccountID, &s.MessageCount, &s.Version, &s.CreatedAt, &s.UpdatedAt, &s.ExpiresAt)
	return &s, err
}

func (s *Store) InitChatSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS chat_sessions(
id TEXT PRIMARY KEY,owner TEXT NOT NULL,scope TEXT NOT NULL,model TEXT NOT NULL,context_hash TEXT NOT NULL,
thread_id TEXT NOT NULL,prefix_hash TEXT NOT NULL DEFAULT '',phase TEXT NOT NULL,
account_id INTEGER NOT NULL DEFAULT 0,message_count INTEGER NOT NULL DEFAULT 0,version INTEGER NOT NULL DEFAULT 0,
created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_match ON chat_sessions(owner,scope,model,context_hash,phase,updated_at);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_expiry ON chat_sessions(phase,expires_at);`)
	return err
}

func (s *Store) RecoverChatSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET phase='retired' WHERE phase='inflight'`)
	return err
}

func (s *Store) ChatCandidates(ctx context.Context, owner, scope, model, contextHash string) ([]ChatSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+chatSessionColumns+` FROM chat_sessions WHERE owner=? AND scope=? AND model=? AND context_hash=? AND phase='ready' AND expires_at>? ORDER BY message_count DESC,updated_at DESC LIMIT 256`, owner, scope, model, contextHash, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatSession
	for rows.Next() {
		v, err := scanChatSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func (s *Store) GetChatSession(ctx context.Context, owner, id string) (*ChatSession, error) {
	return scanChatSession(s.db.QueryRowContext(ctx, `SELECT `+chatSessionColumns+` FROM chat_sessions WHERE owner=? AND id=?`, owner, id))
}

func (s *Store) CreateChatSession(ctx context.Context, v *ChatSession) error {
	now := time.Now().Unix()
	v.Phase, v.CreatedAt, v.UpdatedAt, v.ExpiresAt = "inflight", now, now, now+86400
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_sessions(`+chatSessionColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.Owner, v.Scope, v.Model, v.ContextHash, v.ThreadID, v.PrefixHash, v.Phase, v.AccountID, v.MessageCount, v.Version, v.CreatedAt, v.UpdatedAt, v.ExpiresAt)
	return err
}

// ClaimChatSession is a compare-and-swap: simultaneous branches cannot append to
// the same native history. A losing request must create a new native thread.
func (s *Store) ClaimChatSession(ctx context.Context, id string, version int64) (bool, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET phase='inflight',version=version+1,updated_at=? WHERE id=? AND version=? AND phase='ready' AND expires_at>?`, time.Now().Unix(), id, version, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) BindChatSession(ctx context.Context, id, thread string, account int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET thread_id=?,account_id=?,updated_at=? WHERE id=? AND phase='inflight'`, thread, account, time.Now().Unix(), id)
	return err
}

func (s *Store) CompleteChatSession(ctx context.Context, id, prefix string, count int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET phase='ready',prefix_hash=?,message_count=?,updated_at=?,expires_at=? WHERE id=? AND phase='inflight'`, prefix, count, time.Now().Unix(), time.Now().Add(24*time.Hour).Unix(), id)
	return err
}

func (s *Store) ReleaseChatSession(ctx context.Context, id string, reusable bool) error {
	phase := "retired"
	if reusable {
		phase = "ready"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET phase=?,updated_at=? WHERE id=? AND phase='inflight'`, phase, time.Now().Unix(), id)
	return err
}

func (s *Store) RetireChatSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chat_sessions SET phase='retired',updated_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

func (s *Store) StaleChatSessions(ctx context.Context, limit int) ([]ChatSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+chatSessionColumns+` FROM chat_sessions WHERE phase='retired' OR (phase='ready' AND expires_at<=?) ORDER BY updated_at LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatSession
	for rows.Next() {
		v, err := scanChatSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func (s *Store) DeleteChatSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM chat_sessions WHERE id=? AND phase<>'inflight'`, id)
	return err
}

// MarkChatAccountUsed preserves usage accounting for an already bound account.
func (s *Store) MarkChatAccountUsed(ctx context.Context, id int64) error {
	r, err := s.db.ExecContext(ctx, `UPDATE accounts SET last_used_at=?,use_count=use_count+1 WHERE id=? AND disabled=0`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

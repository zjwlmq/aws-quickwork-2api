package store

import (
	"context"
	"errors"
)

var ErrAdminSessionLimit = errors.New("administrator session limit reached")

// AdminSession stores a one-way hash of the browser's random session token.
// Passwords and usable bearer tokens do not enter this table.
type AdminSession struct {
	ID          string
	CSRF        string
	ExpiresAt   int64
	AuthVersion string
}

func (s *Store) InitAdminSessions(ctx context.Context, version string, now int64) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS admin_sessions (
		id TEXT PRIMARY KEY, csrf TEXT NOT NULL, expires_at INTEGER NOT NULL,
		auth_version TEXT NOT NULL);
		CREATE INDEX IF NOT EXISTS idx_admin_sessions_expiry ON admin_sessions(expires_at);`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at<=? OR auth_version<>?`, now, version)
	return err
}

func (s *Store) SaveAdminSession(ctx context.Context, value AdminSession, now int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at<=? OR auth_version<>?`, now, value.AuthVersion); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_sessions`).Scan(&count); err != nil {
		return err
	}
	if count >= 64 {
		return ErrAdminSessionLimit
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_sessions(id,csrf,expires_at,auth_version) VALUES(?,?,?,?)`, value.ID, value.CSRF, value.ExpiresAt, value.AuthVersion); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetAdminSession(ctx context.Context, id, version string, now int64) (*AdminSession, error) {
	var value AdminSession
	err := s.db.QueryRowContext(ctx, `SELECT id,csrf,expires_at,auth_version FROM admin_sessions WHERE id=? AND auth_version=? AND expires_at>?`, id, version, now).Scan(&value.ID, &value.CSRF, &value.ExpiresAt, &value.AuthVersion)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *Store) DeleteAdminSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id=?`, id)
	return err
}

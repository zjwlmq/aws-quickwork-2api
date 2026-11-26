// Package store persists upstream accounts in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Account is one upstream QuickWork identity plus everything needed to
// refresh it and open its WebSocket session.
type Account struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider"`
	BuildStage  string `json:"build_stage"`
	Region      string `json:"region"`

	TenantURL string `json:"tenant_url"`
	WSHost    string `json:"ws_host"`
	WSPath    string `json:"ws_path"`

	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`
	IDToken      string `json:"-"`

	ActorARN     string `json:"actor_arn"`
	AWSAccountID string `json:"aws_account_id"`
	AccountName  string `json:"account_name"`

	ClientID      string `json:"-"`
	ClientSecret  string `json:"-"`
	IssuerURL     string `json:"-"`
	IDCRegion     string `json:"-"`
	AuthEndpoint  string `json:"-"`
	TokenEndpoint string `json:"-"`
	RedirectURI   string `json:"-"`

	UserInfo string `json:"-"`

	ExpiresAt  int64  `json:"expires_at"`
	ObtainedAt int64  `json:"obtained_at"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	LastUsedAt int64  `json:"last_used_at"`
	UseCount   int64  `json:"use_count"`
	Disabled   bool   `json:"disabled"`
	LastError  string `json:"last_error"`
}

// JWT returns the token used for the upstream WebSocket handshake.
func (a *Account) JWT() string {
	if a.IDToken != "" {
		return a.IDToken
	}
	return a.AccessToken
}

// Expiring reports whether the access token should be refreshed now.
func (a *Account) Expiring(now time.Time, before time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return now.Add(before).Unix() >= a.ExpiresAt
}

// Store is a small SQLite-backed account repository. A single connection is
// used to avoid SQLite writer contention.
type Store struct {
	db *sql.DB
}

// Open creates the database and its schema when needed.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.InitChatSessions(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	// The database holds refresh tokens and client secrets.
	_ = os.Chmod(path, 0o600)
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	email          TEXT NOT NULL DEFAULT '' COLLATE NOCASE,
	display_name   TEXT NOT NULL DEFAULT '',
	provider       TEXT NOT NULL DEFAULT '',
	build_stage    TEXT NOT NULL DEFAULT 'prod',
	region         TEXT NOT NULL DEFAULT '',
	tenant_url     TEXT NOT NULL DEFAULT '',
	ws_host        TEXT NOT NULL DEFAULT '',
	ws_path        TEXT NOT NULL DEFAULT '',
	access_token   TEXT NOT NULL DEFAULT '',
	refresh_token  TEXT NOT NULL DEFAULT '',
	id_token       TEXT NOT NULL DEFAULT '',
	actor_arn      TEXT NOT NULL DEFAULT '',
	aws_account_id TEXT NOT NULL DEFAULT '',
	account_name   TEXT NOT NULL DEFAULT '',
	client_id      TEXT NOT NULL DEFAULT '',
	client_secret  TEXT NOT NULL DEFAULT '',
	issuer_url     TEXT NOT NULL DEFAULT '',
	idc_region     TEXT NOT NULL DEFAULT '',
	auth_endpoint  TEXT NOT NULL DEFAULT '',
	token_endpoint TEXT NOT NULL DEFAULT '',
	redirect_uri   TEXT NOT NULL DEFAULT '',
	user_info      TEXT NOT NULL DEFAULT '',
	expires_at     INTEGER NOT NULL DEFAULT 0,
	obtained_at    INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL DEFAULT 0,
	updated_at     INTEGER NOT NULL DEFAULT 0,
	last_used_at   INTEGER NOT NULL DEFAULT 0,
	use_count      INTEGER NOT NULL DEFAULT 0,
	disabled       INTEGER NOT NULL DEFAULT 0,
	last_error     TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_email_tenant ON accounts(email, tenant_url);
CREATE INDEX IF NOT EXISTS idx_accounts_enabled ON accounts(disabled, id);
`
	_, err := s.db.Exec(schema)
	return err
}

const columns = `id, email, display_name, provider, build_stage, region,
	tenant_url, ws_host, ws_path, access_token, refresh_token, id_token,
	actor_arn, aws_account_id, account_name, client_id, client_secret,
	issuer_url, idc_region, auth_endpoint, token_endpoint, redirect_uri,
	user_info, expires_at, obtained_at, created_at, updated_at,
	last_used_at, use_count, disabled, last_error`

type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(row scanner) (*Account, error) {
	var a Account
	var disabled int
	err := row.Scan(
		&a.ID, &a.Email, &a.DisplayName, &a.Provider, &a.BuildStage, &a.Region,
		&a.TenantURL, &a.WSHost, &a.WSPath, &a.AccessToken, &a.RefreshToken, &a.IDToken,
		&a.ActorARN, &a.AWSAccountID, &a.AccountName, &a.ClientID, &a.ClientSecret,
		&a.IssuerURL, &a.IDCRegion, &a.AuthEndpoint, &a.TokenEndpoint, &a.RedirectURI,
		&a.UserInfo, &a.ExpiresAt, &a.ObtainedAt, &a.CreatedAt, &a.UpdatedAt,
		&a.LastUsedAt, &a.UseCount, &disabled, &a.LastError,
	)
	if err != nil {
		return nil, err
	}
	a.Disabled = disabled != 0
	return &a, nil
}

// Save inserts a new account or updates the matching (email, tenant_url)
// account. The account ID is updated in place.
func (s *Store) Save(ctx context.Context, a *Account) error {
	return saveAccount(ctx, s.db, a, false)
}

type accountWriter interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func saveAccount(ctx context.Context, target accountWriter, a *Account, restore bool) error {
	now := time.Now().Unix()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	var disabled int
	if a.Disabled {
		disabled = 1
	}
	const q = `
INSERT INTO accounts (email, display_name, provider, build_stage, region,
	tenant_url, ws_host, ws_path, access_token, refresh_token, id_token,
	actor_arn, aws_account_id, account_name, client_id, client_secret,
	issuer_url, idc_region, auth_endpoint, token_endpoint, redirect_uri,
	user_info, expires_at, obtained_at, created_at, updated_at,
	last_used_at, use_count, disabled, last_error)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(email, tenant_url) DO UPDATE SET
	display_name=excluded.display_name,
	provider=excluded.provider,
	build_stage=excluded.build_stage,
	region=excluded.region,
	ws_host=excluded.ws_host,
	ws_path=excluded.ws_path,
	access_token=excluded.access_token,
	refresh_token=excluded.refresh_token,
	id_token=excluded.id_token,
	actor_arn=excluded.actor_arn,
	aws_account_id=excluded.aws_account_id,
	account_name=excluded.account_name,
	client_id=excluded.client_id,
	client_secret=excluded.client_secret,
	issuer_url=excluded.issuer_url,
	idc_region=excluded.idc_region,
	auth_endpoint=excluded.auth_endpoint,
	token_endpoint=excluded.token_endpoint,
	redirect_uri=excluded.redirect_uri,
	user_info=excluded.user_info,
	expires_at=excluded.expires_at,
	obtained_at=excluded.obtained_at,
	updated_at=excluded.updated_at,
	disabled=0,
	last_error=''
RETURNING id`
	query := q
	if restore {
		query = strings.Replace(query, "disabled=0,", "disabled=excluded.disabled,", 1)
	}
	err := target.QueryRowContext(ctx, query,
		a.Email, a.DisplayName, a.Provider, a.BuildStage, a.Region,
		a.TenantURL, a.WSHost, a.WSPath, a.AccessToken, a.RefreshToken, a.IDToken,
		a.ActorARN, a.AWSAccountID, a.AccountName, a.ClientID, a.ClientSecret,
		a.IssuerURL, a.IDCRegion, a.AuthEndpoint, a.TokenEndpoint, a.RedirectURI,
		a.UserInfo, a.ExpiresAt, a.ObtainedAt, a.CreatedAt, a.UpdatedAt,
		a.LastUsedAt, a.UseCount, disabled, a.LastError,
	).Scan(&a.ID)
	return err
}

// Update rewrites a stored account after a token refresh.
func (s *Store) Update(ctx context.Context, a *Account) error {
	a.UpdatedAt = time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
UPDATE accounts SET
	display_name=?, provider=?, build_stage=?, region=?, tenant_url=?, ws_host=?, ws_path=?,
	access_token=?, refresh_token=?, id_token=?, actor_arn=?, aws_account_id=?, account_name=?,
	client_id=?, client_secret=?, issuer_url=?, idc_region=?, auth_endpoint=?, token_endpoint=?,
	redirect_uri=?, user_info=?, expires_at=?, obtained_at=?, updated_at=?, last_error=?
WHERE id=?`,
		a.DisplayName, a.Provider, a.BuildStage, a.Region, a.TenantURL, a.WSHost, a.WSPath,
		a.AccessToken, a.RefreshToken, a.IDToken, a.ActorARN, a.AWSAccountID, a.AccountName,
		a.ClientID, a.ClientSecret, a.IssuerURL, a.IDCRegion, a.AuthEndpoint, a.TokenEndpoint,
		a.RedirectURI, a.UserInfo, a.ExpiresAt, a.ObtainedAt, a.UpdatedAt, a.LastError, a.ID,
	)
	return err
}

// List returns all accounts ordered by ID.
func (s *Store) List(ctx context.Context) ([]Account, error) {
	return s.query(ctx, `SELECT `+columns+` FROM accounts ORDER BY id`)
}

// Enabled returns accounts eligible for rotation ordered by ID.
func (s *Store) Enabled(ctx context.Context) ([]Account, error) {
	return s.query(ctx, `SELECT `+columns+` FROM accounts WHERE disabled=0 ORDER BY id`)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Account, 0, 8)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// Get returns one account, or sql.ErrNoRows when absent.
func (s *Store) Get(ctx context.Context, id int64) (*Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM accounts WHERE id=?`, id)
	return scanAccount(row)
}

// Delete removes one account.
func (s *Store) Delete(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteAll removes every account and returns how many were deleted.
func (s *Store) DeleteAll(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MarkUsed records a successful account selection.
func (s *Store) MarkUsed(ctx context.Context, id int64) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET last_used_at=?, use_count=use_count+1 WHERE id=?`, now, id)
	return err
}

// SetError records the last failure without disabling the account.
func (s *Store) SetError(ctx context.Context, id int64, message string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET last_error=?, updated_at=? WHERE id=?`,
		truncate(message, 300), time.Now().Unix(), id)
	return err
}

// SetDisabled toggles an account in or out of rotation.
func (s *Store) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET disabled=? WHERE id=?`, v, id)
	return err
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}

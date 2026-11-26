package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type AccountFilter struct {
	Search, Status string
	Page, PageSize int
}

type AccountSummary struct {
	Total     int `json:"total"`
	Ready     int `json:"ready"`
	Attention int `json:"attention"`
}

type AccountPage struct {
	Accounts       []Account
	Total          int
	Page, PageSize int
	Pages          int
	Summary        AccountSummary
}

// Keep the status precedence aligned with the public administrator account view.
const accountStatusSQL = `CASE WHEN disabled<>0 THEN 'disabled'
	WHEN last_error<>'' OR (id_token='' AND access_token='') THEN 'error'
	WHEN expires_at>0 AND expires_at<=? THEN 'expired' ELSE 'ready' END`

// ListAccountPage applies filters and pagination inside SQLite. Summary counts
// describe all accounts; Total describes only the matching accounts.
func (s *Store) ListAccountPage(ctx context.Context, filter AccountFilter) (AccountPage, error) {
	var out AccountPage
	if filter.Page < 1 || filter.PageSize < 1 || filter.PageSize > 100 {
		return out, errors.New("invalid account pagination")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(status='ready'),0),
		COALESCE(SUM(status IN ('expired','error')),0) FROM (SELECT `+accountStatusSQL+` AS status FROM accounts)`, now).
		Scan(&out.Summary.Total, &out.Summary.Ready, &out.Summary.Attention)
	if err != nil {
		return out, err
	}
	where := " WHERE 1=1"
	args := []any{now}
	if search := strings.TrimSpace(filter.Search); search != "" {
		search = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search)
		where += ` AND (email LIKE ? ESCAPE '\' OR display_name LIKE ? ESCAPE '\'
			OR account_name LIKE ? ESCAPE '\' OR CAST(id AS TEXT) LIKE ? ESCAPE '\')`
		for range 4 {
			args = append(args, "%"+search+"%")
		}
	}
	if filter.Status != "" && filter.Status != "all" {
		where += " AND status=?"
		args = append(args, filter.Status)
	}
	from := ` FROM (SELECT ` + columns + `, ` + accountStatusSQL + ` AS status FROM accounts)` + where
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*)`+from, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	out.PageSize = filter.PageSize
	out.Pages = max(1, (out.Total+filter.PageSize-1)/filter.PageSize)
	out.Page = min(filter.Page, out.Pages)
	args = append(args, filter.PageSize, (out.Page-1)*filter.PageSize)
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+from+` ORDER BY id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return out, err
	}
	out.Accounts = make([]Account, 0, filter.PageSize)
	for rows.Next() {
		a, scanErr := scanAccount(rows)
		if scanErr != nil {
			rows.Close()
			return out, scanErr
		}
		out.Accounts = append(out.Accounts, *a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// SelectedAccounts returns a bounded export or the accounts needed for one page
// of quota queries. An empty ID list selects all accounts, up to limit.
func (s *Store) SelectedAccounts(ctx context.Context, ids []int64, limit int) ([]Account, error) {
	query := `SELECT ` + columns + ` FROM accounts`
	args := make([]any, 0, len(ids)+1)
	if len(ids) > 0 {
		query += ` WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, limit)
	return s.query(ctx, query, args...)
}

type AccountImportResult struct {
	Added   int     `json:"added"`
	Updated int     `json:"updated"`
	Skipped int     `json:"skipped"`
	Total   int     `json:"total"`
	IDs     []int64 `json:"-"`
}

// ImportAccounts is atomic. Repeated identities within a file are skipped, and
// updates preserve local IDs, usage counters and creation timestamps.
func (s *Store) ImportAccounts(ctx context.Context, records []Account, update bool) (AccountImportResult, error) {
	out := AccountImportResult{Total: len(records)}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AccountImportResult{}, err
	}
	defer tx.Rollback()
	seen := make(map[string]bool, len(records))
	for _, a := range records {
		key := strings.ToLower(a.Email) + "\x00" + a.TenantURL
		if seen[key] {
			out.Skipped++
			continue
		}
		seen[key] = true
		var id int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE email=? AND tenant_url=?`, a.Email, a.TenantURL).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return AccountImportResult{}, err
		}
		if id != 0 && !update {
			out.Skipped++
			continue
		}
		// Transfer files do not control server-local identities or statistics.
		a.ID, a.CreatedAt, a.UpdatedAt, a.LastUsedAt, a.UseCount = 0, 0, 0, 0, 0
		a.LastError = ""
		if err = saveAccount(ctx, tx, &a, true); err != nil {
			return AccountImportResult{}, fmt.Errorf("save imported account: %w", err)
		}
		out.IDs = append(out.IDs, a.ID)
		if id == 0 {
			out.Added++
		} else {
			out.Updated++
		}
	}
	if err = tx.Commit(); err != nil {
		return AccountImportResult{}, err
	}
	return out, nil
}

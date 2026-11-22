// Package accounts selects accounts for rotation and keeps their tokens fresh.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quickwork-openai/internal/config"
	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/oauth"
	"quickwork-openai/internal/store"
)

// ErrNoAccounts means the database has no enabled account.
var ErrNoAccounts = errors.New("no accounts configured; run `quickproxy login`")

// Manager rotates over enabled accounts and refreshes tokens on demand.
type Manager struct {
	store *store.Store
	cfg   *config.Config

	next atomic.Uint64

	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func New(st *store.Store, cfg *config.Config) *Manager {
	return &Manager{store: st, cfg: cfg, locks: make(map[int64]*sync.Mutex)}
}

func (m *Manager) lockFor(id int64) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	return l
}

// Next returns the next usable account in round-robin order. Expiring tokens
// are refreshed before the account is returned. Accounts whose refresh fails
// are skipped so one broken login does not take the proxy down.
func (m *Manager) Next(ctx context.Context) (*store.Account, error) {
	list, err := m.store.Enabled(ctx)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		diagnostics.Event(ctx, "error", "account.unavailable", "reason", "no_enabled_accounts")
		return nil, ErrNoAccounts
	}
	start := int(m.next.Add(1)-1) % len(list)
	var errs []string
	for i := range len(list) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a := &list[(start+i)%len(list)]
		diagnostics.RegisterSecrets(a.AccessToken, a.RefreshToken, a.IDToken)
		diagnostics.Event(ctx, "debug", "account.candidate", "account_id", a.ID, "provider", a.Provider, "expires_at", a.ExpiresAt, "enabled_accounts", len(list))
		usable, err := m.ensureFresh(ctx, a)
		if err != nil {
			diagnostics.Event(ctx, "warn", "account.skipped", "account_id", a.ID, "error", err)
			_ = m.store.SetError(ctx, a.ID, err.Error())
			errs = append(errs, fmt.Sprintf("account %d (%s): %v", a.ID, a.Email, err))
			continue
		}
		if err := m.store.MarkUsed(ctx, usable.ID); err != nil {
			return nil, err
		}
		diagnostics.Event(ctx, "info", "account.selected", "account_id", usable.ID, "provider", usable.Provider, "expires_at", usable.ExpiresAt)
		return usable, nil
	}
	return nil, fmt.Errorf("no usable account: %s", strings.Join(errs, "; "))
}

func (m *Manager) ensureFresh(ctx context.Context, a *store.Account) (*store.Account, error) {
	if !a.Expiring(time.Now(), m.cfg.RefreshBefore.Duration()) {
		if a.JWT() == "" {
			return nil, errors.New("account has no access token")
		}
		return a, nil
	}
	lock := m.lockFor(a.ID)
	lock.Lock()
	defer lock.Unlock()

	// Another request may have refreshed it while we waited.
	latest, err := m.store.Get(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	if !latest.Expiring(time.Now(), m.cfg.RefreshBefore.Duration()) {
		if latest.JWT() == "" {
			return nil, errors.New("account has no access token")
		}
		return latest, nil
	}
	return m.refreshLocked(ctx, latest)
}

// refreshLocked refreshes and persists one account. The caller holds its lock.
func (m *Manager) refreshLocked(ctx context.Context, a *store.Account) (*store.Account, error) {
	diagnostics.RegisterSecrets(a.AccessToken, a.RefreshToken, a.IDToken)
	started := time.Now()
	diagnostics.Event(ctx, "info", "account.refresh.started", "account_id", a.ID, "provider", a.Provider)
	refreshed, err := oauth.Refresh(ctx, a)
	if err != nil {
		diagnostics.Event(ctx, "error", "account.refresh.failed", "account_id", a.ID, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		return nil, err
	}
	if err := m.store.Update(ctx, refreshed); err != nil {
		diagnostics.Event(ctx, "error", "account.refresh.save_failed", "account_id", a.ID, "error", err)
		return nil, err
	}
	diagnostics.RegisterSecrets(refreshed.AccessToken, refreshed.RefreshToken, refreshed.IDToken)
	diagnostics.Event(ctx, "info", "account.refresh.completed", "account_id", a.ID, "duration_ms", time.Since(started).Milliseconds(), "expires_at", refreshed.ExpiresAt)
	return refreshed, nil
}

// RefreshOne forces a refresh of one account.
func (m *Manager) RefreshOne(ctx context.Context, id int64) (*store.Account, error) {
	a, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()
	if a, err = m.store.Get(ctx, id); err != nil {
		return nil, err
	}
	refreshed, err := m.refreshLocked(ctx, a)
	if err != nil {
		_ = m.store.SetError(ctx, id, err.Error())
		return nil, err
	}
	return refreshed, nil
}

// RefreshResult is one account's outcome from RefreshAll.
type RefreshResult struct {
	Account *store.Account
	Err     error
}

// RefreshAll refreshes every stored account that has a refresh token.
func (m *Manager) RefreshAll(ctx context.Context) ([]RefreshResult, error) {
	list, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]RefreshResult, 0, len(list))
	for i := range list {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		a := &list[i]
		if strings.TrimSpace(a.RefreshToken) == "" {
			results = append(results, RefreshResult{Account: a, Err: oauth.ErrNoRefreshToken})
			_ = m.store.SetError(ctx, a.ID, oauth.ErrNoRefreshToken.Error())
			continue
		}
		refreshed, err := m.RefreshOne(ctx, a.ID)
		if refreshed == nil {
			refreshed = a
		}
		results = append(results, RefreshResult{Account: refreshed, Err: err})
	}
	return results, nil
}

// ReportFailure records an upstream failure for an account.
func (m *Manager) ReportFailure(ctx context.Context, id int64, err error) {
	if err == nil {
		return
	}
	diagnostics.Event(ctx, "warn", "account.upstream_failed", "account_id", id, "error", err)
	_ = m.store.SetError(ctx, id, err.Error())
}

// EnabledCount returns the number of accounts in rotation.
func (m *Manager) EnabledCount(ctx context.Context) (int, error) {
	list, err := m.store.Enabled(ctx)
	if err != nil {
		return 0, err
	}
	return len(list), nil
}

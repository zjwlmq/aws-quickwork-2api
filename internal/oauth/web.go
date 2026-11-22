package oauth

import (
	"context"
	"errors"
	"sync"
	"time"

	"quickwork-openai/internal/store"
)

// BrowserLogin exposes the existing manual OAuth flow to an authenticated web
// console. It does not open a localhost listener or launch child processes.
type BrowserLogin struct {
	mu      sync.Mutex
	flow    *loginFlow
	expires time.Time
	doneID  int64
}

type BrowserLoginState struct {
	Step      string `json:"step"`
	URL       string `json:"url,omitempty"`
	ExpiresAt int64  `json:"expires_at"`
	AccountID int64  `json:"account_id,omitempty"`
}

func NewBrowserLogin(opts LoginOptions, st *store.Store) (*BrowserLogin, error) {
	opts = normalizeLoginOptions(opts)
	f, err := newLoginFlow(opts, st, opts.Port)
	if err != nil {
		return nil, err
	}
	return &BrowserLogin{flow: f, expires: time.Now().Add(opts.Timeout)}, nil
}

func (b *BrowserLogin) State() BrowserLoginState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked()
}

func (b *BrowserLogin) stateLocked() BrowserLoginState {
	s := BrowserLoginState{Step: "discovery", ExpiresAt: b.expires.Unix()}
	if b.doneID > 0 {
		s.Step, s.AccountID = "complete", b.doneID
	} else if time.Now().After(b.expires) {
		s.Step = "expired"
	} else if b.flow.discoveryDone {
		s.Step, s.URL = "authorize", b.flow.authorizeURL
	} else {
		f := b.flow
		s.URL = discoveryURL(f.provider, f.stage, f.region, f.nonce, f.port, f.opts.AppVersion)
	}
	return s
}

func (b *BrowserLogin) Submit(ctx context.Context, callback string) (BrowserLoginState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.doneID > 0 {
		return b.stateLocked(), nil
	}
	if time.Now().After(b.expires) {
		return b.stateLocked(), errors.New("授权链接已过期，请重新添加账号")
	}
	q, err := parseCallbackURL(callback)
	if err != nil {
		return b.stateLocked(), errors.New("请粘贴浏览器地址栏中的完整回调地址")
	}
	ctx, cancel := context.WithDeadline(ctx, b.expires)
	defer cancel()
	switch classifyCallback(q) {
	case callbackDiscovery, callbackDiscoveryError:
		if _, err := b.flow.processDiscovery(ctx, q); err != nil {
			return b.stateLocked(), err
		}
	case callbackCode, callbackCodeError:
		if !b.flow.discoveryDone {
			return b.stateLocked(), errors.New("请先提交第一步的回调地址")
		}
		a, err := b.flow.completeLogin(ctx, q)
		if err != nil {
			return b.stateLocked(), err
		}
		b.doneID = a.ID
	default:
		return b.stateLocked(), errors.New("这不是回调地址。请打开授权链接完成登录，再复制 localhost 地址栏的完整地址")
	}
	return b.stateLocked(), nil
}

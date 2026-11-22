package proxy

import (
	"fmt"
	"strings"

	"quickwork-openai/internal/config"
	"quickwork-openai/internal/store"
)

// Session is the runtime view of one upstream account used by the WebSocket
// transport.
type Session struct {
	AccountID        int64
	JWT              string
	WSHost           string
	TenantURL        string
	WSPath           string
	Provider         string
	ActorARN         string
	Surface          string
	OutpostID        string
	WorkingSessionID string
}

func (s *Session) jwt() string {
	if s == nil {
		return ""
	}
	return s.JWT
}

// SessionFromAccount merges an account with server-side handshake defaults.
func SessionFromAccount(a *store.Account, cfg *config.Config) *Session {
	surface := cfg.Surface
	if surface == "" {
		surface = "QUICK_WORK_DESKTOP_WINDOWS"
	}
	return &Session{
		AccountID:        a.ID,
		JWT:              a.JWT(),
		WSHost:           a.WSHost,
		TenantURL:        a.TenantURL,
		WSPath:           a.WSPath,
		Provider:         a.Provider,
		ActorARN:         a.ActorARN,
		Surface:          surface,
		OutpostID:        cfg.OutpostID,
		WorkingSessionID: cfg.WorkingSessionID,
	}
}

func (s *Session) validate() error {
	if strings.TrimSpace(s.JWT) == "" {
		return fmt.Errorf("account has no jwt/id_token")
	}
	if strings.TrimSpace(s.WSHost) == "" {
		return fmt.Errorf("account has no ws_host")
	}
	return nil
}

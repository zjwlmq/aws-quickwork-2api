package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/store"
)

func loginRemembered(t *testing.T, f *fixture, remember bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"username": "admin", "password": testPassword, "remember_me": remember})
	w := f.request("POST", "/admin/api/login", string(body), false)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	f.csrf = session.CSRF
	f.cookie = w.Result().Cookies()[0]
	expected := int(sessionLifetime.Seconds())
	if remember {
		expected = int(rememberedSessionLifetime.Seconds())
	}
	if f.cookie.MaxAge != expected || !f.cookie.HttpOnly || !f.cookie.Secure || f.cookie.SameSite != http.SameSiteStrictMode || f.cookie.Expires.Before(time.Now().Add(time.Duration(expected-5)*time.Second)) {
		t.Fatal("persistent cookie lifetime or security attributes incorrect")
	}
}

func TestRememberedSessionsSurviveServerRestartAndRevokeAcrossInstances(t *testing.T) {
	for _, remember := range []bool{false, true} {
		t.Run(map[bool]string{false: "12_hours", true: "30_days"}[remember], func(t *testing.T) {
			f := newFixture(t)
			loginRemembered(t, f, remember)
			saved, err := f.st.GetAdminSession(context.Background(), sessionKey(f.cookie.Value), f.s.authVersion(), time.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(saved)
			for _, secret := range []string{f.cookie.Value, testPassword, f.s.cfg.Admin.PasswordHash} {
				if strings.Contains(string(encoded), secret) {
					t.Fatal("saved session contains a password or usable bearer token")
				}
			}
			restarted, err := New(f.s.cfg, f.st, accounts.New(f.st, f.s.cfg))
			if err != nil {
				t.Fatal(err)
			}
			originalHandler := f.h
			f.h = restarted.Handler()
			if w := f.request("GET", "/admin/api/session", "", true); w.Code != 200 || !strings.Contains(w.Body.String(), f.csrf) {
				t.Fatalf("session lost after restart: %d %s", w.Code, w.Body)
			}
			w := f.request("POST", "/admin/api/logout", "", true)
			if w.Code != 200 || w.Result().Cookies()[0].MaxAge >= 0 {
				t.Fatal("logout did not delete persistent browser cookie")
			}
			f.h = originalHandler
			if w := f.request("GET", "/admin/api/session", "", true); w.Code != 401 {
				t.Fatal("logged-out token remained usable by the original server")
			}
			restartedAgain, err := New(f.s.cfg, f.st, accounts.New(f.st, f.s.cfg))
			if err != nil {
				t.Fatal(err)
			}
			f.h = restartedAgain.Handler()
			if w := f.request("GET", "/admin/api/session", "", true); w.Code != 401 {
				t.Fatal("logout was undone by a later restart")
			}
		})
	}
}

func TestAdminCredentialChangeInvalidatesRememberedLogin(t *testing.T) {
	for _, field := range []string{"password", "username", "origin"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			loginRemembered(t, f, true)
			switch field {
			case "password":
				f.s.cfg.Admin.PasswordHash, _ = HashPassword("new-test-console-password")
			case "username":
				f.s.cfg.Admin.Username = "another-admin"
			case "origin":
				f.s.cfg.Admin.PublicURL = "https://changed.example"
			}
			if w := f.request("GET", "/admin/api/session", "", true); w.Code != 401 {
				t.Fatal("changed credentials did not revoke the old session")
			}
		})
	}
}

func TestRememberedLoginDoesNotWeakenAuthenticationOrCSRF(t *testing.T) {
	f := newFixture(t)
	w := f.request("POST", "/admin/api/login", `{"username":"admin","password":"wrong","remember_me":true}`, false)
	if w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Fatal("remembered login bypassed password verification")
	}
	loginRemembered(t, f, true)
	original := f.cookie.Value
	f.cookie.Value = strings.Repeat("x", 43)
	if w := f.request("GET", "/admin/api/session", "", true); w.Code != 401 {
		t.Fatal("forged persistent cookie authenticated")
	}
	f.cookie.Value = original
	f.csrf = "wrong-csrf"
	if w := f.request("POST", "/admin/api/logout", "", true); w.Code != 403 {
		t.Fatal("persistent login bypassed CSRF validation")
	}
	if w := f.request("GET", "/admin/api/session", "", true); w.Code != 200 {
		t.Fatal("CSRF failure unexpectedly revoked the valid login")
	}
}

func TestPersistentSessionLimitIncludesSavedRecords(t *testing.T) {
	f := newFixture(t)
	now := time.Now().Unix()
	for i := 0; i < 64; i++ {
		id := sessionKey(string(rune(i)))
		if err := f.st.SaveAdminSession(context.Background(), store.AdminSession{ID: id, CSRF: "test-csrf", ExpiresAt: now + 3600, AuthVersion: f.s.authVersion()}, now); err != nil {
			t.Fatal(err)
		}
	}
	w := f.request("POST", "/admin/api/login", `{"username":"admin","password":"`+testPassword+`","remember_me":true}`, false)
	if w.Code != 429 || len(w.Result().Cookies()) != 0 {
		t.Fatal("persistent session limit was not enforced")
	}
}

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/store"
)

const testPassword = "test-console-password-1234"

var cachedHash string
var hashOnce sync.Once

type fixture struct {
	s      *Server
	h      http.Handler
	st     *store.Store
	cookie *http.Cookie
	csrf   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	hashOnce.Do(func() {
		var err error
		cachedHash, err = HashPassword(testPassword)
		if err != nil {
			panic(err)
		}
	})
	st, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.APIKeys = []string{"test-api-key-not-an-admin-password"}
	cfg.Admin = config.Admin{Enabled: true, Username: "admin", PasswordHash: cachedHash, PublicURL: "https://console.example"}
	s, err := New(&cfg, st, accounts.New(st, &cfg))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{s: s, h: s.Handler(), st: st}
}

func (f *fixture) request(method, path, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", f.s.cfg.Admin.PublicURL)
	if auth {
		r.AddCookie(f.cookie)
		r.Header.Set("X-CSRF-Token", f.csrf)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *fixture) login(t *testing.T) {
	t.Helper()
	w := f.request("POST", "/admin/api/login", `{"username":"admin","password":"`+testPassword+`"}`, false)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var result struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	f.csrf = result.CSRF
	f.cookie = w.Result().Cookies()[0]
	if !f.cookie.Secure || !f.cookie.HttpOnly || f.cookie.SameSite != http.SameSiteStrictMode || f.cookie.Path != "/admin" {
		t.Fatalf("unsafe cookie: %+v", f.cookie)
	}
}

func TestAdminAuthCSRFAndSecretIsolation(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/admin/api/accounts", "/admin/api/connection", "/admin/api/session"} {
		if w := f.request("GET", path, "", false); w.Code != http.StatusUnauthorized {
			t.Fatalf("public %s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/admin/api/accounts", nil)
	r.Header.Set("Authorization", "Bearer test-api-key-not-an-admin-password")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("API key granted console access")
	}
	if w := f.request("POST", "/admin/api/login", `{"username":"admin","password":"wrong"}`, false); w.Code != http.StatusUnauthorized {
		t.Fatal("wrong password accepted")
	}
	f.login(t)
	a := &store.Account{Email: "member@example.com", Provider: "social", AccessToken: "private-access-value", RefreshToken: "private-refresh-value", IDToken: "private-id-value", ClientSecret: "private-client-value", ExpiresAt: time.Now().Add(time.Hour).Unix(), LastError: "invalid private-access-value"}
	if err := f.st.Save(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	w = f.request("GET", "/admin/api/accounts", "", true)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	for _, secret := range []string{a.AccessToken, a.RefreshToken, a.IDToken, a.ClientSecret, cachedHash} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("secret leaked in account list")
		}
	}
	for _, origin := range []string{"", "https://attacker.example"} {
		r := httptest.NewRequest("DELETE", "/admin/api/accounts/1", nil)
		r.AddCookie(f.cookie)
		r.Header.Set("X-CSRF-Token", f.csrf)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("untrusted origin accepted: %s", origin)
		}
	}
	r = httptest.NewRequest("DELETE", "/admin/api/accounts/1", nil)
	r.AddCookie(f.cookie)
	r.Header.Set("Origin", f.s.cfg.Admin.PublicURL)
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("missing CSRF token accepted")
	}
	if w := f.request("POST", "/admin/api/logout", "", true); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if w := f.request("GET", "/admin/api/accounts", "", true); w.Code != http.StatusUnauthorized {
		t.Fatal("logged-out session remained valid")
	}
}

func TestAdminMutationsUseStoredAccounts(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh-before" {
			t.Error("refresh token not forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-after","refresh_token":"refresh-after","id_token":"id-after","expires_in":3600}`))
	})
	mux.HandleFunc("/integration/quick-work/user-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userInfo":{"email":"member@example.com","userArn":"arn:aws:quicksight:us-east-1:123:user/default/member"}}`))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	a := &store.Account{Email: "member@example.com", Provider: "enterprise", TenantURL: upstream.URL, TokenEndpoint: upstream.URL + "/token", ClientID: "client", AccessToken: "access-before", RefreshToken: "refresh-before", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := f.st.Save(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []string{"true", "false"} {
		w := f.request("PATCH", "/admin/api/accounts/1", `{"disabled":`+disabled+`}`, true)
		if w.Code != http.StatusOK {
			t.Fatal(w.Body)
		}
		saved, _ := f.st.Get(context.Background(), a.ID)
		if saved.Disabled != (disabled == "true") {
			t.Fatal("enabled state not persisted")
		}
	}
	w := f.request("POST", "/admin/api/accounts/1/refresh", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", w.Code, w.Body)
	}
	saved, _ := f.st.Get(context.Background(), a.ID)
	if saved.AccessToken != "access-after" || saved.RefreshToken != "refresh-after" {
		t.Fatal("refreshed credentials not persisted")
	}
	if strings.Contains(w.Body.String(), "access-after") || strings.Contains(w.Body.String(), "refresh-after") {
		t.Fatal("refresh response exposed tokens")
	}
	if w := f.request("DELETE", "/admin/api/accounts/1", "", true); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if list, _ := f.st.List(context.Background()); len(list) != 0 {
		t.Fatal("account not deleted")
	}
	for _, method := range []string{"DELETE", "PATCH", "POST"} {
		path := "/admin/api/accounts/999"
		body := `{"disabled":true}`
		if method == "POST" {
			path += "/refresh"
			body = ""
		}
		if w := f.request(method, path, body, true); w.Code != http.StatusNotFound {
			t.Fatalf("missing account: %s %d", method, w.Code)
		}
	}
}

func TestAdminFlowOwnershipAndInvalidCallback(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	w := f.request("POST", "/admin/api/login-flows", `{"provider":"social","region":""}`, true)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Body)
	}
	var flow struct {
		ID    string `json:"id"`
		State struct {
			URL string `json:"url"`
		} `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &flow)
	if !strings.HasPrefix(flow.State.URL, "https://quick.aws.com/sn?") {
		t.Fatal("missing discovery URL")
	}
	if w := f.request("POST", "/admin/api/login-flows/"+flow.ID+"/callback", `{"url":"http://localhost:18080/callback?nonce=invalid"}`, true); w.Code != http.StatusBadRequest {
		t.Fatal("invalid nonce accepted")
	}
	f.login(t)
	if w := f.request("POST", "/admin/api/login-flows/"+flow.ID+"/callback", `{"url":"http://localhost/?code=invalid"}`, true); w.Code != http.StatusNotFound {
		t.Fatal("another session used private authorization flow")
	}
	if list, _ := f.st.List(context.Background()); len(list) != 0 {
		t.Fatal("invalid callback created an account")
	}
}

func TestLoginRateLimitAndExpiredSession(t *testing.T) {
	f := newFixture(t)
	for range 5 {
		if w := f.request("POST", "/admin/api/login", `{"username":"admin","password":"wrong"}`, false); w.Code != http.StatusUnauthorized {
			t.Fatal(w.Code)
		}
	}
	if w := f.request("POST", "/admin/api/login", `{"username":"admin","password":"`+testPassword+`"}`, false); w.Code != http.StatusTooManyRequests {
		t.Fatal("rate limit missing")
	}
	f.s.mu.Lock()
	f.s.attempts = make(map[string]loginAttempts)
	f.s.mu.Unlock()
	f.login(t)
	f.s.mu.Lock()
	f.s.sessions[sessionKey(f.cookie.Value)].Expires = time.Now().Add(-time.Second)
	f.s.mu.Unlock()
	if w := f.request("GET", "/admin/api/session", "", true); w.Code != http.StatusUnauthorized {
		t.Fatal("expired session accepted")
	}
}

func TestConsoleAssetsAreSelfContainedAndHardened(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/admin/", "/admin/styles.css", "/admin/app.js"} {
		w := f.request("GET", path, "", false)
		if w.Code != http.StatusOK || w.Body.Len() < 100 {
			t.Fatalf("missing asset %s", path)
		}
		if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatal("console security headers missing")
		}
	}
	if w := f.request("GET", "/admin/config.json", "", false); w.Code != http.StatusNotFound {
		t.Fatal("unexpected static file accessible")
	}
}

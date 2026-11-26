package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quickwork-openai/internal/store"
)

func transferFixture(email string) accountTransfer {
	return accountTransfer{Email: email, Provider: "social", Region: "us-east-1", BuildStage: "prod",
		TenantURL: "https://tenant.example", WSHost: "tenant.example", AccessToken: "fixture-access", IDToken: "fixture-id",
		RefreshToken: "fixture-refresh", ClientID: "fixture-client", ClientSecret: "fixture-secret", ExpiresAt: time.Now().Add(time.Hour).Unix()}
}

func encodeTransfer(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAccountTransferIsAdminAndCSRFProtected(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/admin/api/accounts/import", "/admin/api/accounts/export"} {
		if w := f.request("POST", path, `{}`, false); w.Code != 401 {
			t.Fatalf("public %s: %d", path, w.Code)
		}
	}
	f.login(t)
	for _, path := range []string{"/admin/api/accounts/import", "/admin/api/accounts/export"} {
		for _, origin := range []string{"", "https://other.example", f.s.cfg.Admin.PublicURL} {
			r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
			r.AddCookie(f.cookie)
			r.Header.Set("Origin", origin)
			if origin != f.s.cfg.Admin.PublicURL {
				r.Header.Set("X-CSRF-Token", f.csrf)
			}
			w := httptest.NewRecorder()
			f.h.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("untrusted export/import %s: %d", path, w.Code)
			}
		}
	}
}

func TestAccountTransferRoundTripDeduplicatesAndKeepsCredentialsPrivate(t *testing.T) {
	source, target := newFixture(t), newFixture(t)
	source.login(t)
	target.login(t)
	a := transferFixture("roundtrip@example.com")
	a.Disabled = true
	a.WSPath = "/ws/native"
	a.ActorARN, a.AWSAccountID = "arn:fixture", "123456789012"
	a.UserInfo = `{"userInfo":{"displayName":"Fixture"}}`
	a.IssuerURL, a.AuthEndpoint, a.TokenEndpoint = "https://oidc.example", "https://oidc.example/auth", "https://oidc.example/token"
	a.RedirectURI, a.IDCRegion = "http://127.0.0.1:18080/", "us-east-1"
	saved := a.account()
	if err := source.st.Save(context.Background(), &saved); err != nil {
		t.Fatal(err)
	}
	w := source.request("POST", "/admin/api/accounts/export", `{}`, true)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), ".json") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("export: %d %s", w.Code, w.Body)
	}
	var document accountDocument
	if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil || document.Format != accountTransferFormat || document.Version != 1 || len(document.Accounts) != 1 || document.Accounts[0] != a {
		t.Fatalf("export lost account fields: %v", err)
	}
	for attempt, want := range []string{`"added":1`, `"skipped":1`} {
		result := target.request("POST", "/admin/api/accounts/import", w.Body.String(), true)
		if result.Code != 200 || !strings.Contains(result.Body.String(), want) {
			t.Fatalf("import attempt %d: %d %s", attempt, result.Code, result.Body)
		}
		for _, secret := range []string{a.AccessToken, a.RefreshToken, a.IDToken, a.ClientSecret} {
			if strings.Contains(result.Body.String(), secret) {
				t.Fatal("import response exposed credentials")
			}
		}
	}
	list, _ := target.st.List(context.Background())
	if len(list) != 1 || !list[0].Disabled || transferFromAccount(list[0]) != a {
		t.Fatal("round trip lost credentials or enabled a disabled account")
	}
	listResponse := target.request("GET", "/admin/api/accounts", "", true)
	if strings.Contains(listResponse.Body.String(), "fixture-access") || strings.Contains(listResponse.Body.String(), "fixture-refresh") || strings.Contains(listResponse.Body.String(), "fixture-secret") {
		t.Fatal("account list exposed transfer credentials")
	}
	for _, body := range []string{`{"ids":[999]}`, `{"ids":[1,1]}`, `{"ids":[-1]}`} {
		if got := source.request("POST", "/admin/api/accounts/export", body, true); got.Code < 400 {
			t.Fatal("invalid export selection accepted")
		}
	}
}

func TestAccountImportValidatesWholeFileAndBounds(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	valid := transferFixture("first@example.com")
	bad := valid
	bad.Email = "second@example.com"
	bad.ClientSecret = ""
	for _, value := range []any{
		accountDocument{Accounts: []accountTransfer{valid, bad}},
		accountDocument{Format: "other", Accounts: []accountTransfer{valid}},
		accountDocument{Version: 2, Accounts: []accountTransfer{valid}},
		accountDocument{Accounts: make([]accountTransfer, maxTransferAccounts+1)},
	} {
		w := f.request("POST", "/admin/api/accounts/import", encodeTransfer(t, value), true)
		if w.Code != 400 || strings.Contains(w.Body.String(), valid.RefreshToken) {
			t.Fatalf("invalid transfer: %d %s", w.Code, w.Body)
		}
		list, _ := f.st.List(context.Background())
		if len(list) != 0 {
			t.Fatal("invalid transfer partially imported")
		}
	}
	for _, body := range []string{`{bad-json`, `{"accounts":[]} {}`, `{"accounts":null}`, `{"accounts":[],"unexpected":"fixture-secret"}`} {
		if w := f.request("POST", "/admin/api/accounts/import", body, true); w.Code != 400 {
			t.Fatal("malformed JSON accepted")
		}
	}
	if w := f.request("POST", "/admin/api/accounts/import", strings.Repeat(" ", maxTransferBytes+1), true); w.Code != 413 {
		t.Fatalf("oversized import: %d", w.Code)
	}
	w := f.request("POST", "/admin/api/accounts/import", "\ufeff"+encodeTransfer(t, []accountTransfer{valid}), true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"added":1`) {
		t.Fatalf("BOM array import: %d %s", w.Code, w.Body)
	}
	large := valid
	large.Email = "large@example.com"
	large.UserInfo = strings.Repeat("x", 96<<10)
	if w := f.request("POST", "/admin/api/accounts/import", encodeTransfer(t, []accountTransfer{large}), true); w.Code != 200 {
		t.Fatalf("account transfer incorrectly used the ordinary 64 KiB form limit: %d", w.Code)
	}
}

func TestAccountTransferSupportsAllLoginProviders(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	var accounts []accountTransfer
	for _, provider := range []string{"social", "enterprise", "gateway", "social_gateway"} {
		a := transferFixture(provider + "@example.com")
		a.Provider, a.TokenEndpoint = provider, "https://oidc.example/token"
		accounts = append(accounts, a)
	}
	w := f.request("POST", "/admin/api/accounts/import", encodeTransfer(t, accounts), true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"added":4`) {
		t.Fatalf("login provider rejected: %d %s", w.Code, w.Body)
	}
}

func TestAccountExportFileIsWithinImportSizeLimit(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	for i := range 2 {
		a := transferFixture(fmt.Sprintf("large-export%d@example.com", i)).account()
		a.UserInfo = strings.Repeat("x", 9<<20)
		if err := f.st.Save(context.Background(), &a); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.request("POST", "/admin/api/accounts/export", `{}`, true); w.Code != 400 {
		t.Fatalf("export exceeded the import size limit: %d", w.Code)
	}
	if w := f.request("POST", "/admin/api/accounts/export", `{"ids":[1]}`, true); w.Code != 200 {
		t.Fatalf("valid smaller export rejected: %d", w.Code)
	}
}

func TestImportedAccountCanRefreshWithTransferredOAuthSettings(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			if r.Form.Get("refresh_token") != "fixture-refresh" || r.Form.Get("client_id") != "fixture-client" || r.Form.Get("client_secret") != "fixture-secret" {
				t.Error("transferred OAuth credentials not used")
			}
			_, _ = w.Write([]byte(`{"access_token":"renewed-access","refresh_token":"renewed-refresh","id_token":"renewed-id","expires_in":3600}`))
		} else {
			_, _ = w.Write([]byte(`{"userInfo":{"email":"refresh@example.com","userArn":"arn:fixture"}}`))
		}
	}))
	defer upstream.Close()
	a := transferFixture("refresh@example.com")
	a.Provider, a.TenantURL, a.TokenEndpoint = "enterprise", upstream.URL, upstream.URL+"/token"
	a.WSHost = strings.TrimPrefix(upstream.URL, "http://")
	if w := f.request("POST", "/admin/api/accounts/import", encodeTransfer(t, []accountTransfer{a}), true); w.Code != 200 {
		t.Fatalf("import: %d %s", w.Code, w.Body)
	}
	w := f.request("POST", "/admin/api/accounts/1/refresh", "", true)
	if w.Code != 200 {
		t.Fatalf("refresh imported account: %d %s", w.Code, w.Body)
	}
	saved, _ := f.st.Get(context.Background(), 1)
	if saved.AccessToken != "renewed-access" || saved.RefreshToken != "renewed-refresh" || strings.Contains(w.Body.String(), "renewed-refresh") {
		t.Fatal("imported account refresh failed or exposed credentials")
	}
}

func TestAccountListPaginatesAndExportsAcrossPages(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	for i := range 23 {
		a := transferFixture(fmt.Sprintf("page%02d@example.com", i)).account()
		if err := f.st.Save(context.Background(), &a); err != nil {
			t.Fatal(err)
		}
	}
	w := f.request("GET", "/admin/api/accounts?page=3&page_size=10", "", true)
	var page struct {
		Accounts []accountView        `json:"accounts"`
		Total    int                  `json:"total"`
		Page     int                  `json:"page"`
		Summary  store.AccountSummary `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || page.Total != 23 || page.Page != 3 || len(page.Accounts) != 3 || page.Accounts[0].ID != 21 || page.Summary.Ready != 23 {
		t.Fatalf("list page: %d %s %v", w.Code, w.Body, err)
	}
	w = f.request("POST", "/admin/api/accounts/export", `{"ids":[1,23]}`, true)
	var exported accountDocument
	_ = json.Unmarshal(w.Body.Bytes(), &exported)
	if w.Code != 200 || len(exported.Accounts) != 2 || exported.Accounts[1].Email != "page22@example.com" {
		t.Fatal("export selection was limited to the current page")
	}
	for _, query := range []string{"page=0", "page=-1", "page=abc", "page_size=0", "page_size=101", "status=other"} {
		if got := f.request("GET", "/admin/api/accounts?"+query, "", true); got.Code != 400 {
			t.Fatalf("accepted invalid pagination: %s", query)
		}
	}
}

func TestQuotaPageSelectionDoesNotEvictOtherCachedPages(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	for i := 1; i <= 3; i++ {
		a := transferFixture(fmt.Sprintf("quota%d@example.com", i)).account()
		if err := f.st.Save(context.Background(), &a); err != nil {
			t.Fatal(err)
		}
		f.s.quotas[a.ID] = &quotaCache{value: quotaView{AccountID: a.ID, Status: "available"}, expires: time.Now().Add(time.Minute)}
	}
	w := f.request("GET", "/admin/api/quotas?account_ids=1,3", "", true)
	var result struct {
		Data []quotaView `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || len(result.Data) != 2 || result.Data[0].AccountID != 1 || result.Data[1].AccountID != 3 || len(f.s.quotas) != 3 {
		t.Fatalf("quota page: %d %s", w.Code, w.Body)
	}
	for _, ids := range []string{"", "0", "1,1", "bad"} {
		if w := f.request("GET", "/admin/api/quotas?account_ids="+ids, "", true); w.Code != 400 {
			t.Fatalf("invalid quota selection accepted: %q", ids)
		}
	}
}

package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quickwork-openai/internal/store"
)

type quotaTransport func(*http.Request) (*http.Response, error)

func (f quotaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const nativeQuotaFixture = `{"session":{"used":0,"limit":100,"resets_at":null},"monthly":{"used":4,"limit":600,"percent":0,"resets_at":1796083200},"subscription":{"subscription_type":"QUICK_LITE_FREE_TRIAL","free_trial_metadata":{"status":"ACTIVE","expiry_date":1797921307}},"access_token":"must-not-be-forwarded","billing_email":"private@example.test"}`

func saveQuotaAccount(t *testing.T, f *fixture) *store.Account {
	t.Helper()
	a := &store.Account{Email: "quota@example.test", WSHost: "quota.ai.aws.dev", Provider: "social", AccessToken: "private-access-token", IDToken: "private-id-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := f.st.Save(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func quotaResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestQuotaNativeEnvelopeAndPreciseRatios(t *testing.T) {
	encoded, _ := json.Marshal(map[string]any{"status": 200, "body_encoding": "base64", "body": base64.StdEncoding.EncodeToString([]byte(nativeQuotaFixture))})
	outer, _ := json.Marshal(map[string]any{"eventResponse": map[string]any{"invokeAgentResponse": string(encoded)}})
	v, err := parseQuota(outer)
	if err != nil {
		t.Fatal(err)
	}
	if v.Monthly == nil || v.Monthly.UsedPercent == nil || math.Abs(*v.Monthly.UsedPercent-2.0/3.0) > 1e-8 || *v.Monthly.Remaining != 596 {
		t.Fatalf("native percent was rounded to zero; expected precise ratio: %+v", v.Monthly)
	}
	if *v.Session.RemainingPercent != 100 || v.Session.ResetsAt != 0 || v.Subscription.TrialExpiresAt != 1797921307 {
		t.Fatalf("quota metadata: %+v", v)
	}
	data, _ := json.Marshal(v)
	if strings.Contains(string(data), "must-not-be-forwarded") || strings.Contains(string(data), "private@example.test") {
		t.Fatal("unexpected upstream fields escaped quota allowlist")
	}
}

func TestQuotaMissingOrUnlimitedIsNotExhausted(t *testing.T) {
	for _, raw := range []string{`{}`, `{"used":0,"limit":0,"percent":100}`, `{"used":0,"limit":-1,"percent":100}`, `{"used":"unknown","limit":100}`, `{"used":0,"limit":"unknown","percent":100}`} {
		var input any
		_ = json.Unmarshal([]byte(raw), &input)
		v := normalizeWindow(input)
		if v == nil || v.RemainingPercent != nil {
			t.Fatalf("unknown quota became numeric for %s: %+v", raw, v)
		}
	}
	v := normalizeWindow(map[string]any{"used": float64(120), "limit": float64(100), "resets_at": float64(1796083200)})
	if *v.Remaining != 0 || *v.RemainingPercent != 0 || *v.UsedPercent != 120 {
		t.Fatalf("over limit must remain visible, without negative remaining: %+v", v)
	}
}

func TestQuotaErrorsAndBodyBounds(t *testing.T) {
	for _, raw := range []string{`{"eventResponse":{"invokeAgentResponse":"{\"status\":401,\"body\":\"secret failure body\"}"}}`, `{"status":404,"body":"<html>not found</html>"}`} {
		_, err := parseQuota([]byte(raw))
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "<html>") {
			t.Fatalf("unsafe or missing native error: %v", err)
		}
	}
	for _, raw := range []string{`{bad-json`, `{"session":null,"monthly":null}`, `{"body_encoding":"base64","body":"%%%"}`} {
		if _, err := parseQuota([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid quota: %s", raw)
		}
	}
	client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
		return quotaResponse(strings.Repeat("x", (1<<20)+1), 200), nil
	})}
	if _, err := queryQuota(context.Background(), client, "", "", "", &store.Account{WSHost: "quota.ai.aws.dev", IDToken: "secret"}); err == nil {
		t.Fatal("accepted oversized upstream body")
	}
}

func TestQuotaAdminOnlyAndCSRF(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/admin/api/quotas", "/admin/api/quotas/refresh"} {
		method := "GET"
		if strings.HasSuffix(path, "/refresh") {
			method = "POST"
		}
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer test-api-key-not-an-admin-password")
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("proxy key read %s: %d", path, w.Code)
		}
	}
	f.login(t)
	r := httptest.NewRequest("POST", "/admin/api/quotas/refresh", nil)
	r.AddCookie(f.cookie)
	r.Header.Set("Origin", f.s.cfg.Admin.PublicURL)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("refresh without CSRF: %d", w.Code)
	}
	w = f.request("GET", "/admin/api/quotas", "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatalf("empty account list: %d %s", w.Code, w.Body)
	}
}

func TestQuotaCacheForceRefreshAndStaleFallback(t *testing.T) {
	f := newFixture(t)
	a := saveQuotaAccount(t, f)
	var calls atomic.Int32
	var failing atomic.Bool
	f.s.quotaHTTP.Transport = quotaTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Scheme != "https" || r.URL.Host != a.WSHost || r.URL.Path != "/integration/quick-work/qbs-event" || r.Header.Get("Authorization") != "Bearer private-id-token" {
			t.Errorf("incorrect native authentication/endpoint")
		}
		var payload struct {
			EventBody struct {
				InvokeAgent struct {
					Path  string            `json:"path"`
					Query map[string]string `json:"queryParams"`
				} `json:"invokeAgent"`
			} `json:"eventBody"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.EventBody.InvokeAgent.Path != "profile/usage" || payload.EventBody.InvokeAgent.Query["_method"] != "GET" {
			t.Errorf("invalid read-only native invocation")
		}
		if failing.Load() {
			return quotaResponse("private error contents", 503), nil
		}
		return quotaResponse(nativeQuotaFixture, 200), nil
	})
	ctx := context.Background()
	first := f.s.accountQuota(ctx, a.ID, false)
	if first.Status != "available" || first.Monthly == nil || first.AccountID != a.ID {
		t.Fatalf("initial quota: %+v", first)
	}
	_ = f.s.accountQuota(ctx, a.ID, false)
	if calls.Load() != 1 {
		t.Fatal("cache did not suppress repeated upstream reads")
	}
	failing.Store(true)
	failed := f.s.accountQuota(ctx, a.ID, true)
	if calls.Load() != 2 || failed.Status != "stale" || failed.UpdatedAt != first.UpdatedAt || *failed.Monthly.Used != 4 || strings.Contains(failed.Error, "private") {
		t.Fatalf("lost data or leaked error during failure: %+v", failed)
	}
	failing.Store(false)
	recovered := f.s.accountQuota(ctx, a.ID, true)
	if calls.Load() != 3 || recovered.Status != "available" || recovered.Error != "" {
		t.Fatalf("manual refresh did not recover: %+v", recovered)
	}
	stored, _ := f.st.Get(ctx, a.ID)
	if stored.UseCount != 0 || stored.LastUsedAt != 0 || stored.LastError != "" {
		t.Fatal("quota queries changed chat rotation statistics/status")
	}
}

func TestQuotaSingleFlightAndFailedAccountIsolation(t *testing.T) {
	f := newFixture(t)
	a := saveQuotaAccount(t, f)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	f.s.quotaHTTP.Transport = quotaTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-release
		return quotaResponse(nativeQuotaFixture, 200), nil
	})
	var wg sync.WaitGroup
	results := make([]quotaView, 8)
	wg.Add(1)
	go func() { defer wg.Done(); results[0] = f.s.accountQuota(context.Background(), a.ID, false) }()
	<-started
	for i := 1; i < len(results); i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = f.s.accountQuota(context.Background(), a.ID, false) }(i)
	}
	close(release)
	wg.Wait()
	for _, result := range results {
		if result.Status != "available" || calls.Load() != 1 {
			t.Fatalf("duplicate or incomplete metadata read: %d %+v", calls.Load(), result)
		}
	}
	broken := &store.Account{Email: "broken@example.test", WSHost: "unused", AccessToken: ""}
	if err := f.st.Save(context.Background(), broken); err != nil {
		t.Fatal(err)
	}
	f.login(t)
	w := f.request("GET", "/admin/api/quotas", "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"available"`) || !strings.Contains(w.Body.String(), `"status":"error"`) {
		t.Fatalf("one failed account broke the whole list: %d %s", w.Code, w.Body)
	}
}

func TestQuotaRejectsCredentialRedirects(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer origin.Close()
	client := newQuotaHTTP()
	client.Transport = origin.Client().Transport
	_, err := queryQuota(context.Background(), client, "", "", "", &store.Account{WSHost: strings.TrimPrefix(origin.URL, "https://"), IDToken: "secret"})
	if err == nil || targetCalls.Load() != 0 {
		t.Fatal(fmt.Sprintf("followed authenticated redirect: %v", err))
	}
}

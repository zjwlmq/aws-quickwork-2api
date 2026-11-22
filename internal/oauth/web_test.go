package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserLoginRoundTripAndReplay(t *testing.T) {
	st := testStore(t)
	var exchanges atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		writeJSON(t, w, map[string]any{"access_token": "web-access", "refresh_token": "web-refresh", "expires_in": 3600})
	})
	mux.HandleFunc("/integration/quick-work/user-info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"userInfo": map[string]any{"email": "web@example.com", "userArn": "arn:aws:quicksight:us-east-1:123:user/default/web"}})
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	b, err := NewBrowserLogin(LoginOptions{Provider: "enterprise", Region: "us-east-1", Timeout: time.Minute}, st)
	if err != nil {
		t.Fatal(err)
	}
	if b.State().Step != "discovery" {
		t.Fatal("wrong initial step")
	}
	if _, err := b.Submit(context.Background(), "http://localhost/callback?nonce=wrong"); err == nil {
		t.Fatal("invalid nonce accepted")
	}
	q := url.Values{"nonce": {b.flow.nonce}, "authEndpoint": {upstream.URL + "/authorize"}, "tokenEndpoint": {upstream.URL + "/token"}, "clientId": {"web-client"}, "tenantUrl": {upstream.URL}, "region": {"us-east-1"}}
	state, err := b.Submit(context.Background(), "http://localhost:18080/callback?"+q.Encode())
	if err != nil || state.Step != "authorize" {
		t.Fatalf("discovery failed: %s %v", state.Step, err)
	}
	if _, err := b.Submit(context.Background(), "http://localhost/?code=bad&state=wrong"); err == nil {
		t.Fatal("invalid state accepted")
	}
	callback := "http://localhost:18080/?" + url.Values{"code": {"valid-code"}, "state": {b.flow.state}}.Encode()
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			state, err := b.Submit(context.Background(), callback)
			if err != nil || state.Step != "complete" || state.AccountID <= 0 {
				t.Errorf("completion: %s %v", state.Step, err)
			}
		})
	}
	wg.Wait()
	if exchanges.Load() != 1 {
		t.Fatalf("authorization code exchanged %d times", exchanges.Load())
	}
	list, err := st.List(context.Background())
	if err != nil || len(list) != 1 || list[0].Email != "web@example.com" || list[0].RefreshToken != "web-refresh" {
		t.Fatal("web login did not persist account")
	}
}

func TestBrowserLoginExpiryAndOrdering(t *testing.T) {
	b, err := NewBrowserLogin(LoginOptions{Provider: "social"}, testStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Submit(context.Background(), "http://localhost/?code=foo&state="+b.flow.state); err == nil {
		t.Fatal("code submitted before discovery was accepted")
	}
	b.expires = time.Now().Add(-time.Second)
	if b.State().Step != "expired" {
		t.Fatal("expired flow remained active")
	}
	if _, err := b.Submit(context.Background(), "http://localhost/callback?nonce="+b.flow.nonce); err == nil {
		t.Fatal("expired flow accepted callback")
	}
}

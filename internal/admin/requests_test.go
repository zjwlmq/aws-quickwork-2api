package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/requestlog"
	"quickwork-openai/internal/store"
)

func TestRequestHistoryIsAdminOnlyAndExportsRedactedContent(t *testing.T) {
	f := newFixture(t)
	recorder, err := requestlog.New(f.st, requestlog.DefaultOptions(filepath.Join(t.TempDir(), "details")))
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	f.s.SetRequestLogs(recorder)
	handler := diagnostics.Middleware(recorder.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input any
		_ = json.NewDecoder(r.Body).Decode(&input)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		fmt.Fprint(w, `{"error":{"message":"upstream fixture failed"}}`)
	})))
	request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"find the bug"}],"api_key":"opaque-private-key"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer private-client-credential")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	id := response.Header().Get("X-Request-ID")
	for _, path := range []string{"/admin/api/requests", "/admin/api/requests/" + id, "/admin/api/requests/" + id + "/download"} {
		if w := f.request("GET", path, "", false); w.Code != 401 {
			t.Fatalf("public request details: %d", w.Code)
		}
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer test-api-key-not-an-admin-password")
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("proxy key exposed request details")
		}
	}
	f.login(t)
	w := f.request("GET", "/admin/api/requests?status=errors&search="+id, "", true)
	var result struct {
		Data  []store.RequestRecord `json:"data"`
		Total int                   `json:"total"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Total != 1 || result.Data[0].Status != "error" {
		t.Fatalf("filtered list: %d %s", w.Code, w.Body)
	}
	w = f.request("GET", "/admin/api/requests/"+id+"/download", "", true)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || !json.Valid(w.Body.Bytes()) {
		t.Fatal("request export failed")
	}
	if !strings.Contains(w.Body.String(), "find the bug") || strings.Contains(w.Body.String(), "opaque-private-key") || strings.Contains(w.Body.String(), "private-client-credential") {
		t.Fatal("details missing useful content or exposing credentials")
	}
	for _, query := range []string{"limit=2001", "offset=-1", "status=unknown"} {
		if w := f.request("GET", "/admin/api/requests?"+query, "", true); w.Code != 400 {
			t.Fatal("unbounded request query accepted")
		}
	}
	if w := f.request("GET", "/admin/api/requests/not-found", "", true); w.Code != 404 {
		t.Fatal("unknown request did not return 404")
	}
}

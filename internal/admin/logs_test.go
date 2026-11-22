package admin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"quickwork-openai/internal/diagnostics"
)

func TestDiagnosticsRequiresAdminAndExportsMatchingTrace(t *testing.T) {
	l, err := diagnostics.Open(diagnostics.Options{File: filepath.Join(t.TempDir(), "events.jsonl"), Level: "debug", MaxMB: 1, Backups: 1})
	if err != nil {
		t.Fatal(err)
	}
	old := diagnostics.SetDefault(l)
	defer func() { diagnostics.SetDefault(old); l.Close() }()
	f := newFixture(t)
	if w := f.request("GET", "/admin/api/logs", "", false); w.Code != 401 {
		t.Fatal("logs exposed without admin session")
	}
	f.login(t)
	ctx := diagnostics.With(context.Background(), "request_id", "req_admin_fixture")
	diagnostics.Event(ctx, "error", "upstream.fixture", "error_message", "Bearer secret-for-log-test")
	diagnostics.Event(context.Background(), "info", "unrelated.fixture")
	w := f.request("GET", "/admin/api/logs?request_id=req_admin_fixture&level=error", "", true)
	var result diagnostics.Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Entries) != 1 {
		t.Fatalf("filter: %d %s", w.Code, w.Body)
	}
	w = f.request("GET", "/admin/api/logs?request_id=req_admin_fixture&download=1", "", true)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || !json.Valid([]byte(strings.TrimSpace(w.Body.String()))) {
		t.Fatalf("export failed: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "secret-for-log-test") || strings.Contains(w.Body.String(), "unrelated.fixture") {
		t.Fatal("export ignored redaction or filter")
	}
	compressedRequest := httptest.NewRequest("GET", "/admin/api/logs?request_id=req_admin_fixture", nil)
	compressedRequest.AddCookie(f.cookie)
	compressedRequest.Header.Set("Accept-Encoding", "gzip, deflate")
	compressedResponse := httptest.NewRecorder()
	f.h.ServeHTTP(compressedResponse, compressedRequest)
	if compressedResponse.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("large log responses are not compressed")
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressedResponse.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if json.NewDecoder(reader).Decode(&result) != nil || len(result.Entries) != 1 {
		t.Fatal("compressed log response lost its content")
	}
	if acceptsGzip("gzip;q=0, br") || acceptsGzip("gzip;q=invalid") {
		t.Fatal("client compression opt-out ignored")
	}
	for _, q := range []string{"limit=-1", "limit=5001", "level=verbose"} {
		if w := f.request("GET", "/admin/api/logs?"+q, "", true); w.Code != 400 {
			t.Fatal("unbounded query accepted")
		}
	}
}

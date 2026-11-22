package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func testLogger(t *testing.T) (*Logger, *bytes.Buffer) {
	t.Helper()
	output := new(bytes.Buffer)
	l, err := Open(Options{Level: "debug", File: filepath.Join(t.TempDir(), "logs", "events.jsonl"), MaxMB: 1, Backups: 2, Stdout: output})
	if err != nil {
		t.Fatal(err)
	}
	old := SetDefault(l)
	t.Cleanup(func() { SetDefault(old); _ = l.Close() })
	return l, output
}

func TestRedactsNestedSecretsAndNeverLogsRawBodies(t *testing.T) {
	l, output := testLogger(t)
	secret := "opaque-upstream-credential-ABC123"
	RegisterSecrets(secret)
	ctx := With(context.Background(), "request_id", "req_redaction")
	Event(ctx, "error", "test.redaction", "error", fmt.Errorf(`HTTP failed Bearer bearer-credential; access_token="access-123" refresh_token=refresh-123 https://files.example/video?expires=123&signature=signature-123 %s`, secret), "nested", map[string]any{"api_key": "arbitrary-key", "cookie": "session-value", "payload": map[string]any{"anything": "raw-user-code"}, "safe": "visible"}, "media_source", "data:image/png;base64,c2VjcmV0", "json_secret", `{"password":"password-123"}`)
	for _, value := range []string{secret, "bearer-credential", "access-123", "refresh-123", "signature-123", "arbitrary-key", "session-value", "raw-user-code", "c2VjcmV0", "password-123"} {
		if strings.Contains(output.String(), value) {
			t.Errorf("secret leaked: %q", value)
		}
	}
	result, err := l.Query(Filter{RequestID: "req_redaction"})
	if err != nil || len(result.Entries) != 1 {
		t.Fatalf("query: %+v %v", result, err)
	}
	if !strings.Contains(output.String(), "visible") {
		t.Fatal("safe diagnosis metadata removed")
	}
}

func TestRotationQueryAndConcurrentWrites(t *testing.T) {
	l, _ := testLogger(t)
	l.maxBytes = 1800
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				Event(With(context.Background(), "request_id", fmt.Sprintf("req_%d", worker)), "warn", "test.concurrent", "index", i, "padding", strings.Repeat("a", 60))
			}
		}()
	}
	wg.Wait()
	for i := range 4 {
		Event(With(context.Background(), "request_id", "req_target"), "error", "test.target", "index", i)
	}
	result, err := l.Query(Filter{RequestID: "req_target", Level: "error", Search: "target", Limit: 10})
	if err != nil || len(result.Entries) != 4 {
		t.Fatalf("tail query: %d %v", len(result.Entries), err)
	}
	if result.Entries[0]["index"] != float64(3) || result.Entries[3]["index"] != float64(0) {
		t.Fatal("results not newest first across chunks")
	}
	for _, p := range []string{l.path, l.path + ".1", l.path + ".2"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > l.maxBytes {
			t.Fatal("rotation size unbounded")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe mode: %v", info.Mode())
		}
		data, _ := os.ReadFile(p)
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			if !json.Valid(line) {
				t.Fatal("concurrent writes corrupted JSON")
			}
		}
	}
	if _, err := os.Stat(l.path + ".3"); !os.IsNotExist(err) {
		t.Fatal("retention not enforced")
	}
}

func TestTailAcross64KiBChunksAndPartialLine(t *testing.T) {
	l, _ := testLogger(t)
	for i := range 80 {
		Event(With(context.Background(), "request_id", "req_chunk"), "debug", "test.chunk", "index", i, "padding", strings.Repeat("x", 1800))
	}
	result, err := l.Query(Filter{RequestID: "req_chunk", Limit: 100})
	if err != nil || len(result.Entries) != 80 {
		t.Fatalf("chunk boundaries dropped events: %d %v", len(result.Entries), err)
	}
	for i, entry := range result.Entries {
		if entry["index"] != float64(79-i) {
			t.Fatalf("index %d: %+v", i, entry)
		}
	}
}

func TestHTTPTraceSSEAndBackgroundCorrelation(t *testing.T) {
	l, output := testLogger(t)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestID(r.Context())
		if id == "" {
			t.Error("missing request ID")
		}
		child := context.WithoutCancel(r.Context())
		Event(With(child, "job_id", "video-fixture"), "info", "video.fixture")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: private-model-response\n\n")
		w.(http.Flusher).Flush()
		Event(r.Context(), "error", "chat.response.stream_error", "error_message", "upstream fixture failure")
	}))
	r := httptest.NewRequest("POST", "https://console.example/v1/messages?signature=do-not-record", strings.NewReader("private-request-body"))
	r.Header.Set("Authorization", "Bearer client-secret")
	r.Header.Set("X-Request-ID", "attacker-chosen-id")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	id := w.Header().Get("X-Request-ID")
	if id == "" || id == "attacker-chosen-id" || w.Header().Get("request-id") != id {
		t.Fatal("response IDs not server-generated or inconsistent")
	}
	result, err := l.Query(Filter{RequestID: id})
	if err != nil || len(result.Entries) != 4 {
		t.Fatalf("trace: %+v %v", result, err)
	}
	completed := result.Entries[0]
	if completed["status"] != float64(200) || completed["error_events"] != float64(1) || completed["response_bytes"].(float64) <= 0 {
		t.Fatalf("SSE error invisible in access summary: %+v", completed)
	}
	for _, value := range []string{"private-model-response", "private-request-body", "client-secret", "do-not-record", "attacker-chosen-id"} {
		if strings.Contains(output.String(), value) {
			t.Fatalf("request/response content leaked: %s", value)
		}
	}
	if !w.Flushed {
		t.Fatal("middleware broke SSE flushing")
	}
}

func TestHTTPValidationErrorRedactedAndQueryQuiet(t *testing.T) {
	l, _ := testLogger(t)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"access_token=unsafe-token","type":"authentication_error"}}`)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", nil))
	result, _ := l.Query(Filter{RequestID: w.Header().Get("X-Request-ID")})
	if len(result.Entries) != 2 || result.Entries[0]["level"] != "WARN" {
		t.Fatal("HTTP errors not captured")
	}
	data, _ := json.Marshal(result)
	if bytes.Contains(data, []byte("unsafe-token")) {
		t.Fatal("validation error leaked secret")
	}
	quiet := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	quiet.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/admin/api/logs", nil))
	after, _ := l.Query(Filter{})
	if len(after.Entries) != len(result.Entries) {
		t.Fatal("viewer recursively logged its own reads")
	}
}

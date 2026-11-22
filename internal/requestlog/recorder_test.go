package requestlog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

func fixture(t *testing.T, edit func(*Options)) (*Recorder, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions(filepath.Join(dir, "details"))
	if edit != nil {
		edit(&opts)
	}
	r, err := New(st, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); st.Close() })
	return r, st
}

func invoke(r *Recorder, h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "sk-test-private-request-secret")
	req.Header.Set("User-Agent", "claude-code/test")
	w := httptest.NewRecorder()
	diagnostics.Middleware(r.Middleware(h)).ServeHTTP(w, req)
	return w
}

func bodyHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input any
		_ = json.NewDecoder(r.Body).Decode(&input)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":{"message":"fixture failure","type":"api_error"}}`)
	})
}

func TestRecoveredSSEActualUsageRedactionAndDiskPersistence(t *testing.T) {
	r, st := fixture(t, nil)
	l, err := diagnostics.Open(diagnostics.Options{Level: "off"})
	if err != nil {
		t.Fatal(err)
	}
	old := diagnostics.SetDefault(l)
	defer diagnostics.SetDefault(old)
	diagnostics.RegisterSecrets("opaque-upstream-secret-123456")
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var input any
		_ = json.NewDecoder(req.Body).Decode(&input)
		ctx := req.Context()
		diagnostics.Event(ctx, "info", "anthropic.request.converted", "requested_model", "smart", "stream", true, "requested_max_tokens", 123)
		one := diagnostics.With(ctx, "attempt", 1, "account_id", int64(1))
		diagnostics.Event(one, "info", "chat.attempt.started")
		diagnostics.Event(one, "warn", "upstream.socket.read_failed", "saw_message_complete", false, "partial_accepted", false, "error", fmt.Errorf("connection interrupted before output"))
		diagnostics.Event(one, "error", "upstream.turn.finished", "error", fmt.Errorf("Bearer opaque-upstream-secret-123456"), "upstream_usage", map[string]any{"input_tokens": 4, "output_tokens": 1, "total_tokens": 5})
		diagnostics.Event(one, "warn", "account.upstream_failed", "error", fmt.Errorf("fixture upstream failed"))
		two := diagnostics.With(ctx, "attempt", 2, "account_id", int64(2))
		diagnostics.Event(two, "info", "chat.attempt.started")
		diagnostics.Event(two, "info", "account.selected")
		diagnostics.Event(two, "info", "upstream.turn.finished", "upstream_usage", map[string]any{"input_tokens": 1000, "cache_read_tokens": 200, "cache_write_tokens": 50, "output_tokens": 10, "total_tokens": 1260})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"answer text\"}}\n\n")
		w.(http.Flusher).Flush()
		diagnostics.Event(ctx, "info", "chat.response.completed", "finish_reason", "stop")
	})
	input := `{"model":"smart","stream":true,"max_tokens":123,"metadata":{"user_id":"user-account-session-123"},"system":"Preserve this project rule","messages":[{"role":"user","content":"fix code api_key=secret-argument-123 AWS_SECRET_ACCESS_KEY=unknown-secret-key https://private-user:private-password@example.com/file"}],"OPENAI_API_KEY":"opaque-input-key-123","image":{"source":{"type":"base64","data":"BINARY_IMAGE_BYTES"}}}`
	w := invoke(r, h, input)
	if !w.Flushed {
		t.Fatal("SSE flushing broken")
	}
	id := w.Header().Get("X-Request-ID")
	row, err := st.GetRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "recovered" || row.RetryCount != 1 || row.AccountID != 2 || row.InputTokens != 1254 || row.OutputTokens != 11 || row.CacheReadTokens != 200 || row.CacheWriteTokens != 50 || row.MaxTokens != 123 || !row.ErrorDetail {
		t.Fatalf("incorrect recovered summary: %+v", row)
	}
	detail, err := r.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(detail)
	for _, secret := range []string{"sk-test-private-request-secret", "opaque-upstream-secret-123456", "opaque-input-key-123", "secret-argument-123", "BINARY_IMAGE_BYTES", "user-account-session-123", "unknown-secret-key", "private-password", "private-user"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("detail leaked secret: %s", secret)
		}
	}
	if !strings.Contains(string(b), "Preserve this project rule") || !strings.Contains(string(b), "answer text") {
		t.Fatal("redaction removed useful request/response content")
	}
	r.Close()
	restarted, err := New(st, r.options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	detail, err = restarted.Get(context.Background(), id)
	if err != nil || detail["storage"] != "disk" {
		t.Fatalf("error detail lost after restart: %v %v", detail, err)
	}
}

func TestSuccessBodyIsMemoryOnlyAndSummarySurvivesRestart(t *testing.T) {
	r, st := fixture(t, nil)
	w := invoke(r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var v any
		_ = json.NewDecoder(req.Body).Decode(&v)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"SUCCESS_BODY"}]}`)
	}), `{"model":"smart","messages":[{"role":"user","content":"SUCCESS_INPUT"}]}`)
	id := w.Header().Get("X-Request-ID")
	detail, err := r.Get(context.Background(), id)
	if err != nil || detail["storage"] != "memory" {
		t.Fatal("fresh success detail unavailable")
	}
	files, _ := os.ReadDir(r.options.Directory)
	if len(files) != 0 {
		t.Fatal("successful bodies written to disk")
	}
	r.Close()
	restarted, err := New(st, r.options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	detail, err = restarted.Get(context.Background(), id)
	if err != nil || detail["storage"] != "summary" {
		t.Fatal("summary lost or success body persisted across restart")
	}
	b, _ := json.Marshal(detail)
	if strings.Contains(string(b), "SUCCESS_BODY") || strings.Contains(string(b), "SUCCESS_INPUT") {
		t.Fatal("success body survived on disk")
	}
}

func TestHTTP200StreamingErrorIsFailed(t *testing.T) {
	r, st := fixture(t, nil)
	w := invoke(r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"native failed\"}}\n\n")
		w.(http.Flusher).Flush()
	}), `{}`)
	row, err := st.GetRequest(context.Background(), w.Header().Get("X-Request-ID"))
	if err != nil || row.Status != "error" || row.HTTPStatus != 200 || row.Error != "native failed" || row.RetryCount != 0 {
		t.Fatalf("stream error mislabeled: %+v %v", row, err)
	}
}

func TestClientCancellationIsRecordedSeparately(t *testing.T) {
	r, st := fixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`)).WithContext(ctx)
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cancel()
		diagnostics.Event(req.Context(), "error", "chat.response.stream_error", "error_message", "context cancelled")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
	})
	w := httptest.NewRecorder()
	diagnostics.Middleware(r.Middleware(h)).ServeHTTP(w, req)
	row, err := st.GetRequest(context.Background(), w.Header().Get("X-Request-ID"))
	if err != nil || row.Status != "cancelled" {
		t.Fatalf("client cancellation hidden as upstream error: %+v %v", row, err)
	}
}

func TestRetentionBoundsAgeCountDiskAndMemory(t *testing.T) {
	r, st := fixture(t, func(o *Options) { o.MaxRows = 3; o.MaxFiles = 2; o.MaxMemoryBytes = 1 })
	var ids []string
	for range 5 {
		w := invoke(r, bodyHandler(502), `{"model":"smart"}`)
		ids = append(ids, w.Header().Get("X-Request-ID"))
	}
	rows, total, err := st.ListRequests(context.Background(), store.RequestFilter{Limit: 10})
	if err != nil || total != 3 || len(rows) != 3 {
		t.Fatalf("summary retention: %d %v", total, err)
	}
	files, _ := os.ReadDir(r.options.Directory)
	if len(files) != 2 {
		t.Fatalf("error-file retention: %d", len(files))
	}
	detail, err := r.Get(context.Background(), rows[0].ID)
	if err != nil || detail["storage"] != "disk" {
		t.Fatal("memory budget should evict bodies while disk details remain")
	}
	for _, file := range files {
		old := time.Now().Add(-8 * 24 * time.Hour)
		if err := os.Chtimes(filepath.Join(r.options.Directory, file.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.prune(); err != nil {
		t.Fatal(err)
	}
	files, _ = os.ReadDir(r.options.Directory)
	if len(files) != 0 {
		t.Fatal("expired details not removed")
	}
	w := invoke(r, bodyHandler(502), `{"model":"smart"}`)
	r.options.MaxDiskBytes = 1
	if err := r.prune(); err != nil {
		t.Fatal(err)
	}
	detail, err = r.Get(context.Background(), w.Header().Get("X-Request-ID"))
	if err != nil || detail["storage"] != "summary" {
		t.Fatal("disk budget not enforced")
	}
}

func TestAsyncVideoUpdatesOriginalRequest(t *testing.T) {
	r, st := fixture(t, nil)
	var background context.Context
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var input any
		_ = json.NewDecoder(req.Body).Decode(&input)
		background = context.WithoutCancel(req.Context())
		diagnostics.Event(req.Context(), "info", "video.job.queued", "job_id", "video-fixture", "model", "smart")
		w.WriteHeader(202)
		fmt.Fprint(w, `{"id":"video-fixture","status":"queued"}`)
	})
	req := httptest.NewRequest("POST", "/v1/videos", strings.NewReader(`{"prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	diagnostics.Middleware(r.Middleware(h)).ServeHTTP(w, req)
	id := w.Header().Get("X-Request-ID")
	row, _ := st.GetRequest(context.Background(), id)
	if row.Status != "queued" {
		t.Fatal("async job not recorded")
	}
	diagnostics.Event(background, "info", "video.job.started")
	diagnostics.Event(background, "error", "video.job.finished", "job_id", "video-fixture", "status", "failed", "error", fmt.Errorf("renderer failed"))
	row, err := st.GetRequest(context.Background(), id)
	if err != nil || row.Status != "error" || row.JobID != "video-fixture" || !row.ErrorDetail {
		t.Fatalf("background failure not correlated: %+v %v", row, err)
	}
}

func TestPrunedErrorBodyCannotSurviveInMemory(t *testing.T) {
	r, _ := fixture(t, func(o *Options) { o.MaxFiles = 1 })
	first := invoke(r, bodyHandler(502), `{"model":"smart","messages":[{"role":"user","content":"OLD_ERROR_CONTENT"}]}`)
	invoke(r, bodyHandler(502), `{"model":"smart"}`)
	detail, err := r.Get(context.Background(), first.Header().Get("X-Request-ID"))
	if err != nil || detail["storage"] != "summary" {
		t.Fatal("error detail count cap was bypassed by memory cache")
	}
	b, _ := json.Marshal(detail)
	if strings.Contains(string(b), "OLD_ERROR_CONTENT") {
		t.Fatal("deleted error body still accessible")
	}
}

func TestLargeAndMultipartBodiesAreExplicitlyOmitted(t *testing.T) {
	c := &capture{}
	_, _ = c.Write([]byte(strings.Repeat("x", bodyLimit+100)))
	snapshot := c.snapshot("application/json")
	if snapshot["body"] != nil || snapshot["truncated"] != true {
		t.Fatal("oversized body silently exposed or reported complete")
	}
	c = &capture{}
	_, _ = c.Write([]byte("private binary input"))
	snapshot = c.snapshot("multipart/form-data; boundary=x")
	if snapshot["body"] != nil || snapshot["body_text"] != nil {
		t.Fatal("multipart binary recorded")
	}
}

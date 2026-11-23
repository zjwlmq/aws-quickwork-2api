package requestlog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quickwork-openai/internal/diagnostics"
)

func TestServerTimeoutRemainsErrorWhenClientAlsoCloses(t *testing.T) {
	recorder, st := fixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		diagnostics.Event(req.Context(), "error", "chat.request.timed_out", "timeout_ms", 300000, "error", context.DeadlineExceeded)
		cancel()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"error\":{\"code\":\"upstream_timeout\",\"message\":\"Quick request total timeout\"}}\n\ndata: [DONE]\n\n")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"smart","stream":true}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	diagnostics.Middleware(recorder.Middleware(h)).ServeHTTP(w, req)
	row, err := st.GetRequest(context.Background(), w.Header().Get("X-Request-ID"))
	if err != nil || row.Status != "error" || !strings.Contains(row.Error, "timeout") {
		t.Fatalf("server timeout mislabeled as client cancellation: %+v %v", row, err)
	}
	detail, err := recorder.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail["storage"] != "disk" {
		t.Fatal("timeout detail was not persisted")
	}
}

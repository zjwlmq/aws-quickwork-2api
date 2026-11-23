package requestlog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"quickwork-openai/internal/diagnostics"
)

func TestUpstreamFailurePrecedenceOverClientClosingAtError(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelFirst), func(t *testing.T) {
			recorder, st := fixture(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_, _ = io.Copy(io.Discard, req.Body)
				if cancelFirst {
					cancel()
				}
				diagnostics.Event(req.Context(), "error", "chat.response.stream_error", "error_message", "generated artifact download HTTP 403")
				cancel()
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"error\":{\"code\":\"upstream_error\",\"message\":\"generated artifact download HTTP 403\"}}\n\ndata: [DONE]\n\n")
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"smart","stream":true}`)).WithContext(ctx)
			w := httptest.NewRecorder()
			diagnostics.Middleware(recorder.Middleware(h)).ServeHTTP(w, req)
			row, err := st.GetRequest(context.Background(), w.Header().Get("X-Request-ID"))
			want := "error"
			if cancelFirst {
				want = "cancelled"
			}
			if err != nil || row.Status != want || !strings.Contains(row.Error, "403") {
				t.Fatalf("wrong failure classification: %+v %v", row, err)
			}
		})
	}
}

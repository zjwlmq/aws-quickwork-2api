package requestlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quickwork-openai/internal/diagnostics"
)

const openAIFinish = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
const openAIDone = "data: [DONE]\n\n"
const anthropicFinish = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"
const anthropicDone = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestSSEClientClosesAfterCompletionRemainsSuccessful(t *testing.T) {
	r, st := fixture(t, nil)
	logged := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var input any
		_ = json.NewDecoder(req.Body).Decode(&input)
		diagnostics.Event(req.Context(), "info", "chat.request.validated", "requested_model", "smart", "stream", true)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, openAIFinish+openAIDone)
		w.(http.Flusher).Flush()
		// Reproduce an SDK closing immediately at [DONE], while the handler is
		// still finishing session/log bookkeeping rather than writing more data.
		select {
		case <-req.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("client close did not cancel the transport")
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		diagnostics.Middleware(r.Middleware(h)).ServeHTTP(w, req)
		close(logged)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"smart","stream":true}`))
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	id := response.Header.Get("X-Request-ID")
	scanner := bufio.NewScanner(response.Body)
	sawDone := false
	for scanner.Scan() {
		if scanner.Text() == "data: [DONE]" {
			sawDone = true
			break
		}
	}
	cancel()
	response.Body.Close()
	if !sawDone {
		t.Fatal("missing response completion")
	}
	select {
	case <-logged:
	case <-time.After(4 * time.Second):
		t.Fatal("request logging did not finish")
	}
	row, err := st.GetRequest(context.Background(), id)
	if err != nil || row.Status != "success" || row.Error != "" || row.FinishReason != "tool_calls" {
		t.Fatalf("completed stream mislabeled: %+v %v", row, err)
	}
}

func TestSSECompletionDistinguishesLateCloseFromInterruptedOutput(t *testing.T) {
	for _, test := range []struct {
		name, path, frames, want string
		cancelBefore             bool
	}{
		{"openai_complete", "/v1/chat/completions", openAIFinish + openAIDone, "success", false},
		{"anthropic_complete", "/v1/messages", anthropicFinish + anthropicDone, "success", false},
		{"openai_missing_done", "/v1/chat/completions", openAIFinish, "cancelled", false},
		{"openai_missing_finish", "/v1/chat/completions", openAIDone, "cancelled", false},
		{"anthropic_missing_stop", "/v1/messages", anthropicFinish, "cancelled", false},
		{"anthropic_missing_reason", "/v1/messages", anthropicDone, "cancelled", false},
		{"cancel_before_output", "/v1/chat/completions", openAIFinish + openAIDone, "cancelled", true},
		{"error_followed_by_done", "/v1/chat/completions", "data: {\"error\":{\"message\":\"failed\"}}\n\n" + openAIFinish + openAIDone, "cancelled", false},
		{"large_output", "/v1/chat/completions", "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 600<<10) + "\"}}]}\n\n" + openAIFinish + openAIDone, "success", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, st := fixture(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("POST", test.path, strings.NewReader(`{"model":"smart","stream":true}`)).WithContext(ctx)
			h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				diagnostics.Event(req.Context(), "info", "chat.response.completed", "finish_reason", "tool_calls")
				if test.cancelBefore {
					cancel()
				}
				// Fragment event names, JSON and completion markers across writes.
				for start := 0; start < len(test.frames); start += 17 {
					fmt.Fprint(w, test.frames[start:min(start+17, len(test.frames))])
				}
				cancel()
			})
			w := httptest.NewRecorder()
			diagnostics.Middleware(r.Middleware(h)).ServeHTTP(w, req)
			row, err := st.GetRequest(context.Background(), w.Header().Get("X-Request-ID"))
			if err != nil || row.Status != test.want {
				t.Fatalf("got %+v %v; want %s", row, err, test.want)
			}
		})
	}
}

type completedWriteError struct{ *httptest.ResponseRecorder }

func (w *completedWriteError) Write(b []byte) (int, error) {
	n, _ := w.ResponseRecorder.Write(b)
	return n, errors.New("fixture response write failed")
}

func TestCompletedFramesWithWriteErrorAreNotSuccessful(t *testing.T) {
	r, st := fixture(t, nil)
	w := &completedWriteError{httptest.NewRecorder()}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, openAIFinish+openAIDone)
	})
	diagnostics.Middleware(r.Middleware(h)).ServeHTTP(w, req)
	row, err := st.GetRequest(context.Background(), w.Header().Get("X-Request-ID"))
	if err != nil || row.Status != "cancelled" {
		t.Fatalf("failed write mislabeled: %+v %v", row, err)
	}
}

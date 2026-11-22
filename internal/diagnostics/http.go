package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
)

type responseWriter struct {
	http.ResponseWriter
	status     int
	bytes      int64
	first      time.Time
	errorBody  bytes.Buffer
	writeError error
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) WriteHeader(status int) {
	if status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.first = time.Now()
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if w.status >= 400 && w.errorBody.Len() < 8192 {
		_, _ = w.errorBody.Write(b[:min(len(b), 8192-w.errorBody.Len())])
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	if err != nil {
		w.writeError = err
	}
	return n, err
}

type flushWriter struct{ *responseWriter }

func (w *flushWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	w.ResponseWriter.(http.Flusher).Flush()
}

// Middleware wraps the complete mux so admin and API errors share the same ID.
// Query strings, cookies, credentials, successful bodies and media are excluded.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestID(r.Context()) != "" {
			next.ServeHTTP(w, r)
			return
		}
		id := "req_" + ID()
		ctx := With(r.Context(), "request_id", id)
		state := &requestState{}
		ctx = context.WithValue(ctx, requestKey{}, state)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("request-id", id)
		started := time.Now()
		capture := &responseWriter{ResponseWriter: w}
		var writer http.ResponseWriter = capture
		if _, ok := w.(http.Flusher); ok {
			writer = &flushWriter{capture}
		}
		quiet := r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/admin/api/logs") || strings.HasPrefix(r.URL.Path, "/admin/api/requests") || (strings.HasPrefix(r.URL.Path, "/admin/") && !strings.HasPrefix(r.URL.Path, "/admin/api/"))
		if !quiet {
			peer, _, _ := net.SplitHostPort(r.RemoteAddr)
			Event(ctx, "info", "http.request.started", "method", r.Method, "path", r.URL.Path, "content_length", r.ContentLength, "content_type", r.Header.Get("Content-Type"), "peer_ip", peer, "user_agent", r.UserAgent(), "anthropic_version", r.Header.Get("Anthropic-Version"))
		}
		defer func() {
			panicked := recover()
			if capture.status == 0 && panicked == nil {
				capture.status = http.StatusOK
			}
			level := "info"
			if capture.status >= 500 || capture.writeError != nil || panicked != nil {
				level = "error"
			} else if capture.status >= 400 {
				level = "warn"
			}
			if !quiet || level != "info" {
				var errorValue any
				if capture.errorBody.Len() > 0 {
					var obj map[string]any
					if json.Unmarshal(capture.errorBody.Bytes(), &obj) == nil {
						errorValue = obj["error"]
					}
				}
				firstMS := int64(-1)
				if !capture.first.IsZero() {
					firstMS = capture.first.Sub(started).Milliseconds()
				}
				Event(ctx, level, "http.request.completed", "method", r.Method, "path", r.URL.Path, "status", capture.status, "duration_ms", time.Since(started).Milliseconds(), "first_byte_ms", firstMS, "response_bytes", capture.bytes, "error_detail", errorValue, "write_error", capture.writeError, "client_cancelled", r.Context().Err() != nil, "error_events", state.errors.Load(), "warning_events", state.warnings.Load(), "panicked", panicked != nil)
			}
			if panicked != nil {
				panic(panicked)
			}
		}()
		next.ServeHTTP(writer, r)
	})
}

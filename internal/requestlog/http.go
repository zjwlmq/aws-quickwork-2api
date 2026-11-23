package requestlog

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

type writer struct {
	http.ResponseWriter
	status      int
	firstByteMS int64
	started     time.Time
	body        capture
	writeError  error
	ctx         context.Context
	completion  streamCompletion
}

func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *writer) WriteHeader(code int) {
	if code < 200 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = code
	w.firstByteMS = time.Since(w.started).Milliseconds()
	w.ResponseWriter.WriteHeader(code)
}
func (w *writer) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	// Observe cancellation before the write: a client may read [DONE] and close
	// while Write returns. That is different from writing after it already left.
	active := w.ctx == nil || w.ctx.Err() == nil
	n, err := w.ResponseWriter.Write(b)
	_, _ = w.body.Write(b[:n])
	if err != nil {
		w.writeError = err
	} else if n != len(b) {
		w.writeError = io.ErrShortWrite
	}
	if strings.HasPrefix(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream") {
		w.completion.write(b[:n], active && err == nil && n == len(b))
	}
	return n, err
}

type flushingWriter struct{ *writer }

func (w *flushingWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	w.ResponseWriter.(http.Flusher).Flush()
}

func protocol(r *http.Request) string {
	if r.Method != "POST" {
		return ""
	}
	switch r.URL.Path {
	case "/v1/messages":
		return "anthropic_messages"
	case "/v1/chat/completions":
		return "openai_chat"
	case "/v1/videos", "/admin/api/videos":
		return "video"
	}
	return ""
}

func (r *Recorder) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		kind := protocol(req)
		if kind == "" {
			next.ServeHTTP(w, req)
			return
		}
		started := time.Now()
		id := diagnostics.RequestID(req.Context())
		if id == "" {
			id = "req_" + diagnostics.ID()
			req = req.WithContext(diagnostics.With(req.Context(), "request_id", id))
			w.Header().Set("X-Request-ID", id)
			w.Header().Set("request-id", id)
		}
		row := store.RequestRecord{ID: id, StartedAt: started.UnixMilli(), UpdatedAt: started.UnixMilli(), Status: "running", Method: req.Method, Path: req.URL.Path, Protocol: kind, KeyID: keyID(req), Client: diagnostics.RedactText(req.UserAgent(), 160), FirstByteMS: -1}
		if session := req.Header.Get("X-Thread-Id"); session != "" {
			row.SessionID = "session_" + diagnostics.Fingerprint(session)[:12]
		}
		t := &trace{recorder: r, record: row, started: started, attempts: map[int]*attempt{}, failedAccounts: map[int64]bool{}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.store.SaveRequest(ctx, row); err != nil {
			r.report("requestlog.summary_save_failed", err)
		}
		cancel()
		req = req.WithContext(diagnostics.WithObserver(req.Context(), t))
		input := &capture{}
		if req.Body != nil {
			req.Body = &readCloser{Reader: io.TeeReader(req.Body, input), Closer: req.Body}
		}
		captureWriter := &writer{ResponseWriter: w, started: started, firstByteMS: -1, ctx: req.Context(), completion: streamCompletion{protocol: kind}}
		var wrapped http.ResponseWriter = captureWriter
		if _, ok := w.(http.Flusher); ok {
			wrapped = &flushingWriter{captureWriter}
		}
		defer func() {
			panicked := recover()
			if captureWriter.status == 0 {
				captureWriter.status = 200
				if panicked != nil {
					captureWriter.status = 500
				}
			}
			t.end(req, captureWriter, input, panicked != nil)
			if panicked != nil {
				panic(panicked)
			}
		}()
		next.ServeHTTP(wrapped, req)
	})
}

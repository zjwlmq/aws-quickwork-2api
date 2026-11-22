package admin

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"quickwork-openai/internal/diagnostics"
)

func (s *Server) logs(w http.ResponseWriter, r *http.Request, _ *session) {
	l := diagnostics.Default()
	if l == nil {
		fail(w, 503, "诊断日志尚未启用")
		return
	}
	q := r.URL.Query()
	limit := 300
	if q.Get("limit") != "" {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > 5000 {
			fail(w, 400, "limit 必须为 1 到 5000")
			return
		}
		limit = n
	}
	level := strings.ToLower(q.Get("level"))
	switch level {
	case "", "debug", "info", "warn", "error":
	default:
		fail(w, 400, "日志级别无效")
		return
	}
	if len(q.Get("request_id")) > 128 || len(q.Get("search")) > 256 {
		fail(w, 400, "筛选条件过长")
		return
	}
	result, err := l.Query(diagnostics.Filter{RequestID: q.Get("request_id"), Level: level, Search: q.Get("search"), Limit: limit})
	if err != nil {
		diagnostics.Event(r.Context(), "error", "logs.query.failed", "error", err)
		fail(w, 500, "日志读取失败")
		return
	}
	w.Header().Add("Vary", "Accept-Encoding")
	var output io.Writer = w
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		defer compressed.Close()
		output = compressed
	}
	if q.Get("download") == "1" {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="quickwork-diagnostics.jsonl"`)
		w.Header().Set("X-Log-Truncated", strconv.FormatBool(result.Truncated))
		enc := json.NewEncoder(output)
		for i := len(result.Entries) - 1; i >= 0; i-- {
			if enc.Encode(result.Entries[i]) != nil {
				return
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(output).Encode(result)
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		values := strings.Split(part, ";")
		if !strings.EqualFold(strings.TrimSpace(values[0]), "gzip") {
			continue
		}
		for _, parameter := range values[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && strings.EqualFold(key, "q") {
				q, err := strconv.ParseFloat(value, 64)
				return err == nil && q > 0 && q <= 1
			}
		}
		return true
	}
	return false
}

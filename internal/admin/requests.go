package admin

import (
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/requestlog"
	"quickwork-openai/internal/store"
)

func (s *Server) SetRequestLogs(recorder *requestlog.Recorder) { s.requests = recorder }

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request, _ *session) {
	if s.requests == nil {
		fail(w, 503, "请求日志尚未启用")
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	switch status {
	case "", "success", "error", "errors", "active", "recovered", "cancelled", "interrupted", "running", "queued", "in_progress":
	default:
		fail(w, 400, "请求状态无效")
		return
	}
	limit := 50
	offset := 0
	if value := q.Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			fail(w, 400, "limit 必须为 1 到 100")
			return
		}
		limit = n
	}
	if value := q.Get("offset"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 2000 {
			fail(w, 400, "offset 必须为 0 到 2000")
			return
		}
		offset = n
	}
	if len(q.Get("search")) > 256 {
		fail(w, 400, "搜索条件过长")
		return
	}
	rows, total, err := s.requests.List(r.Context(), store.RequestFilter{Status: status, Search: q.Get("search"), Limit: limit, Offset: offset})
	if err != nil {
		diagnostics.Event(r.Context(), "error", "requestlog.query_failed", "error", err)
		fail(w, 500, "请求日志读取失败")
		return
	}
	opts := s.requests.Options()
	compressedJSON(w, r, map[string]any{"data": rows, "total": total, "limit": limit, "offset": offset, "retention": map[string]any{"max_records": opts.MaxRows, "error_days": int(opts.MaxAge / (24 * time.Hour)), "error_max_records": opts.MaxFiles, "error_max_mb": opts.MaxDiskBytes >> 20}})
}

func (s *Server) requestDetail(w http.ResponseWriter, r *http.Request, _ *session) {
	if s.requests == nil {
		fail(w, 503, "请求日志尚未启用")
		return
	}
	detail, err := s.requests.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
		fail(w, 404, "请求记录不存在或已清理")
		return
	}
	if err != nil {
		diagnostics.Event(r.Context(), "error", "requestlog.detail_failed", "error", err)
		fail(w, 500, "请求详情读取失败")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/download") {
		w.Header().Set("Content-Disposition", `attachment; filename="quickwork-request-`+r.PathValue("id")+`.json"`)
	}
	compressedJSON(w, r, detail)
}

func compressedJSON(w http.ResponseWriter, r *http.Request, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Add("Vary", "Accept-Encoding")
	var output io.Writer = w
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		defer compressed.Close()
		output = compressed
	}
	_ = json.NewEncoder(output).Encode(value)
}

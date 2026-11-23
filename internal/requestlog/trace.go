package requestlog

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

type attempt struct {
	Index            int    `json:"index"`
	AccountID        int64  `json:"account_id"`
	DurationMS       int64  `json:"duration_ms"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	UsageKnown       bool   `json:"usage_known"`
	Error            string `json:"error,omitempty"`
}
type trace struct {
	mu             sync.Mutex
	persistMu      sync.Mutex
	recorder       *Recorder
	record         store.RequestRecord
	started        time.Time
	attempts       map[int]*attempt
	failedAccounts map[int64]bool
	events         []map[string]any
	request        map[string]any
	response       map[string]any
	httpDone       bool
	completed      bool
	streamError    bool
	serverError    bool
	timedOut       bool
	transientError bool
	background     bool
	backgroundDone bool
	lastError      string
}

func (t *trace) Observe(ctx context.Context, level, event string, f map[string]any) {
	t.mu.Lock()
	index := int(number(f["attempt"]))
	if index > t.record.AttemptCount {
		t.record.AttemptCount = index
	}
	if len(t.events) < 200 && (level == "error" || level == "warn" || event == "chat.client_tool.emitted" || event == "account.selected" || event == "media.preview.skipped" || strings.HasPrefix(event, "video.job.")) {
		entry := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "level": level, "event": event}
		for _, key := range []string{"account_id", "attempt", "tool_id", "tool_name", "arguments_bytes", "job_id", "status", "duration_ms", "timeout_ms", "download_attempt", "http_status", "upstream_code", "native_path_sha256", "reason", "error", "error_message"} {
			if value, ok := f[key]; ok {
				if e, ok := value.(error); ok {
					value = e.Error()
				}
				entry[key] = redact(value, 0)
			}
		}
		t.events = append(t.events, entry)
	}
	switch event {
	case "anthropic.request.converted", "chat.request.validated":
		t.record.Model = cleanText(f["requested_model"])
		t.record.Stream, _ = f["stream"].(bool)
		if n := number(f["requested_max_tokens"]); n > 0 {
			t.record.MaxTokens = n
		}
	case "chat.context.prepared":
		t.record.UpstreamModel = cleanText(f["mapped_model"])
	case "account.selected":
		t.record.AccountID = number(f["account_id"])
	case "account.upstream_failed", "account.skipped", "account.refresh.failed":
		t.failedAccounts[number(f["account_id"])] = true
		t.lastError = cleanText(f["error"])
	case "upstream.turn.finished":
		if index < 1 {
			index = 1
		}
		a := &attempt{Index: index, AccountID: number(f["account_id"]), DurationMS: number(f["duration_ms"]), Error: cleanText(f["error"])}
		if usage, ok := f["upstream_usage"].(map[string]any); ok {
			a.CacheReadTokens = number(usage["cache_read_tokens"])
			a.CacheWriteTokens = number(usage["cache_write_tokens"])
			a.OutputTokens = number(usage["output_tokens"])
			a.InputTokens = number(usage["input_tokens"]) + a.CacheReadTokens + a.CacheWriteTokens
			if total := number(usage["total_tokens"]); total > 0 {
				a.InputTokens = max(total-a.OutputTokens, 0)
			}
			a.UsageKnown = a.InputTokens > 0 || a.OutputTokens > 0
		} else if usage, ok := f["usage"].(map[string]any); ok {
			a.InputTokens = number(usage["prompt_tokens"])
			a.OutputTokens = number(usage["completion_tokens"])
			a.UsageKnown = a.InputTokens > 0 || a.OutputTokens > 0
			if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
				a.CacheReadTokens = number(details["cached_tokens"])
			}
		}
		t.attempts[index] = a
		if a.Error != "" {
			t.lastError = a.Error
			t.failedAccounts[a.AccountID] = true
		}
	case "chat.response.completed":
		t.completed = true
		t.record.FinishReason = cleanText(f["finish_reason"])
	case "chat.response.stream_error":
		t.streamError = true
		t.serverError = t.serverError || ctx.Err() == nil
		t.lastError = cleanText(f["error_message"])
	case "chat.request.timed_out":
		t.timedOut = true
		t.streamError = true
		t.lastError = cleanText(f["error"])
	case "upstream.tool.error":
		t.transientError = true
		t.lastError = cleanText(f["error_message"])
	case "media.download.retry":
		t.transientError = true
		t.lastError = cleanText(f["error"])
	case "upstream.tool.result":
		if status := textValue(f["tool_status"]); status == "error" || status == "failed" {
			t.transientError = true
			t.lastError = cleanText(f["error_message"])
			if t.lastError == "" {
				t.lastError = "上游工具曾执行失败"
			}
		}
	case "upstream.socket.read_failed":
		complete, _ := f["saw_message_complete"].(bool)
		partial, _ := f["partial_accepted"].(bool)
		if !complete && partial {
			t.streamError = true
			t.lastError = "上游异常断开，回复可能不完整：" + cleanText(f["error"])
		}
	case "chat.attempt.account_failed", "video.attempt.account_failed", "chat.media.prepare_failed":
		t.lastError = cleanText(f["error"])
	case "video.job.queued":
		t.background = true
		t.record.JobID = cleanText(f["job_id"])
		t.record.Status = "queued"
		t.record.Model = cleanText(f["model"])
	case "video.job.started":
		t.background = true
		t.record.Status = "in_progress"
	case "video.job.finished":
		t.backgroundDone = true
		t.completed = f["status"] == "completed"
		t.record.Status = "success"
		if !t.completed {
			t.record.Status = "error"
			t.lastError = cleanText(f["error"])
		}
	}
	save := (event == "video.job.finished" || event == "video.job.started") && t.httpDone
	t.mu.Unlock()
	if save {
		t.persist()
	}
}

func (t *trace) aggregate() {
	t.record.InputTokens = 0
	t.record.OutputTokens = 0
	t.record.CacheReadTokens = 0
	t.record.CacheWriteTokens = 0
	t.record.UsageKnown = false
	for _, a := range t.attempts {
		t.record.InputTokens += a.InputTokens
		t.record.OutputTokens += a.OutputTokens
		t.record.CacheReadTokens += a.CacheReadTokens
		t.record.CacheWriteTokens += a.CacheWriteTokens
		t.record.UsageKnown = t.record.UsageKnown || a.UsageKnown
	}
	t.record.RetryCount = max(t.record.AttemptCount-1, 0)
	if t.completed {
		t.record.RetryCount = max(t.record.RetryCount, len(t.failedAccounts))
	} else {
		t.record.RetryCount = max(t.record.RetryCount, len(t.failedAccounts)-1)
	}
	if t.record.Status == "success" && (t.record.RetryCount > 0 || t.transientError) {
		t.record.Status = "recovered"
	}
	if t.record.Status == "error" || t.record.Status == "cancelled" || t.record.Status == "recovered" {
		t.record.Error = t.lastError
	}
	t.record.UpdatedAt = time.Now().UnixMilli()
	t.record.DurationMS = time.Since(t.started).Milliseconds()
}

func (t *trace) persist() {
	t.persistMu.Lock()
	defer t.persistMu.Unlock()
	t.mu.Lock()
	t.aggregate()
	record := t.record
	attempts := make([]attempt, 0, len(t.attempts))
	for index := 1; index <= max(t.record.AttemptCount, 1); index++ {
		if a := t.attempts[index]; a != nil {
			attempts = append(attempts, *a)
		}
	}
	detail := map[string]any{"record": record, "request": t.request, "response": t.response, "attempts": attempts, "events": append([]map[string]any(nil), t.events...), "diagnostic_request_id": record.ID, "redaction_note": "凭据和下载签名已脱敏；媒体原始数据已省略；单侧正文最多捕获 512 KiB"}
	t.mu.Unlock()
	t.recorder.persist(record, detail)
}

func (t *trace) end(r *http.Request, w *writer, requestBody *capture, panicked bool) {
	t.mu.Lock()
	defer func() { t.mu.Unlock(); t.persist() }()
	t.httpDone = true
	t.record.HTTPStatus = w.status
	t.record.FirstByteMS = w.firstByteMS
	t.record.RequestBytes = requestBody.total
	if r.ContentLength > t.record.RequestBytes {
		t.record.RequestBytes = r.ContentLength
	}
	t.record.ResponseBytes = w.body.total
	t.request = map[string]any{"headers": safeHeaders(r.Header), "method": r.Method, "path": r.URL.Path, "content": requestBody.snapshot(r.Header.Get("Content-Type"))}
	requestContent := t.request["content"].(map[string]any)
	requestContent["declared_bytes"] = r.ContentLength
	if r.ContentLength > requestBody.total {
		requestContent["partial_read"] = true
		requestContent["body_note"] = "处理器未完整读取请求正文，详情只包含已读取的数据"
	}
	t.response = map[string]any{"headers": safeHeaders(w.Header()), "http_status": w.status, "content": w.body.snapshot(w.Header().Get("Content-Type"))}
	if strings.HasPrefix(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream") {
		t.response["completion"] = map[string]any{"protocol_complete": w.completion.finished, "completed_while_connected": w.completion.finishedWhileActive, "stream_error_seen": w.completion.failed}
	}
	if message, failed := responseError(w.body.prefix.Bytes(), w.Header().Get("Content-Type")); failed {
		t.streamError = true
		if message != "" {
			t.lastError = diagnostics.RedactText(message, 2048)
		}
	}
	var input map[string]any
	if requestBody.total <= int64(requestBody.prefix.Len()) && json.Unmarshal(requestBody.prefix.Bytes(), &input) == nil {
		if t.record.Model == "" {
			t.record.Model = cleanText(input["model"])
		}
		if n := number(input["max_tokens"]); n > 0 {
			t.record.MaxTokens = n
		}
		if meta, ok := input["metadata"].(map[string]any); ok {
			if id := textValue(meta["user_id"]); id != "" {
				t.record.SessionID = "session_" + diagnostics.Fingerprint(id)[:12]
			}
		}
	}
	if t.background && !t.backgroundDone && w.status < 400 && !panicked {
		return
	}
	if !t.backgroundDone {
		t.record.Status = "success"
		responseComplete := w.completion.complete() && w.status < 400 && !t.streamError && !panicked
		if responseComplete {
			t.completed = true
			if t.record.FinishReason == "" {
				t.record.FinishReason = w.completion.finishReason
			}
			if r.Context().Err() != nil && len(t.events) < 200 {
				t.events = append(t.events, map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "level": "info", "event": "http.client.closed_after_completion"})
			}
		}
		if (r.Context().Err() != nil && !responseComplete) || w.writeError != nil {
			t.record.Status = "cancelled"
			if t.lastError == "" {
				t.lastError = "客户端连接已关闭"
			}
		}
		if t.timedOut || t.serverError || ((w.status >= 400 || t.streamError || panicked) && t.record.Status != "cancelled") {
			t.record.Status = "error"
		}
		if panicked && t.lastError == "" {
			t.lastError = "处理请求时发生内部异常"
		}
		if t.record.Status == "error" && t.lastError == "" {
			var output map[string]any
			if json.Unmarshal(w.body.prefix.Bytes(), &output) == nil {
				switch detail := output["error"].(type) {
				case string:
					t.lastError = cleanText(detail)
				case map[string]any:
					t.lastError = cleanText(detail["message"])
				}
			}
			if t.lastError == "" {
				t.lastError = http.StatusText(w.status)
			}
		}
	}
}

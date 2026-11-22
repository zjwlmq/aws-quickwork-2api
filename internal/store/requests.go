package store

import (
	"context"
	"encoding/json"
	"strings"
)

// RequestRecord contains metadata only. Request and response bodies never enter
// this table; error details use independently bounded private files.
type RequestRecord struct {
	ID                string `json:"id"`
	StartedAt         int64  `json:"started_at"`
	UpdatedAt         int64  `json:"updated_at"`
	Status            string `json:"status"`
	HTTPStatus        int    `json:"http_status"`
	Method            string `json:"method"`
	Path              string `json:"path"`
	Protocol          string `json:"protocol"`
	Model             string `json:"model"`
	UpstreamModel     string `json:"upstream_model"`
	Stream            bool   `json:"stream"`
	KeyID             string `json:"key_id"`
	Client            string `json:"client"`
	SessionID         string `json:"session_id,omitempty"`
	AccountID         int64  `json:"account_id"`
	AttemptCount      int    `json:"attempt_count"`
	RetryCount        int    `json:"retry_count"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	UsageKnown        bool   `json:"usage_known"`
	DurationMS        int64  `json:"duration_ms"`
	FirstByteMS       int64  `json:"first_byte_ms"`
	RequestBytes      int64  `json:"request_bytes"`
	ResponseBytes     int64  `json:"response_bytes"`
	MaxTokens         int64  `json:"requested_max_tokens,omitempty"`
	MaxTokensEnforced bool   `json:"max_tokens_enforced"`
	FinishReason      string `json:"finish_reason,omitempty"`
	Error             string `json:"error,omitempty"`
	JobID             string `json:"job_id,omitempty"`
	ErrorDetail       bool   `json:"error_detail"`
}

type RequestFilter struct {
	Status string
	Search string
	Limit  int
	Offset int
}

func (s *Store) InitRequests(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS request_logs (
		id TEXT PRIMARY KEY, started_at INTEGER NOT NULL, status TEXT NOT NULL,
		search_text TEXT NOT NULL, summary_json TEXT NOT NULL);
		CREATE INDEX IF NOT EXISTS idx_request_logs_time ON request_logs(started_at DESC);
		CREATE INDEX IF NOT EXISTS idx_request_logs_status ON request_logs(status,started_at DESC);`)
	return err
}

func (s *Store) SaveRequest(ctx context.Context, r RequestRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	search := strings.ToLower(strings.Join([]string{r.ID, r.Model, r.UpstreamModel, r.Protocol, r.KeyID, r.Client, r.SessionID, r.JobID}, " "))
	_, err = s.db.ExecContext(ctx, `INSERT INTO request_logs(id,started_at,status,search_text,summary_json) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status,search_text=excluded.search_text,summary_json=excluded.summary_json`, r.ID, r.StartedAt, r.Status, search, string(b))
	return err
}

func (s *Store) GetRequest(ctx context.Context, id string) (RequestRecord, error) {
	var raw string
	var r RequestRecord
	err := s.db.QueryRowContext(ctx, `SELECT summary_json FROM request_logs WHERE id=?`, id).Scan(&raw)
	if err != nil {
		return r, err
	}
	err = json.Unmarshal([]byte(raw), &r)
	return r, err
}

func (s *Store) ListRequests(ctx context.Context, f RequestFilter) ([]RequestRecord, int, error) {
	where := " WHERE 1=1"
	args := []any{}
	if f.Status == "errors" {
		where += " AND status IN ('error','cancelled','interrupted')"
	} else if f.Status == "active" {
		where += " AND status IN ('running','queued','in_progress')"
	} else if f.Status != "" {
		where += " AND status=?"
		args = append(args, f.Status)
	}
	if f.Search != "" {
		where += " AND instr(search_text,?)>0"
		args = append(args, strings.ToLower(f.Search))
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_logs"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit < 1 {
		limit = 50
	}
	if limit > 2000 {
		limit = 2000
	}
	offset := max(f.Offset, 0)
	queryArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := s.db.QueryContext(ctx, "SELECT summary_json FROM request_logs"+where+" ORDER BY started_at DESC,id DESC LIMIT ? OFFSET ?", queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]RequestRecord, 0)
	for rows.Next() {
		var raw string
		var r RequestRecord
		if err := rows.Scan(&raw); err != nil {
			return nil, 0, err
		}
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, 0, err
		}
		result = append(result, r)
	}
	return result, total, rows.Err()
}

func (s *Store) TrimRequests(ctx context.Context, limit int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE id IN (SELECT id FROM request_logs ORDER BY started_at DESC,id DESC LIMIT -1 OFFSET ?)`, limit)
	return err
}

func (s *Store) RecoverRequests(ctx context.Context, now int64) error {
	requests, _, err := s.ListRequests(ctx, RequestFilter{Limit: 2000})
	if err != nil {
		return err
	}
	for _, r := range requests {
		if r.Status == "running" || r.Status == "queued" || r.Status == "in_progress" {
			r.Status = "interrupted"
			r.Error = "服务重启，处理结果未知"
			r.UpdatedAt = now
			r.DurationMS = max(now-r.StartedAt, 0)
			if err := s.SaveRequest(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

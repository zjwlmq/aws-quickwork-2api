package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

const quotaTTL = 2 * time.Minute

type quotaWarning struct {
	Severity    string `json:"severity"`
	Description string `json:"description,omitempty"`
}

// Values retain Quick's native units. Only ratios are presented in the console:
// session and monthly usage must not be assumed to share units or token costs.
type quotaWindow struct {
	Used             *float64      `json:"used"`
	Limit            *float64      `json:"limit"`
	Remaining        *float64      `json:"remaining"`
	UsedPercent      *float64      `json:"used_percent"`
	RemainingPercent *float64      `json:"remaining_percent"`
	ResetsAt         int64         `json:"resets_at,omitempty"`
	ResumeInMinutes  *float64      `json:"resume_in_minutes,omitempty"`
	Warning          *quotaWarning `json:"warning,omitempty"`
}

type quotaSubscription struct {
	Type           string `json:"type"`
	TrialStatus    string `json:"trial_status,omitempty"`
	TrialExpiresAt int64  `json:"trial_expires_at,omitempty"`
}

type quotaView struct {
	AccountID    int64             `json:"account_id"`
	Status       string            `json:"status"`
	UpdatedAt    int64             `json:"updated_at"`
	CheckedAt    int64             `json:"checked_at"`
	Session      *quotaWindow      `json:"session"`
	Monthly      *quotaWindow      `json:"monthly"`
	Overage      *quotaWindow      `json:"overage_monthly,omitempty"`
	Subscription quotaSubscription `json:"subscription"`
	Error        string            `json:"error,omitempty"`
}

type quotaCache struct {
	value   quotaView
	expires time.Time
	loading chan struct{}
}

type quotaError struct{ status int }

func (e *quotaError) Error() string {
	switch e.status {
	case 401:
		return "登录已失效，请刷新账号授权后重试"
	case 403:
		return "当前账号无权查询额度"
	case 404, 405, 501:
		return "该账号暂不提供额度数据"
	case 429:
		return "额度查询过于频繁，请稍后重试"
	default:
		return fmt.Sprintf("Quick 额度接口返回 HTTP %d", e.status)
	}
}

func newQuotaHTTP() *http.Client {
	return &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *Server) listQuotas(w http.ResponseWriter, r *http.Request, _ *session) {
	list, err := s.store.List(r.Context())
	if err != nil {
		fail(w, 500, "读取账号失败")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	out := make([]quotaView, len(list))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, a := range list {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				out[i] = quotaView{AccountID: id, Status: "error", Error: "额度查询超时，请重试"}
				return
			}
			out[i] = s.accountQuota(ctx, id, r.Method == http.MethodPost)
		}(i, a.ID)
	}
	wg.Wait()
	// Discard cache entries for deleted accounts without affecting in-flight reads.
	s.quotaMu.Lock()
	ids := make(map[int64]bool, len(list))
	for _, a := range list {
		ids[a.ID] = true
	}
	for id := range s.quotas {
		if !ids[id] && s.quotas[id].loading == nil {
			delete(s.quotas, id)
		}
	}
	s.quotaMu.Unlock()
	reply(w, 200, map[string]any{"data": out, "cache_seconds": int(quotaTTL / time.Second)})
}

func (s *Server) accountQuota(ctx context.Context, id int64, force bool) quotaView {
	s.quotaMu.Lock()
	entry := s.quotas[id]
	if entry == nil {
		entry = &quotaCache{value: quotaView{AccountID: id}}
		s.quotas[id] = entry
	}
	if entry.loading != nil {
		loading := entry.loading
		s.quotaMu.Unlock()
		select {
		case <-loading:
			s.quotaMu.Lock()
			value := entry.value
			s.quotaMu.Unlock()
			return value
		case <-ctx.Done():
			return quotaView{AccountID: id, Status: "error", Error: "额度查询超时，请重试"}
		}
	}
	if !force && time.Now().Before(entry.expires) {
		value := entry.value
		s.quotaMu.Unlock()
		return value
	}
	entry.loading = make(chan struct{})
	s.quotaMu.Unlock()

	started := time.Now()
	ctx = diagnostics.With(ctx, "account_id", id)
	a, err := s.manager.GetFresh(ctx, id)
	var value quotaView
	if err == nil {
		value, err = queryQuota(ctx, s.quotaHTTP, s.cfg.WorkingSessionID, s.cfg.Surface, s.cfg.AppVersion, a)
		var nativeErr *quotaError
		if errors.As(err, &nativeErr) && nativeErr.status == 401 {
			if fresh, refreshErr := s.manager.RefreshOne(ctx, id); refreshErr == nil {
				value, err = queryQuota(ctx, s.quotaHTTP, s.cfg.WorkingSessionID, s.cfg.Surface, s.cfg.AppVersion, fresh)
			} else {
				err = errors.New("登录已失效，请刷新账号授权后重试")
			}
		}
	} else {
		err = errors.New("无法读取账号授权，请刷新账号或重新登录")
	}
	now := time.Now()
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if err == nil {
		value.AccountID = id
		value.Status = "available"
		value.UpdatedAt = now.Unix()
		value.CheckedAt = now.Unix()
		entry.value = value
		entry.expires = now.Add(quotaTTL)
		diagnostics.Event(ctx, "info", "account.quota.updated", "duration_ms", time.Since(started).Milliseconds())
	} else {
		entry.value.Status = "error"
		if entry.value.UpdatedAt != 0 {
			entry.value.Status = "stale"
		}
		entry.value.Error = err.Error()
		entry.value.CheckedAt = now.Unix()
		entry.expires = now.Add(15 * time.Second)
		diagnostics.Event(ctx, "warn", "account.quota.failed", "duration_ms", time.Since(started).Milliseconds(), "error", err)
	}
	close(entry.loading)
	entry.loading = nil
	return entry.value
}

func queryQuota(ctx context.Context, client *http.Client, workingSession, surface, version string, a *store.Account) (quotaView, error) {
	u, err := url.Parse("https://" + strings.TrimPrefix(strings.TrimPrefix(a.WSHost, "wss://"), "https://"))
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return quotaView{}, errors.New("Quick 额度服务地址无效")
	}
	u.Path = "/integration/quick-work/qbs-event"
	query := map[string]string{"_method": "GET"}
	if surface != "" {
		query["surfaceType"] = surface
	}
	if version != "" {
		query["appVersion"] = version
	}
	invocation := map[string]any{"path": "profile/usage", "queryParams": query, "body": map[string]any{}}
	if workingSession != "" {
		invocation["workingSessionId"] = workingSession
	}
	body, _ := json.Marshal(map[string]any{"eventType": "invokeAgent", "eventBody": map[string]any{"invokeAgent": invocation}})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return quotaView{}, errors.New("无法创建额度查询")
	}
	request.Header.Set("Authorization", "Bearer "+a.JWT())
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return quotaView{}, errors.New("额度查询连接失败或超时，请稍后重试")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return quotaView{}, &quotaError{status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return quotaView{}, errors.New("额度响应读取失败或超过大小限制")
	}
	return parseQuota(data)
}

func parseQuota(data []byte) (quotaView, error) {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return quotaView{}, errors.New("额度响应格式无效")
	}
	for range 10 {
		switch x := value.(type) {
		case string:
			if json.Unmarshal([]byte(x), &value) != nil {
				return quotaView{}, errors.New("额度响应格式无效")
			}
		case map[string]any:
			if event, ok := x["eventResponse"].(map[string]any); ok {
				value = event["invokeAgentResponse"]
				continue
			}
			if status, ok := x["status"].(float64); ok && status >= 300 {
				return quotaView{}, &quotaError{status: int(status)}
			}
			if body, ok := x["body"]; ok {
				if x["body_encoding"] == "base64" {
					text, _ := body.(string)
					decoded, err := base64.StdEncoding.DecodeString(text)
					if err != nil {
						return quotaView{}, errors.New("额度响应编码无效")
					}
					body = string(decoded)
				}
				value = body
				continue
			}
			return normalizeQuota(x)
		default:
			return quotaView{}, errors.New("上游未提供可识别的额度数据")
		}
	}
	return quotaView{}, errors.New("额度响应嵌套过深")
}

func nonnegative(value any) *float64 {
	n, ok := value.(float64)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	return &n
}

func normalizeWindow(value any) *quotaWindow {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	w := &quotaWindow{Used: nonnegative(raw["used"]), Limit: nonnegative(raw["limit"]), ResumeInMinutes: nonnegative(raw["resume_in_minutes"])}
	if w.Used != nil && w.Limit != nil && *w.Limit > 0 {
		remaining := max(*w.Limit-*w.Used, 0)
		percent := *w.Used / *w.Limit * 100
		if !math.IsInf(percent, 0) {
			w.Remaining = &remaining
			w.UsedPercent = &percent
		}
	} else if raw["limit"] == nil {
		w.UsedPercent = nonnegative(raw["percent"])
	}
	if w.UsedPercent != nil {
		remaining := max(100-*w.UsedPercent, 0)
		w.RemainingPercent = &remaining
	}
	if resets := nonnegative(raw["resets_at"]); resets != nil && *resets <= 253402300799 {
		w.ResetsAt = int64(*resets)
	}
	if warning, ok := raw["warning"].(map[string]any); ok {
		severity, _ := warning["severity"].(string)
		if severity == "warning" || severity == "error" || severity == "info" {
			text, _ := warning["description"].(string)
			w.Warning = &quotaWarning{Severity: severity, Description: diagnostics.RedactText(text, 512)}
		}
	}
	return w
}

func normalizeQuota(raw map[string]any) (quotaView, error) {
	v := quotaView{Session: normalizeWindow(raw["session"]), Monthly: normalizeWindow(raw["monthly"]), Overage: normalizeWindow(raw["overage_monthly"])}
	if v.Session == nil && v.Monthly == nil && v.Overage == nil {
		return v, errors.New("上游未提供可识别的额度数据")
	}
	if subscription, ok := raw["subscription"].(map[string]any); ok {
		kind, _ := subscription["subscription_type"].(string)
		v.Subscription.Type = diagnostics.RedactText(kind, 100)
		if trial, ok := subscription["free_trial_metadata"].(map[string]any); ok {
			status, _ := trial["status"].(string)
			v.Subscription.TrialStatus = diagnostics.RedactText(status, 64)
			if expires := nonnegative(trial["expiry_date"]); expires != nil && *expires <= 253402300799 {
				v.Subscription.TrialExpiresAt = int64(*expires)
			}
		}
	}
	return v, nil
}

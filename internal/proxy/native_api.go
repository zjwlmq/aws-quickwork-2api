package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"quickwork-openai/internal/diagnostics"
)

func (u *Upstream) nativeAPI(ctx context.Context, sess *Session, method, apiPath string, data any) (any, error) {
	host := sess.WSHost
	if strings.HasPrefix(host, "ws://") {
		host = "http://" + strings.TrimPrefix(host, "ws://")
	}
	if strings.HasPrefix(host, "wss://") {
		host = "https://" + strings.TrimPrefix(host, "wss://")
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	endpoint, err := url.Parse(host)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, fmt.Errorf("invalid native API host")
	}
	endpoint.Path = "/integration/quick-work/qbs-event"
	invocation := map[string]any{"path": apiPath, "queryParams": map[string]string{"_method": method, "surfaceType": sess.Surface, "appVersion": u.cfg.AppVersion}, "body": data}
	if sess.WorkingSessionID != "" {
		invocation["workingSessionId"] = sess.WorkingSessionID
	}
	body, err := json.Marshal(map[string]any{"eventType": "invokeAgent", "eventBody": map[string]any{"invokeAgent": invocation}})
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+sess.jwt())
	r.Header.Set("Content-Type", "application/json")
	res, err := nativeHTTP.Do(r)
	if err != nil {
		return nil, fmt.Errorf("native API request failed: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 {
		return nil, fmt.Errorf("native API response unreadable or over 8 MiB")
	}
	diagnostics.Event(ctx, "debug", "upstream.api.response", "operation", apiPath, "http_status", res.StatusCode, "response_bytes", len(raw), "upstream_request_id", res.Header.Get("X-Amzn-Requestid"))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("native API HTTP %d: %s", res.StatusCode, errorText(json.RawMessage(raw), "", ""))
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, fmt.Errorf("native API returned invalid JSON")
	}
	return unwrapNative(value)
}

func (u *Upstream) DeleteSession(ctx context.Context, sess *Session, thread string) error {
	value, err := u.nativeAPI(ctx, sess, http.MethodPost, "api/ws-rpc", map[string]any{"type": "session_delete", "payload": map[string]any{"session_id": thread}})
	if err != nil {
		return err
	}
	obj, _ := value.(map[string]any)
	frames, _ := obj["frames"].([]any)
	for _, frame := range frames {
		f, _ := frame.(map[string]any)
		if f["type"] == "session_deleted" {
			return nil
		}
	}
	return fmt.Errorf("native session deletion was not acknowledged")
}

func (u *Upstream) createSession(ctx context.Context, sess *Session, thread, title string) error {
	value, err := u.nativeAPI(ctx, sess, http.MethodPost, "api/ws-rpc", map[string]any{"type": "session_create", "payload": map[string]any{"session_id": thread, "title": title}})
	if err != nil {
		return err
	}
	obj, _ := value.(map[string]any)
	frames, _ := obj["frames"].([]any)
	for _, frame := range frames {
		f, _ := frame.(map[string]any)
		if f["type"] == "session_created" {
			return nil
		}
	}
	return fmt.Errorf("native session creation was not acknowledged")
}

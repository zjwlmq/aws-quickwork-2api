package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"quickwork-openai/internal/config"
	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

const (
	defaultWSPath        = "/streaming/quickwork-chat-bidirectional-stream"
	defaultGatewayWSPath = "/ws/service/SpaceNeedleConversationService/operation/ChatBidirectionalStream"
)

type Upstream struct {
	cfg *config.Config
}

func NewUpstream(cfg *config.Config) *Upstream {
	return &Upstream{cfg: cfg}
}

// ChatInput is one upstream turn.
type ChatInput struct {
	Prompt          string
	Model           string
	ThreadID        string
	Title           string
	SystemPrompt    string
	ToolPrompt      string
	Attachments     []Attachment
	contextPrepared bool
	KeepSession     bool
	ArtifactHandler ArtifactHandler
}

// ToolCallState is a tool invocation reported by the QuickWork agent.
type ToolCallState struct {
	ID        string
	Name      string
	Arguments string
	Result    string
	Status    string
}

// Usage aggregates turn_usage events across a multi-step tool turn.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
}

func (u *Usage) add(v Usage) {
	u.PromptTokens += v.PromptTokens
	u.CompletionTokens += v.CompletionTokens
	u.TotalTokens += v.TotalTokens
	u.CacheReadTokens += v.CacheReadTokens
	u.CacheWriteTokens += v.CacheWriteTokens
}

// StreamEvent is a text delta or a completed tool call.
type StreamEvent struct {
	Type     string
	Text     string
	ToolCall *ToolCallState
}

type EventFunc func(StreamEvent) error

type ChatResult struct {
	Text      string
	ToolCalls []ToolCallState
	Usage     Usage
	Artifacts []NativeArtifact
	Files     []store.MediaFile
	ThreadID  string
}

// Chat sends one prompt through the QuickWork chat bidi WebSocket. Text
// deltas and completed tool calls are reported through onEvent.
func (u *Upstream) Chat(ctx context.Context, sess *Session, in ChatInput, onEvent EventFunc) (result ChatResult, err error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return ChatResult{}, fmt.Errorf("prompt is empty")
	}
	if !in.contextPrepared {
		in, err = prepareNativeContext(ctx, in)
		if err != nil {
			return ChatResult{}, err
		}
	}
	threadID := strings.TrimSpace(in.ThreadID)
	if threadID == "" {
		threadID = uuid()
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = titleFor(in.Prompt)
	}
	messageID := uuid()
	traceID := uuid()
	ctx = diagnostics.With(ctx, "account_id", sess.AccountID, "upstream_thread_id", threadID, "upstream_message_id", messageID, "upstream_trace_id", traceID)
	started := time.Now()
	defer func() {
		level := "info"
		if err != nil {
			level = "error"
		}
		diagnostics.Event(ctx, level, "upstream.turn.finished", "duration_ms", time.Since(started).Milliseconds(), "text_bytes", len(result.Text), "native_tools", len(result.ToolCalls), "artifacts", len(result.Artifacts), "cached_files", len(result.Files), "usage", usageMap(result.Usage), "upstream_usage", map[string]any{"input_tokens": result.Usage.PromptTokens, "output_tokens": result.Usage.CompletionTokens, "total_tokens": result.Usage.TotalTokens, "cache_read_tokens": result.Usage.CacheReadTokens, "cache_write_tokens": result.Usage.CacheWriteTokens}, "error", err, "context_error", ctx.Err())
	}()

	wsURL, headers, gateway, err := u.dialInfo(sess)
	if err != nil {
		return ChatResult{}, err
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 20 * time.Second
	parsedURL, _ := url.Parse(wsURL)
	diagnostics.Event(ctx, "info", "upstream.connection.started", "host", parsedURL.Hostname(), "gateway", gateway, "handshake_timeout_ms", 20000)
	dialStarted := time.Now()
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		detail := ""
		if resp != nil {
			detail = fmt.Sprintf(" HTTP %d", resp.StatusCode)
		}
		return ChatResult{}, fmt.Errorf("upstream WebSocket dial failed%s: %w", detail, err)
	}
	defer conn.Close()
	diagnostics.Event(ctx, "info", "upstream.connection.established", "duration_ms", time.Since(dialStarted).Milliseconds(), "http_status", resp.StatusCode, "upstream_request_id", resp.Header.Get("X-Amzn-Requestid"))

	writeMu := &sync.Mutex{}
	encode := func(v any) ([]byte, error) {
		body, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if gateway {
			frame := gatewayEncode(string(body))
			if frame == nil {
				return nil, fmt.Errorf("encode gateway frame")
			}
			return frame, nil
		}
		// AIDP (enterprise and social) uses the block-style binary envelope:
		// {"eventType":"rawInputEvent","payload":{"payload":base64(json)}}.
		return aidpEncode(string(body)), nil
	}
	messageType := websocket.BinaryMessage
	send := func(v any) error {
		frame, err := encode(v)
		if err != nil {
			return err
		}
		kind := ""
		if obj, ok := v.(map[string]any); ok {
			kind, _ = obj["type"].(string)
		}
		if kind == "user_message" && len(frame) > 64<<10 {
			diagnostics.Event(ctx, "warn", "upstream.message.too_large", "frame_bytes", len(frame), "frame_budget_bytes", 64<<10)
			return &upstreamRequestError{code: "context_length_exceeded", message: "完整请求封装超过 Quick 的 64 KiB 传输上限；内容未截断，请缩短会话或减少附件后重试"}
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		err = conn.WriteMessage(messageType, frame)
		level := "debug"
		if err != nil {
			level = "error"
		}
		diagnostics.Event(ctx, level, "upstream.message.sent", "event_type", kind, "frame_bytes", len(frame), "error", err)
		return err
	}

	// Keep the socket alive while the agent is thinking.
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeMu.Lock()
				pingErr := conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
				writeMu.Unlock()
				if pingErr != nil {
					diagnostics.Event(ctx, "warn", "upstream.ping.failed", "error", pingErr)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	// Handshake. The AIDP and Gateway paths use different session_connect shapes.
	if gateway {
		_ = send(map[string]any{"type": "session_connect", "session_id": sess.OutpostID})
	} else {
		_ = send(map[string]any{
			"type": "session_connect",
			"payload": map[string]any{
				"actor_id":               sess.ActorARN,
				"surfaceType":            sess.Surface,
				"outpostId":              sess.OutpostID,
				"appVersion":             u.cfg.AppVersion,
				"external_agent_sign_in": 1,
				"recents_promotion":      1,
			},
		})
	}
	_ = send(map[string]any{"type": "federate_status", "payload": map[string]any{}})
	_ = send(map[string]any{
		"type":    "session_create",
		"payload": map[string]any{"session_id": threadID, "title": title},
	})
	if !u.cfg.KeepSessions && !in.KeepSession {
		defer func() {
			_ = send(map[string]any{"type": "session_delete", "payload": map[string]any{"session_id": threadID}})
		}()
	}

	payload := map[string]any{
		"text":               in.Prompt,
		"send_origin":        "programmatic",
		"had_physical_input": false,
		"surfaceType":        sess.Surface,
		"outpostId":          sess.OutpostID,
		"appVersion":         u.cfg.AppVersion,
		"traceId":            traceID,
		"message_id":         messageID,
		"thread_id":          threadID,
		"agent_mode":         in.Model,
		"user_timezone":      u.cfg.Timezone,
		"user_locale":        u.cfg.Locale,
		"chat_agent_id":      nil,
		"auto_routing":       false,
		"web_search_enabled": false,
		"pre_approved_tools": []any{},
		"preload_skills":     []any{},
		"panel_edits":        []any{},
		"custom_prompt":      nil,
	}
	// Quick's first-party client sends application instructions as custom_prompt.
	// The undocumented system_prompt key is silently ignored by the AIDP service.
	if strings.TrimSpace(in.SystemPrompt) != "" {
		payload["custom_prompt"] = in.SystemPrompt
	}
	if u.cfg.ThinkingEffort != "" && u.cfg.ThinkingEffort != "off" {
		payload["thinking_effort"] = u.cfg.ThinkingEffort
	}
	if len(in.Attachments) > 0 {
		payload["attachments"] = in.Attachments
	}
	if err := send(map[string]any{"type": "user_message", "payload": payload}); err != nil {
		return ChatResult{}, fmt.Errorf("send user_message: %w", err)
	}

	diagnostics.Event(ctx, "info", "upstream.context.sent", "prompt_bytes", len(in.Prompt), "custom_prompt_present", in.SystemPrompt != "", "custom_prompt_bytes", len(in.SystemPrompt), "system_wire_field", "custom_prompt", "attachment_count", len(in.Attachments), "model", in.Model)
	result, err = u.readEvents(ctx, conn, gateway, onEvent)
	result.ThreadID = threadID
	if err != nil {
		return result, err
	}
	if len(result.Artifacts) > 0 && in.ArtifactHandler != nil {
		result.Files, err = in.ArtifactHandler(ctx, threadID, result.Artifacts)
		if err != nil {
			return result, fmt.Errorf("生成文件下载失败: %w", err)
		}
	}

	return result, nil
}

func (u *Upstream) dialInfo(sess *Session) (wsURL string, headers http.Header, gateway bool, err error) {
	jwt := sess.jwt()
	if jwt == "" {
		return "", nil, false, fmt.Errorf("missing jwt")
	}
	gateway = isGatewayProvider(sess.Provider)

	host := strings.TrimSpace(sess.WSHost)
	path := strings.TrimSpace(sess.WSPath)
	if path == "" {
		if gateway {
			path = defaultGatewayWSPath
		} else {
			path = defaultWSPath
		}
	}
	switch {
	case strings.HasPrefix(host, "wss://") || strings.HasPrefix(host, "ws://"):
		host = strings.TrimRight(host, "/")
	case strings.HasPrefix(host, "https://"):
		host = "wss://" + strings.TrimPrefix(host, "https://")
	default:
		host = "wss://" + strings.TrimRight(host, "/")
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		wsURL = path
	} else {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		wsURL = host + path
	}

	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", nil, false, fmt.Errorf("invalid ws_url: %w", err)
	}
	if !gateway {
		q := parsed.Query()
		if sess.Surface != "" {
			q.Set("surfaceType", sess.Surface)
		}
		if sess.OutpostID != "" {
			q.Set("outpostId", sess.OutpostID)
		}
		if sess.WorkingSessionID != "" {
			q.Set("workingSessionId", sess.WorkingSessionID)
		}
		// Social AIDP transports accept the bearer in the query; enterprise
		// relies on the Authorization header injected below.
		if strings.EqualFold(sess.Provider, "social") {
			q.Set("token", jwt)
		}
		parsed.RawQuery = q.Encode()
	}

	headers = http.Header{}
	headers.Set("Authorization", "Bearer "+jwt)
	headers.Set("Origin", "file://")
	headers.Set("X-Surface-Type", sess.Surface)
	headers.Set("X-Outpost-Id", sess.OutpostID)
	headers.Set("X-Agent-Service-Type", "UNIFIED_QUICK")
	if sess.WorkingSessionID != "" {
		headers.Set("X-Working-Session-Id", sess.WorkingSessionID)
	}
	return parsed.String(), headers, gateway, nil
}

func (u *Upstream) readEvents(ctx context.Context, conn *websocket.Conn, gateway bool, onEvent EventFunc) (ChatResult, error) {
	var result ChatResult
	var text strings.Builder
	sawMessageComplete := false
	var completeDeadline time.Time
	started := time.Now()
	sequence := 0
	eventCounts := map[string]int{}
	finishReason := "unknown"
	firstText := true
	defer func() {
		diagnostics.Event(ctx, "info", "upstream.stream.summary", "duration_ms", time.Since(started).Milliseconds(), "event_count", sequence, "event_counts", eventCounts, "finish_reason", finishReason, "text_bytes", text.Len(), "saw_message_complete", sawMessageComplete)
	}()

	toolCalls := make(map[string]*ToolCallState)
	toolOrder := make([]string, 0, 2)
	toolSent := make(map[string]bool)
	partialArgs := make(map[string]string)
	nativeSeen := make(map[string]bool)

	getTool := func(id, name string) *ToolCallState {
		id = strings.TrimSpace(id)
		if id == "" {
			id = "call_" + strings.ReplaceAll(uuid(), "-", "")
		}
		tc := toolCalls[id]
		if tc == nil {
			tc = &ToolCallState{ID: id}
			toolCalls[id] = tc
			toolOrder = append(toolOrder, id)
		}
		if n := strings.TrimSpace(name); n != "" {
			tc.Name = n
		}
		return tc
	}
	emitTool := func(tc *ToolCallState) error {
		if tc == nil || onEvent == nil || toolSent[tc.ID] {
			return nil
		}
		toolSent[tc.ID] = true
		copy := *tc
		return onEvent(StreamEvent{Type: "tool_call", ToolCall: &copy})
	}
	emitToolUpdate := func(tc *ToolCallState) error {
		if tc == nil || onEvent == nil {
			return nil
		}
		copy := *tc
		return onEvent(StreamEvent{Type: "tool_call_update", ToolCall: &copy})
	}
	emitOrUpdate := func(tc *ToolCallState) error {
		if toolSent[tc.ID] {
			return emitToolUpdate(tc)
		}
		return emitTool(tc)
	}
	finish := func() (ChatResult, error) {
		result.Text = text.String()
		result.ToolCalls = result.ToolCalls[:0]
		for _, id := range toolOrder {
			tc := toolCalls[id]
			if tc == nil {
				continue
			}
			if strings.TrimSpace(tc.Name) == "" {
				tc.Name = "unknown"
			}
			if strings.TrimSpace(tc.Arguments) == "" {
				if p := strings.TrimSpace(partialArgs[id]); p != "" && json.Valid([]byte(p)) {
					tc.Arguments = p
				} else {
					tc.Arguments = "{}"
				}
			}
			result.ToolCalls = append(result.ToolCalls, *tc)
			if err := emitTool(tc); err != nil {
				return result, err
			}
		}
		return result, nil
	}

	terminal := map[string]bool{
		"turn_completed": true,
		"turn_aborted":   true,
		"turn_ended":     true,
		"stream_ended":   true,
		"chat_complete":  true,
	}
	for {
		select {
		case <-ctx.Done():
			finishReason = "context_done"
			res, _ := finish()
			return res, ctx.Err()
		default:
		}
		readWait := 2 * time.Minute
		if !completeDeadline.IsZero() {
			if remaining := time.Until(completeDeadline); remaining > 0 {
				readWait = remaining
			} else {
				finishReason = "message_complete_grace_expired"
				return finish()
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			finishReason = "socket_read_error"
			if expectedStreamEnd(ctx, err, sawMessageComplete, completeDeadline) {
				finishReason = "message_complete_grace_expired_or_closed"
				diagnostics.Event(ctx, "info", "upstream.stream.closed", "finish_reason", finishReason, "read_wait_ms", readWait.Milliseconds())
			} else {
				diagnostics.Event(ctx, "warn", "upstream.socket.read_failed", "error", err, "read_wait_ms", readWait.Milliseconds(), "partial_accepted", text.Len() > 0 || len(toolOrder) > 0, "saw_message_complete", sawMessageComplete)
			}
			res, emitErr := finish()
			if emitErr != nil {
				return res, emitErr
			}
			if text.Len() > 0 || len(toolOrder) > 0 {
				return res, nil
			}
			return res, fmt.Errorf("upstream WebSocket read: %w", err)
		}

		var jsonStr string
		if msgType == websocket.TextMessage {
			jsonStr = string(data)
		} else {
			if gateway {
				jsonStr, err = gatewayDecode(data)
			} else {
				jsonStr, err = aidpDecode(data)
			}
			if err != nil {
				diagnostics.Event(ctx, "warn", "upstream.frame.decode_failed", "frame_bytes", len(data), "gateway", gateway, "error", err)
				continue
			}
		}

		var env struct {
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
			MessageID string          `json:"message_id"`
			Error     json.RawMessage `json:"error"`
			ErrorCode string          `json:"error_code"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
			diagnostics.Event(ctx, "warn", "upstream.event.decode_failed", "frame_bytes", len(data), "error", err)
			continue
		}
		sequence++
		eventCounts[env.Type]++
		if diagnostics.Enabled("debug") {
			diagnostics.Event(ctx, "debug", "upstream.event.received", "sequence", sequence, "event_type", env.Type, "frame_bytes", len(data), "since_start_ms", time.Since(started).Milliseconds(), "metadata", nativeEventMetadata(env.Payload))
		}
		switch env.Type {
		case "tool_result", "text_complete", "message_complete", "artifact_created", "file_created":
			for _, ref := range nativeArtifacts(env.Payload) {
				if !nativeSeen[ref.Path] && len(result.Artifacts) < 20 {
					nativeSeen[ref.Path] = true
					result.Artifacts = append(result.Artifacts, ref)
				}
			}
		}
		switch env.Type {
		case "text_delta":
			delta := deltaText(env.Payload)
			if delta == "" {
				continue
			}
			text.WriteString(delta)
			if firstText {
				firstText = false
				diagnostics.Event(ctx, "info", "upstream.first_text", "latency_ms", time.Since(started).Milliseconds())
			}
			if onEvent != nil {
				if err := onEvent(StreamEvent{Type: "text", Text: delta}); err != nil {
					res, _ := finish()
					return res, err
				}
			}
		case "text_complete":
			if text.Len() == 0 {
				var p struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(env.Payload, &p)
				if p.Text != "" {
					text.WriteString(p.Text)
					if onEvent != nil {
						if err := onEvent(StreamEvent{Type: "text", Text: p.Text}); err != nil {
							res, _ := finish()
							return res, err
						}
					}
				}
			}
			sawMessageComplete = true
		case "turn_usage":
			var p struct {
				InputTokens      int `json:"input_tokens"`
				OutputTokens     int `json:"output_tokens"`
				TotalTokens      int `json:"total_tokens"`
				CacheReadTokens  int `json:"cache_read_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			}
			if err := json.Unmarshal(env.Payload, &p); err == nil {
				result.Usage.add(Usage{
					PromptTokens:     p.InputTokens,
					CompletionTokens: p.OutputTokens,
					TotalTokens:      p.TotalTokens,
					CacheReadTokens:  p.CacheReadTokens,
					CacheWriteTokens: p.CacheWriteTokens,
				})
			}
		case "tool_start":
			var p struct {
				ToolName   string          `json:"tool_name"`
				ToolID     string          `json:"tool_id"`
				ToolCallID string          `json:"tool_call_id"`
				Arguments  json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			if args := argumentString(p.Arguments); args != "{}" {
				tc.Arguments = args
			}
		case "tool_arguments", "tool_arguments_edited":
			var p struct {
				ToolID     string          `json:"tool_id"`
				ToolCallID string          `json:"tool_call_id"`
				ToolName   string          `json:"tool_name"`
				Arguments  json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			if len(p.Arguments) > 0 {
				tc.Arguments = argumentString(p.Arguments)
			}
			if err := emitOrUpdate(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_progress":
			var p struct {
				ToolID     string `json:"tool_id"`
				ToolCallID string `json:"tool_call_id"`
				InputDelta string `json:"input_delta"`
				Progress   struct {
					InputDelta string `json:"input_delta"`
				} `json:"progress"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			delta := p.InputDelta
			if delta == "" {
				delta = p.Progress.InputDelta
			}
			if id != "" && delta != "" {
				partialArgs[id] += delta
			}
		case "tool_result":
			var p struct {
				ToolID     string          `json:"tool_id"`
				ToolCallID string          `json:"tool_call_id"`
				ToolName   string          `json:"tool_name"`
				Arguments  json.RawMessage `json:"arguments"`
				Result     json.RawMessage `json:"result"`
				Error      string          `json:"error"`
				Status     string          `json:"status"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			if len(p.Arguments) > 0 {
				tc.Arguments = argumentString(p.Arguments)
			}
			tc.Result = rawText(p.Result)
			if p.Error != "" {
				tc.Result = p.Error
				tc.Status = "error"
			} else {
				tc.Status = p.Status
			}
			level := "info"
			if tc.Status == "error" {
				level = "warn"
			}
			diagnostics.Event(ctx, level, "upstream.tool.result", "tool_id", tc.ID, "tool_name", tc.Name, "tool_status", tc.Status, "arguments_bytes", len(tc.Arguments), "result_bytes", len(tc.Result), "result_sha256", diagnostics.Fingerprint(tc.Result), "error_message", p.Error)
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_error":
			var p struct {
				ToolID     string `json:"tool_id"`
				ToolCallID string `json:"tool_call_id"`
				ToolName   string `json:"tool_name"`
				Error      string `json:"error"`
				Message    string `json:"message"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			tc.Status = "error"
			tc.Result = p.Error
			if tc.Result == "" {
				tc.Result = p.Message
			}
			diagnostics.Event(ctx, "warn", "upstream.tool.error", "tool_id", tc.ID, "tool_name", tc.Name, "error_message", tc.Result)
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_complete":
			var p struct {
				ToolID     string `json:"tool_id"`
				ToolCallID string `json:"tool_call_id"`
				ToolName   string `json:"tool_name"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			if tc.Status == "" {
				tc.Status = "complete"
			}
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_call", "tool_call_update":
			var p struct {
				ToolCallID      string          `json:"toolCallId"`
				ToolCallIDSnake string          `json:"tool_call_id"`
				ToolID          string          `json:"tool_id"`
				ToolName        string          `json:"tool_name"`
				Title           string          `json:"title"`
				RawInput        json.RawMessage `json:"rawInput"`
				RawInputSnake   json.RawMessage `json:"raw_input"`
				Arguments       json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			if id == "" {
				id = p.ToolCallIDSnake
			}
			name := p.ToolName
			if name == "" {
				name = p.Title
			}
			tc := getTool(id, name)
			args := p.RawInput
			if len(args) == 0 {
				args = p.RawInputSnake
			}
			if len(args) == 0 {
				args = p.Arguments
			}
			if len(args) > 0 {
				tc.Arguments = argumentString(args)
			}
			if err := emitOrUpdate(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "message_complete":
			sawMessageComplete = true
			var p struct {
				ContinuationExpected bool `json:"continuation_expected"`
				ToolCalls            []struct {
					ID         string          `json:"id"`
					ToolCallID string          `json:"tool_call_id"`
					Name       string          `json:"name"`
					ToolName   string          `json:"tool_name"`
					Arguments  json.RawMessage `json:"arguments"`
					Function   struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			for _, item := range p.ToolCalls {
				id := item.ID
				if id == "" {
					id = item.ToolCallID
				}
				name := item.Name
				if name == "" {
					name = item.ToolName
				}
				if name == "" {
					name = item.Function.Name
				}
				tc := getTool(id, name)
				args := item.Arguments
				if len(args) == 0 {
					args = item.Function.Arguments
				}
				if len(args) > 0 {
					tc.Arguments = argumentString(args)
				}
				if err := emitTool(tc); err != nil {
					res, _ := finish()
					return res, err
				}
			}
			if p.ContinuationExpected {
				completeDeadline = time.Time{}
			} else {
				completeDeadline = time.Now().Add(5 * time.Second)
			}
		case "error", "session_error", "stream_error", "turn_failed":
			finishReason = env.Type
			message := errorText(env.Payload, "", "")
			if message == "unknown upstream error" {
				message = errorText(json.RawMessage(jsonStr), "", env.ErrorCode)
			}
			diagnostics.Event(ctx, "error", "upstream.error", "event_type", env.Type, "error_code", env.ErrorCode, "error_message", message, "envelope_metadata", nativeEventMetadata(json.RawMessage(jsonStr)))
			res, _ := finish()
			return res, nativeReplyError(env.Type, message)
		default:
			if terminal[env.Type] {
				finishReason = env.Type
				return finish()
			}
			if env.Type == "ping" {
				// Best effort pong; ignore errors.
				_ = conn.WriteControl(websocket.PongMessage, []byte("pong"), time.Now().Add(3*time.Second))
			}
			if sawMessageComplete && env.Type == "session_list" {
				continue
			}
		}
	}
}

// The existing reader deliberately waits five seconds after message_complete
// for possible continuation. Its own grace timeout is a successful finish.
func expectedStreamEnd(ctx context.Context, err error, complete bool, deadline time.Time) bool {
	if ctx.Err() != nil || !complete || deadline.IsZero() {
		return false
	}
	var networkErr net.Error
	return (errors.As(err, &networkErr) && networkErr.Timeout() && !time.Now().Before(deadline)) || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
}

func deltaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var p struct {
		Delta string `json:"delta"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if p.Delta != "" {
		return p.Delta
	}
	return p.Text
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func errorText(raw json.RawMessage, fallback, code string) string {
	var value any
	_ = json.Unmarshal(raw, &value)
	var find func(any, int) string
	find = func(v any, depth int) string {
		if depth > 6 {
			return ""
		}
		switch x := v.(type) {
		case string:
			return strings.TrimSpace(x)
		case map[string]any:
			// Only inspect error fields; never serialize arbitrary echoed input.
			for _, key := range []string{"message", "detail", "error", "description", "reason", "errors", "msg", "error_code", "code"} {
				if message := find(x[key], depth+1); message != "" {
					return message
				}
			}
		case []any:
			for _, item := range x {
				if message := find(item, depth+1); message != "" {
					return message
				}
			}
		}
		return ""
	}
	for _, s := range []string{find(value, 0), fallback, code} {
		if strings.TrimSpace(s) != "" {
			return diagnostics.RedactText(s, 2048)
		}
	}
	return "unknown upstream error"
}

func titleFor(prompt string) string {
	clean := strings.Join(strings.Fields(prompt), " ")
	if len(clean) > 60 {
		clean = clean[:60] + "…"
	}
	if clean == "" {
		clean = "New Conversation"
	}
	return clean
}

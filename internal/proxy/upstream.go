package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"quickwork-openai/internal/config"
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
	Attachments     []Attachment
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
func (u *Upstream) Chat(ctx context.Context, sess *Session, in ChatInput, onEvent EventFunc) (ChatResult, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return ChatResult{}, fmt.Errorf("prompt is empty")
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

	wsURL, headers, gateway, err := u.dialInfo(sess)
	if err != nil {
		return ChatResult{}, err
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 20 * time.Second
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		detail := ""
		if resp != nil {
			detail = fmt.Sprintf(" HTTP %d", resp.StatusCode)
		}
		return ChatResult{}, fmt.Errorf("upstream WebSocket dial failed%s: %w", detail, err)
	}
	defer conn.Close()

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
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(messageType, frame)
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
				_ = conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
				writeMu.Unlock()
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
	// system_prompt is the native per-turn override. It always carries the
	// caller's system/developer instructions plus the backend-prompt override
	// notice, so instructions the service attaches to the turn lose precedence.
	if strings.TrimSpace(in.SystemPrompt) != "" {
		payload["system_prompt"] = in.SystemPrompt
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

	result, err := u.readEvents(ctx, conn, gateway, onEvent)
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

	if !u.cfg.KeepSessions && !in.KeepSession {
		_ = send(map[string]any{"type": "session_delete", "payload": map[string]any{"session_id": threadID}})
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
			res, _ := finish()
			return res, ctx.Err()
		default:
		}
		readWait := 2 * time.Minute
		if !completeDeadline.IsZero() {
			if remaining := time.Until(completeDeadline); remaining > 0 {
				readWait = remaining
			} else {
				return finish()
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		msgType, data, err := conn.ReadMessage()
		if err != nil {
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
				log.Printf("upstream frame decode: %v", err)
				continue
			}
		}

		var env struct {
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
			MessageID string          `json:"message_id"`
			Error     string          `json:"error"`
			ErrorCode string          `json:"error_code"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
			continue
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
			res, _ := finish()
			return res, fmt.Errorf("upstream %s: %s", env.Type, errorText(env.Payload, env.Error, env.ErrorCode))
		default:
			if terminal[env.Type] {
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
	var p struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Code    string `json:"code"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	for _, s := range []string{p.Message, p.Error, fallback, p.Code, code} {
		if strings.TrimSpace(s) != "" {
			return s
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

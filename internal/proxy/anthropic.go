package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"quickwork-openai/internal/diagnostics"
)

type anthropicRequest struct {
	Model    string `json:"model"`
	System   any    `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema any    `json:"input_schema"`
		Type        string `json:"type"`
	} `json:"tools"`
	ToolChoice struct {
		Type            string `json:"type"`
		Name            string `json:"name"`
		DisableParallel bool   `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	Stream    bool `json:"stream"`
	MaxTokens int  `json:"max_tokens"`
}

func anthropicParts(content any) ([]any, error) {
	if content == nil {
		return nil, nil
	}
	if text, ok := content.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, errors.New("content must be a string or array")
	}
	parts := make([]any, 0, len(blocks))
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("invalid content block")
		}
		switch stringValue(block, "type") {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": stringValue(block, "text")})
		case "thinking", "redacted_thinking": // Historical provider reasoning is not replayed as instructions.
		case "image", "document":
			source, _ := block["source"].(map[string]any)
			kind := stringValue(source, "type")
			mimeType := stringValue(source, "media_type")
			var data string
			switch kind {
			case "base64":
				data = "data:" + mimeType + ";base64," + stringValue(source, "data")
			case "url":
				data = stringValue(source, "url")
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": stringValue(source, "data")})
				continue
			default:
				return nil, fmt.Errorf("unsupported %s source: use base64 or URL", stringValue(block, "type"))
			}
			if stringValue(block, "type") == "image" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": data}})
			} else {
				name := stringValue(block, "title")
				if name == "" {
					name = "document"
				}
				if mimeType == "application/pdf" && !strings.HasSuffix(strings.ToLower(name), ".pdf") {
					name += ".pdf"
				}
				parts = append(parts, map[string]any{"type": "file", "file": map[string]any{"filename": name, "file_data": data}})
			}
		default:
			return nil, fmt.Errorf("unsupported content block: %s", stringValue(block, "type"))
		}
	}
	return parts, nil
}

func (input *anthropicRequest) chatRequest() (ChatRequest, error) {
	out := ChatRequest{Model: input.Model, Stream: input.Stream}
	if strings.TrimSpace(input.Model) == "" || len(input.Messages) == 0 {
		return out, errors.New("model and messages are required")
	}
	if input.System != nil {
		parts, err := anthropicParts(input.System)
		if err != nil {
			return out, err
		}
		out.Messages = append(out.Messages, Message{Role: "system", Content: parts})
	}
	for _, message := range input.Messages {
		if message.Role == "system" {
			parts, err := anthropicParts(message.Content)
			if err != nil {
				return out, err
			}
			for _, part := range parts {
				if block, ok := part.(map[string]any); !ok || block["type"] != "text" {
					return out, errors.New("system messages only support text content")
				}
			}
			out.Messages = append(out.Messages, Message{Role: "system", Content: parts})
			continue
		}
		if message.Role != "user" && message.Role != "assistant" {
			return out, errors.New("messages role must be user, assistant or system")
		}
		blocks, ok := message.Content.([]any)
		if !ok {
			parts, err := anthropicParts(message.Content)
			if err != nil {
				return out, err
			}
			out.Messages = append(out.Messages, Message{Role: message.Role, Content: parts})
			continue
		}
		current := Message{Role: message.Role}
		normal := []any{}
		flush := func() {
			if len(normal) > 0 || len(current.ToolCalls) > 0 {
				current.Content = normal
				out.Messages = append(out.Messages, current)
			}
			current = Message{Role: message.Role}
			normal = nil
		}
		for _, value := range blocks {
			block, ok := value.(map[string]any)
			if !ok {
				return out, errors.New("invalid message block")
			}
			switch stringValue(block, "type") {
			case "tool_use":
				if message.Role != "assistant" || stringValue(block, "id") == "" || stringValue(block, "name") == "" {
					return out, errors.New("tool_use requires assistant role, id and name")
				}
				args, err := json.Marshal(block["input"])
				if err != nil {
					return out, err
				}
				var object map[string]any
				if json.Unmarshal(args, &object) != nil || object == nil {
					return out, errors.New("tool_use.input must be an object")
				}
				current.ToolCalls = append(current.ToolCalls, ToolCall{ID: stringValue(block, "id"), Type: "function", Function: FunctionCall{Name: stringValue(block, "name"), Arguments: string(args)}})
			case "tool_result":
				if message.Role != "user" || stringValue(block, "tool_use_id") == "" {
					return out, errors.New("tool_result requires user role and tool_use_id")
				}
				flush()
				parts, err := anthropicParts(block["content"])
				if err != nil {
					return out, err
				}
				textParts := []any{}
				mediaParts := []any{}
				if failed, _ := block["is_error"].(bool); failed {
					textParts = append(textParts, map[string]any{"type": "text", "text": "Tool execution failed:\n"})
				}
				for _, p := range parts {
					if part, ok := p.(map[string]any); ok && part["type"] != "text" {
						mediaParts = append(mediaParts, p)
					} else {
						textParts = append(textParts, p)
					}
				}
				out.Messages = append(out.Messages, Message{Role: "tool", ToolCallID: stringValue(block, "tool_use_id"), Content: textParts})
				if len(mediaParts) > 0 {
					mediaParts = append([]any{map[string]any{"type": "text", "text": "Attachments returned by client tool " + stringValue(block, "tool_use_id") + ":"}}, mediaParts...)
					out.Messages = append(out.Messages, Message{Role: "user", Content: mediaParts})
				}
			default:
				parts, err := anthropicParts([]any{block})
				if err != nil {
					return out, err
				}
				normal = append(normal, parts...)
			}
		}
		flush()
	}
	for _, tool := range input.Tools {
		if tool.Type != "" && tool.Type != "custom" {
			return out, errors.New("only client tools are supported; server tools and deferred tool search are unavailable")
		}
		if tool.Name == "" || tool.InputSchema == nil {
			return out, errors.New("tool name and input_schema are required")
		}
		out.Tools = append(out.Tools, Tool{Type: "function", Function: ToolFunction{Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema}})
	}
	parallel := !input.ToolChoice.DisableParallel
	out.ParallelToolCalls = &parallel
	switch input.ToolChoice.Type {
	case "", "auto":
		out.ToolChoice = "auto"
	case "none":
		out.ToolChoice = "none"
	case "any":
		out.ToolChoice = "required"
	case "tool":
		if input.ToolChoice.Name == "" {
			return out, errors.New("tool_choice.name is required")
		}
		out.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": input.ToolChoice.Name}}
	default:
		return out, errors.New("unsupported tool_choice.type")
	}
	if input.Stream {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	return out, nil
}

func anthropicError(w http.ResponseWriter, status int, message string) {
	kind := "api_error"
	switch status {
	case 400, 404, 405, 413, 422:
		kind = "invalid_request_error"
	case 401:
		kind = "authentication_error"
	case 403:
		kind = "permission_error"
	case 429:
		kind = "rate_limit_error"
	case 529, 503:
		kind = "overloaded_error"
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
}

func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		anthropicError(w, 405, "POST required")
		return
	}
	if !s.tokens.Allowed(r) {
		anthropicError(w, 401, "invalid proxy API key")
		return
	}
	var input anthropicRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBody))
	if err := decoder.Decode(&input); err != nil {
		anthropicError(w, 400, "invalid JSON body: "+err.Error())
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		anthropicError(w, 400, "request must contain one JSON object")
		return
	}
	chat, err := input.chatRequest()
	if err != nil {
		anthropicError(w, 400, err.Error())
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/count_tokens") && input.MaxTokens < 1 {
		anthropicError(w, 400, "max_tokens must be positive")
		return
	}
	if w.Header().Get("request-id") == "" {
		w.Header().Set("request-id", "req_"+strings.ReplaceAll(uuid(), "-", ""))
	}
	w.Header().Set("X-Quickwork-Compatibility", "anthropic-messages")
	diagnostics.Event(r.Context(), "info", "anthropic.request.converted", "requested_model", input.Model, "mapped_model", s.mapModel(input.Model), "stream", input.Stream, "client_tools", len(chat.Tools), "messages", len(chat.Messages), "requested_max_tokens", input.MaxTokens, "max_tokens_enforced", false, "system_present", input.System != nil, "tool_transport", "quick_text_rpc")
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		// Quick has no public tokenizer. This local estimate is explicitly labelled
		// and does not make an upstream request or pretend to use Claude's tokenizer.
		w.Header().Set("X-Token-Count-Method", "estimate")
		writeJSON(w, 200, map[string]any{"input_tokens": estimateAnthropic(input)})
		return
	}
	body, _ := json.Marshal(chat)
	forward := r.Clone(r.Context())
	forward.Body = io.NopCloser(bytes.NewReader(body))
	forward.ContentLength = int64(len(body))
	forward.Header = r.Header.Clone()
	forward.Header.Set("Content-Type", "application/json")
	translator := newAnthropicWriter(w, s.mapModel(input.Model), input.Stream)
	var stopPing, pingDone chan struct{}
	if input.Stream {
		translator.start()
		stopPing = make(chan struct{})
		pingDone = make(chan struct{})
		defer func() { close(stopPing); <-pingDone }()
		go func() {
			defer close(pingDone)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopPing:
					return
				case <-r.Context().Done():
					return
				case <-ticker.C:
					translator.event("ping", map[string]any{})
				}
			}
		}()
	}
	s.handleChat(translator, forward)
	translator.finish()
}

func estimateAnthropic(input anthropicRequest) int {
	ascii, wide, media := 0, 0, 0
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			for _, r := range x {
				if r < 128 {
					ascii++
				} else {
					wide++
				}
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		case map[string]any:
			if x["type"] == "image" || x["type"] == "document" {
				media += 2048
				return
			}
			for key, child := range x {
				ascii += utf8.RuneCountInString(key)
				walk(child)
			}
		}
	}
	raw, _ := json.Marshal(input)
	var root any
	_ = json.Unmarshal(raw, &root)
	walk(root)
	return max(1, (ascii+3)/4+wide+media+len(input.Messages)*8)
}

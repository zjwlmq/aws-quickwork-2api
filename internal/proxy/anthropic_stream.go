package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type chatAttachment struct {
	Filename string `json:"filename"`
	URL      string `json:"url"`
}
type chatEnvelope struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content     *string          `json:"content"`
			ToolCalls   []ToolCall       `json:"tool_calls"`
			Attachments []chatAttachment `json:"attachments"`
		} `json:"message"`
		Delta struct {
			Content     string           `json:"content"`
			Attachments []chatAttachment `json:"attachments"`
			ToolCalls   []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type anthropicWriter struct {
	wireMu                    sync.Mutex
	target                    http.ResponseWriter
	headers                   http.Header
	status                    int
	stream                    bool
	model, id                 string
	buffer                    bytes.Buffer
	started, ended, textOpen  bool
	index                     int
	tools                     map[int]*ToolCall
	files                     []chatAttachment
	finishReason              string
	inputTokens, outputTokens int
}

func newAnthropicWriter(w http.ResponseWriter, model string, stream bool) *anthropicWriter {
	return &anthropicWriter{target: w, headers: make(http.Header), model: model, id: "msg_" + strings.ReplaceAll(uuid(), "-", ""), stream: stream, tools: map[int]*ToolCall{}}
}
func (a *anthropicWriter) Header() http.Header { return a.headers }
func (a *anthropicWriter) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}
func (a *anthropicWriter) Write(p []byte) (int, error) {
	if a.status == 0 {
		a.status = 200
	}
	if !a.stream || a.status >= 400 {
		_, err := a.buffer.Write(p)
		return len(p), err
	}
	_, _ = a.buffer.Write(p)
	for {
		raw := a.buffer.Bytes()
		end := bytes.Index(raw, []byte("\n\n"))
		if end < 0 {
			break
		}
		event := string(raw[:end])
		a.buffer.Next(end + 2)
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "data: ") {
				a.chunk(strings.TrimPrefix(line, "data: "))
			}
		}
	}
	return len(p), nil
}
func (a *anthropicWriter) Flush() {
	a.wireMu.Lock()
	defer a.wireMu.Unlock()
	a.flushTarget()
}
func (a *anthropicWriter) flushTarget() {
	if a.started {
		if f, ok := a.target.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func (a *anthropicWriter) event(kind string, data map[string]any) {
	a.wireMu.Lock()
	defer a.wireMu.Unlock()
	if a.ended {
		return
	}
	data["type"] = kind
	body, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(a.target, "event: %s\ndata: %s\n\n", kind, body)
	a.flushTarget()
}
func (a *anthropicWriter) markEnded() { a.wireMu.Lock(); a.ended = true; a.wireMu.Unlock() }
func (a *anthropicWriter) message(content []any, reason any) map[string]any {
	return map[string]any{"id": a.id, "type": "message", "role": "assistant", "model": a.model, "content": content, "stop_reason": reason, "stop_sequence": nil, "usage": map[string]any{"input_tokens": a.inputTokens, "output_tokens": a.outputTokens, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}}
}
func (a *anthropicWriter) start() {
	if a.started {
		return
	}
	a.started = true
	a.target.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	a.target.Header().Set("Cache-Control", "no-cache")
	a.target.Header().Set("X-Accel-Buffering", "no")
	a.target.WriteHeader(200)
	a.event("message_start", map[string]any{"message": a.message([]any{}, nil)})
}
func (a *anthropicWriter) closeText() {
	if a.textOpen {
		a.event("content_block_stop", map[string]any{"index": a.index})
		a.index++
		a.textOpen = false
	}
}
func toolBlock(call ToolCall) (map[string]any, error) {
	var input map[string]any
	if json.Unmarshal([]byte(call.Function.Arguments), &input) != nil || input == nil {
		return nil, fmt.Errorf("upstream returned invalid arguments for %s", call.Function.Name)
	}
	if call.ID == "" {
		call.ID = "toolu_" + strings.ReplaceAll(uuid(), "-", "")
	}
	if call.Function.Name == "" {
		return nil, fmt.Errorf("upstream returned an unnamed tool call")
	}
	return map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input}, nil
}

func (a *anthropicWriter) chunk(raw string) {
	if a.ended {
		return
	}
	if raw == "[DONE]" {
		a.endStream()
		return
	}
	var chunk chatEnvelope
	if json.Unmarshal([]byte(raw), &chunk) != nil {
		a.fail("invalid upstream streaming response")
		return
	}
	if chunk.Error.Message != "" {
		a.fail(chunk.Error.Message)
		return
	}
	a.inputTokens = max(a.inputTokens, chunk.Usage.Prompt)
	a.outputTokens = max(a.outputTokens, chunk.Usage.Completion)
	a.start()
	for _, choice := range chunk.Choices {
		a.files = append(a.files, choice.Delta.Attachments...)
		if choice.Delta.Content != "" {
			if !a.textOpen {
				a.textOpen = true
				a.event("content_block_start", map[string]any{"index": a.index, "content_block": map[string]any{"type": "text", "text": ""}})
			}
			a.event("content_block_delta", map[string]any{"index": a.index, "delta": map[string]any{"type": "text_delta", "text": choice.Delta.Content}})
		}
		for _, tc := range choice.Delta.ToolCalls {
			call := a.tools[tc.Index]
			if call == nil {
				call = &ToolCall{Type: "function"}
				a.tools[tc.Index] = call
			}
			if tc.ID != "" {
				call.ID = tc.ID
			}
			if tc.Function.Name != "" {
				call.Function.Name = tc.Function.Name
			}
			call.Function.Arguments += tc.Function.Arguments
		}
		if choice.FinishReason != nil {
			a.finishReason = *choice.FinishReason
		}
	}
}
func (a *anthropicWriter) fail(message string) {
	if !a.started {
		anthropicError(a.target, 502, message)
	} else {
		a.event("error", map[string]any{"error": map[string]any{"type": "api_error", "message": message}})
	}
	a.markEnded()
}
func (a *anthropicWriter) endStream() {
	if a.ended {
		return
	}
	a.start()
	if !a.textOpen && a.index == 0 && len(a.tools) == 0 && len(a.files) > 0 {
		a.textOpen = true
		a.event("content_block_start", map[string]any{"index": a.index, "content_block": map[string]any{"type": "text", "text": ""}})
		for _, file := range a.files {
			a.event("content_block_delta", map[string]any{"index": a.index, "delta": map[string]any{"type": "text_delta", "text": fmt.Sprintf("[%s](%s)\n", file.Filename, file.URL)}})
		}
	}
	a.closeText()
	keys := make([]int, 0, len(a.tools))
	for index := range a.tools {
		keys = append(keys, index)
	}
	sort.Ints(keys)
	for _, key := range keys {
		call := a.tools[key]
		block, err := toolBlock(*call)
		if err != nil {
			a.fail(err.Error())
			return
		}
		block["input"] = map[string]any{}
		a.event("content_block_start", map[string]any{"index": a.index, "content_block": block})
		a.event("content_block_delta", map[string]any{"index": a.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments}})
		a.event("content_block_stop", map[string]any{"index": a.index})
		a.index++
	}
	reason := "end_turn"
	if len(keys) > 0 {
		reason = "tool_use"
	} else if a.finishReason == "length" {
		reason = "max_tokens"
	}
	a.event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": map[string]any{"input_tokens": a.inputTokens, "output_tokens": a.outputTokens}})
	a.event("message_stop", map[string]any{})
	a.markEnded()
}
func (a *anthropicWriter) finish() {
	if a.ended {
		return
	}
	if a.stream && a.status > 0 && a.status < 400 {
		a.fail("upstream stream ended without a completion marker")
		return
	}
	var reply chatEnvelope
	if json.Unmarshal(a.buffer.Bytes(), &reply) != nil {
		a.fail("invalid upstream response")
		return
	}
	if a.status >= 400 {
		if a.stream && a.started {
			a.fail(reply.Error.Message)
		} else {
			anthropicError(a.target, a.status, reply.Error.Message)
			a.markEnded()
		}
		return
	}
	if len(reply.Choices) == 0 {
		a.fail("upstream returned no message")
		return
	}
	a.inputTokens = reply.Usage.Prompt
	a.outputTokens = reply.Usage.Completion
	message := reply.Choices[0].Message
	content := []any{}
	if message.Content != nil && *message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": *message.Content})
	}
	for _, tc := range message.ToolCalls {
		block, err := toolBlock(tc)
		if err != nil {
			a.fail(err.Error())
			return
		}
		content = append(content, block)
	}
	if len(content) == 0 && len(message.Attachments) > 0 {
		for _, f := range message.Attachments {
			content = append(content, map[string]any{"type": "text", "text": fmt.Sprintf("[%s](%s)", f.Filename, f.URL)})
		}
	}
	reason := "end_turn"
	if len(message.ToolCalls) > 0 {
		reason = "tool_use"
	}
	writeJSON(a.target, 200, a.message(content, reason))
	a.markEnded()
}

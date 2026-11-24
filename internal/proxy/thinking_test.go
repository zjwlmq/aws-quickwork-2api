package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestClientThinkingStreamsBeforeTextAndToolCalls(t *testing.T) {
	f := newNativeChatFixture(t)
	f.server.cfg.ThinkingEffort = "low"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	thinkingReceived := make(chan struct{}, 1)
	f.answer = func(_ capturedChat, c *websocket.Conn) {
		for _, frame := range []string{
			`{"type":"thinking_start","payload":{}}`,
			`{"type":"thinking_delta","payload":{"delta":"Visible planning note <client_tool_call>"}}`,
			`{"type":"thinking_complete","payload":{"thinking":"completion snapshot"}}`,
		} {
			if err := c.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				return
			}
		}
		select {
		case <-thinkingReceived:
		case <-ctx.Done():
			return
		}
		nativeAnswer(c, `Reading the notes.<client_tool_call>{"name":"Read","arguments":{"path":"README.md"}}</client_tool_call>`)
	}
	api := httptest.NewServer(f.server.Handler())
	defer api.Close()
	defer cancel()
	const body = `{"model":"smart","reasoning_effort":"max","stream":true,"messages":[{"role":"user","content":"Check our notes."}],"tools":[{"type":"function","function":{"name":"Read","parameters":` + bridgeReadSchema + `}}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer key-a")
	resp, err := api.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var thinking, text strings.Builder
	var toolCalls []ToolCall
	var finish string
	sawDone := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		raw, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		if raw == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk struct {
			Error   json.RawMessage
			Choices []struct {
				Finish string `json:"finish_reason"`
				Delta  struct {
					Content   string
					Thinking  string     `json:"reasoning_content"`
					ToolCalls []ToolCall `json:"tool_calls"`
				}
			}
		}
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil || len(chunk.Error) != 0 {
			t.Fatalf("unexpected stream chunk: %s (%v)", raw, err)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Thinking != "" {
				if thinking.Len() == 0 {
					thinkingReceived <- struct{}{}
				}
				thinking.WriteString(choice.Delta.Thinking)
			}
			text.WriteString(choice.Delta.Content)
			toolCalls = append(toolCalls, choice.Delta.ToolCalls...)
			if choice.Finish != "" {
				finish = choice.Finish
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("thinking waited for the reply: %v", err)
	}
	if thinking.String() != "Visible planning note <client_tool_call>" || text.String() != "Reading the notes." || len(toolCalls) != 1 || toolCalls[0].Function.Name != "Read" || finish != "tool_calls" || !sawDone {
		t.Fatalf("thinking, text, and tools were not delivered separately: %q %q %+v %q", thinking.String(), text.String(), toolCalls, finish)
	}
	if len(f.turns) != 1 || f.turns[0].thinkingEffort != "high" {
		t.Fatalf("client max effort did not override the configured default: %+v", f.turns)
	}
}

func TestThinkingEffortRequestAndDefaultOnNativeWire(t *testing.T) {
	f := newNativeChatFixture(t)
	f.server.cfg.ThinkingEffort = "medium"
	f.answer = func(turn capturedChat, c *websocket.Conn) {
		if turn.thinkingEffort != "" && turn.thinkingEffort != "off" {
			_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"thinking_delta","payload":{"text":"Planning note"}}`))
		}
		nativeAnswer(c, "Answer")
	}
	for _, tc := range []struct{ model, requested, want string }{
		{"smart", "xhigh", "high"},
		{"smart", "none", "off"},
		{"balanced", "", "medium"},
		{"balanced", "minimal", "low"},
		{"fast", "high", ""},
	} {
		t.Run(tc.model+"/"+tc.requested, func(t *testing.T) {
			body, err := json.Marshal(ChatRequest{Model: tc.model, ReasoningEffort: tc.requested, Messages: []Message{{Role: "user", Content: "Explain the answer."}}})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer key-a")
			w := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(w, req)
			if w.Code != 200 || f.turns[len(f.turns)-1].thinkingEffort != tc.want {
				t.Fatalf("unexpected native effort or reply: %+v %s", f.turns, w.Body.String())
			}
			hasThinking := strings.Contains(w.Body.String(), `"reasoning_content":"Planning note"`)
			if hasThinking != (tc.want != "" && tc.want != "off") || !strings.Contains(w.Body.String(), `"content":"Answer"`) {
				t.Fatalf("unexpected reasoning output: %s", w.Body.String())
			}
		})
	}
}

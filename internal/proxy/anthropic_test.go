package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicToolHistoryAndMediaConversion(t *testing.T) {
	raw := `{"model":"smart","max_tokens":1000,"system":[{"type":"text","text":"Keep instructions","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"Read","description":"Read a local file","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"Read","disable_parallel_tool_use":true},"messages":[{"role":"user","content":"Read file"},{"role":"assistant","content":[{"type":"thinking","thinking":"old reasoning"},{"type":"text","text":"Reading"},{"type":"tool_use","id":"toolu_test","name":"Read","input":{"file_path":"input.txt"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_test","is_error":true,"content":[{"type":"text","text":"file unavailable"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]}`
	var in anthropicRequest
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	out, err := in.chatRequest()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 5 || out.Messages[0].Role != "system" || out.Messages[2].ToolCalls[0].ID != "toolu_test" || out.Messages[3].Role != "tool" || out.Messages[3].ToolCallID != "toolu_test" || out.Messages[4].Role != "user" {
		t.Fatalf("broken history: %+v", out.Messages)
	}
	if !strings.Contains(contentText(out.Messages[3].Content), "file unavailable") || strings.Contains(contentText(out.Messages[2].Content), "old reasoning") {
		t.Fatal("tool content lost or provider reasoning replayed")
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "Read" || *out.ParallelToolCalls {
		t.Fatal("tool schema/choice changed")
	}
	if mode, name := parseToolChoice(out.ToolChoice); mode != "function" || name != "Read" {
		t.Fatal("tool choice lost")
	}
}

func anthropicEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			out = append(out, event)
		}
	}
	return out
}

func TestAnthropicStreamHandlesFragmentedToolArgumentsAndUsage(t *testing.T) {
	target := httptest.NewRecorder()
	writer := newAnthropicWriter(target, "smart", true)
	payloads := []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"Reading file."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"toolu_read","function":{"name":"Read"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"file_path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"input.txt\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":67}}`,
		`[DONE]`,
	}
	for _, payload := range payloads {
		frame := "data: " + payload + "\n\n"
		for len(frame) > 0 {
			n := min(11, len(frame))
			if _, err := writer.Write([]byte(frame[:n])); err != nil {
				t.Fatal(err)
			}
			frame = frame[n:]
		}
	}
	writer.finish()
	events := anthropicEvents(t, target.Body.String())
	if events[0]["type"] != "message_start" || events[len(events)-1]["type"] != "message_stop" {
		t.Fatal("missing stream lifecycle")
	}
	input := ""
	toolID := ""
	seenStop := false
	for _, event := range events {
		if event["type"] == "error" {
			t.Fatal(event)
		}
		if event["type"] == "content_block_start" {
			block := event["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				toolID = block["id"].(string)
			}
		}
		if event["type"] == "content_block_delta" {
			delta := event["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				input += delta["partial_json"].(string)
			}
		}
		if event["type"] == "message_delta" {
			seenStop = true
			if event["delta"].(map[string]any)["stop_reason"] != "tool_use" || event["usage"].(map[string]any)["input_tokens"] != float64(1234) {
				t.Fatal(event)
			}
		}
	}
	if input != `{"file_path":"input.txt"}` || toolID != "toolu_read" || !seenStop || strings.Contains(target.Body.String(), "[DONE]") {
		t.Fatalf("invalid tool streaming: %s", target.Body.String())
	}
}

func TestAnthropicNonStreamAndErrorEnvelopes(t *testing.T) {
	target := httptest.NewRecorder()
	writer := newAnthropicWriter(target, "smart", false)
	_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Write","arguments":"{\"file_path\":\"output.txt\",\"content\":\"ok\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":6}}`))
	writer.finish()
	var reply map[string]any
	json.Unmarshal(target.Body.Bytes(), &reply)
	if reply["type"] != "message" || reply["stop_reason"] != "tool_use" || reply["content"].([]any)[0].(map[string]any)["input"].(map[string]any)["content"] != "ok" {
		t.Fatal(target.Body.String())
	}
	target = httptest.NewRecorder()
	writer = newAnthropicWriter(target, "smart", true)
	writer.start()
	writer.WriteHeader(401)
	writer.Write([]byte(`{"error":{"message":"upstream unavailable"}}`))
	writer.finish()
	events := anthropicEvents(t, target.Body.String())
	if events[len(events)-1]["type"] != "error" || target.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatal("mixed JSON and SSE error response")
	}
}

func TestAnthropicAuthAndTokenEstimateWithoutUpstream(t *testing.T) {
	server := mediaTestServer(t)
	body := `{"model":"smart","messages":[{"role":"user","content":"Count only; no upstream account exists"}]}`
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "authentication_error") {
			t.Fatal("unauthenticated access", w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	r.Header.Set("X-Api-Key", "first-key")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	var reply struct {
		Input int `json:"input_tokens"`
	}
	json.Unmarshal(w.Body.Bytes(), &reply)
	if w.Code != 200 || reply.Input < 1 || w.Header().Get("X-Token-Count-Method") != "estimate" {
		t.Fatalf("estimate failed: %d %s", w.Code, w.Body.String())
	}
}

func TestAnthropicStreamReturnsGeneratedFileLinks(t *testing.T) {
	target := httptest.NewRecorder()
	writer := newAnthropicWriter(target, "smart", true)
	writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"attachments\":[{\"filename\":\"video.mp4\",\"url\":\"https://example.test/video\"}]}}]}\n\n"))
	writer.Write([]byte("data: [DONE]\n\n"))
	writer.finish()
	events := anthropicEvents(t, target.Body.String())
	found := false
	for _, event := range events {
		if event["type"] == "content_block_delta" {
			found = strings.Contains(event["delta"].(map[string]any)["text"].(string), "https://example.test/video") || found
		}
	}
	if !found || events[len(events)-1]["type"] != "message_stop" {
		t.Fatal("generated file link missing from Anthropic stream")
	}
}

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const bridgeReadSchema = `{"type":"object","properties":{"path":{"type":"string"}}}`

func writeNativeDelta(t *testing.T, c *websocket.Conn, text string) bool {
	t.Helper()
	frame, _ := json.Marshal(map[string]any{"type": "text_delta", "payload": map[string]string{"text": text}})
	return c.WriteMessage(websocket.TextMessage, frame) == nil
}

func TestBridgeStreamsTextBeforeNativeTurnCompletes(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			const before, after = "先读取项目说明。", "读取后再继续。"
			f := newNativeChatFixture(t)
			gotBefore, gotAfter := make(chan struct{}, 1), make(chan struct{}, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			f.answer = func(_ capturedChat, c *websocket.Conn) {
				if !writeNativeDelta(t, c, before+"<client_") {
					return
				}
				// The upstream cannot advance without an actual downstream
				// content frame. A role chunk or keepalive cannot release it.
				select {
				case <-gotBefore:
				case <-ctx.Done():
					return
				}
				for _, delta := range []string{`tool_call>{"name":"Read","arguments":{`, `"path":"README.md"}}</client_tool_`, "call>" + after} {
					if !writeNativeDelta(t, c, delta) {
						return
					}
				}
				select {
				case <-gotAfter:
				case <-ctx.Done():
					return
				}
				_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_usage","payload":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
				_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_completed","payload":{}}`))
			}
			api := httptest.NewServer(f.server.Handler())
			defer api.Close()
			defer cancel()

			const common = `"model":"smart","stream":true,"messages":[{"role":"user","content":"Read the project notes."}],`
			path, body := "/v1/chat/completions", `{`+common+`"stream_options":{"include_usage":true},"tools":[{"type":"function","function":{"name":"Read","parameters":`+bridgeReadSchema+`}}]}`
			done, finish := "data: [DONE]", `"finish_reason":"tool_calls"`
			if protocol == "anthropic" {
				path, body = "/v1/messages", `{`+common+`"max_tokens":100,"tools":[{"name":"Read","input_schema":`+bridgeReadSchema+`}]}`
				done, finish = "event: message_stop", `"stop_reason":"tool_use"`
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer key-a")
			resp, err := api.Client().Do(req)
			if err != nil {
				t.Fatalf("response held while native turn waited for first content: %v", err)
			}
			defer resp.Body.Close()
			scanner := bufio.NewScanner(resp.Body)
			var wire strings.Builder
			sawBefore, sawAfter, sawDone := false, false, false
			for scanner.Scan() {
				line := scanner.Text()
				wire.WriteString(line + "\n")
				if strings.Contains(line, `"name":"Read"`) && !sawAfter {
					t.Fatal("tool call sent before the native turn finished")
				}
				if strings.Contains(line, before) && !sawBefore {
					sawBefore = true
					gotBefore <- struct{}{}
				}
				if strings.Contains(line, after) && !sawAfter {
					sawAfter = true
					gotAfter <- struct{}{}
				}
				sawDone = sawDone || line == done
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("could not read all content while the native turn was open: %v", err)
			}
			full := wire.String()
			if resp.StatusCode != 200 || !sawBefore || !sawAfter || !sawDone || !strings.Contains(full, finish) || !strings.Contains(full, "README.md") || strings.Count(full, `"name":"Read"`) != 1 || strings.Contains(full, `"error":`) || strings.Contains(full, "client_tool_") {
				t.Fatalf("unexpected streaming reply: HTTP %d %s", resp.StatusCode, full)
			}
			if protocol == "openai" {
				if !strings.Contains(full, `"completion_tokens":5`) {
					t.Fatalf("final usage missing: %s", full)
				}
				row, err := f.store.GetChatSession(context.Background(), ownerKey("key-a"), resp.Header.Get("X-Thread-Id"))
				if err != nil || row.Phase != "ready" {
					t.Fatalf("streamed turn was not committed: %+v %v", row, err)
				}
			}
		})
	}
}

func TestBridgeKeepsUnfinishedOrInterruptedCallsOffTheWire(t *testing.T) {
	for _, tc := range []struct {
		name, generated string
		completed       bool
	}{
		{"missing delimiter", `<client_tool_call>{"name":"Read","arguments":{"path":"PRIVATE_ARGUMENT"}}`, true},
		{"invalid encoded arguments", `<client_tool_call>{"name":"Read","arguments":"{PRIVATE_ARGUMENT"}</client_tool_call>`, true},
		{"socket closed after a complete call", `<client_tool_call>{"name":"Read","arguments":{"path":"PRIVATE_ARGUMENT"}}</client_tool_call>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeChatFixture(t)
			f.answer = func(_ capturedChat, c *websocket.Conn) {
				if writeNativeDelta(t, c, "Preparing. "+tc.generated) && tc.completed {
					_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_completed","payload":{}}`))
				}
			}
			tools := []Tool{{Type: "function", Function: ToolFunction{Name: "Read", Parameters: json.RawMessage(bridgeReadSchema)}}}
			w, reply := f.request(t, []Message{{Role: "user", Content: "Read the project notes."}}, "key-a", "", true, tools)
			if contentText(reply.Content) != "Preparing. " || len(reply.ToolCalls) != 0 || w.Code != 200 || !strings.Contains(w.Body.String(), `"code":"upstream_error"`) || strings.Contains(w.Body.String(), "PRIVATE_ARGUMENT") || strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
				t.Fatalf("incomplete turn was delivered as a valid call: HTTP %d %s", w.Code, w.Body.String())
			}
			row, err := f.store.GetChatSession(context.Background(), ownerKey("key-a"), w.Header().Get("X-Thread-Id"))
			if err != nil || row.Phase != "retired" {
				t.Fatalf("interrupted turn was saved as resumable: %+v %v", row, err)
			}
		})
	}
}

func TestToolStreamRecognizesSplitMarkersAndLegacyJSON(t *testing.T) {
	const literal = "这段参数中的 </client_tool_call> 是原文。"
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{"name": "Read", "arguments": map[string]string{"path": literal}}); err != nil {
		t.Fatal(err)
	}
	object := strings.TrimSpace(encoded.String())
	quoted, _ := json.Marshal(object)
	escaped := string(quoted[1 : len(quoted)-1])
	for _, tc := range []struct {
		name, input, prose string
		wantCalls          int
	}{
		{"tag embedded in argument", "前。" + clientToolCallOpen + object + clientToolCallClose + "后。", "前。后。", 1},
		{"escaped marker body", clientToolCallOpen + escaped + clientToolCallClose, "", 1},
		{"bare array", "[" + object + "]", "", 1},
		{"fenced bare call", "\n```json\n" + object + "\n```\n", "", 1},
		{"ordinary Markdown", "[项目说明](README.md)\n", "[项目说明](README.md)\n", 0},
		{"ordinary JSON", `{"result":42} 已确认。`, `{"result":42} 已确认。`, 0},
		{"partial opening tag as text", "字面量 <cl", "字面量 <cl", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var text strings.Builder
			stream := clientToolStream{allowed: map[string]bool{"Read": true}, checkBare: true, emitText: func(chunk string) { text.WriteString(chunk) }}
			for _, ch := range tc.input {
				stream.push(string(ch))
			}
			calls, err := stream.finish()
			if err != nil || text.String() != tc.prose || len(calls) != tc.wantCalls {
				t.Fatalf("unexpected tool and text separation: %q %+v %v", text.String(), calls, err)
			}
			for _, call := range calls {
				var args map[string]string
				if json.Unmarshal([]byte(call.Arguments), &args) != nil || args["path"] != literal {
					t.Fatalf("tool argument changed: %s", call.Arguments)
				}
			}
		})
	}
}

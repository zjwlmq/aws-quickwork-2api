package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"quickwork-openai/internal/config"
)

func TestLongStreamingTurnKeepsClientAliveBeforeToolResult(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			f := newNativeChatFixture(t)
			f.server.streamKeepAliveInterval = 10 * time.Millisecond
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			f.answer = func(_ capturedChat, c *websocket.Conn) {
				select {
				case <-release:
				case <-time.After(2 * time.Second):
					return
				}
				nativeAnswer(c, `<client_tool_call>{"name":"Read","arguments":{"path":"a.go"}}</client_tool_call>`)
			}
			api := httptest.NewServer(f.server.Handler())
			defer api.Close()
			body := `{"model":"smart","stream":true,"messages":[{"role":"user","content":"Read a.go using the client tool"}],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
			path, ping, done := "/v1/chat/completions", ": keep-alive", "data: [DONE]"
			if protocol == "anthropic" {
				body = `{"model":"smart","stream":true,"max_tokens":1000,"messages":[{"role":"user","content":"Read a.go using the client tool"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`
				path, ping, done = "/v1/messages", "event: ping", "event: message_stop"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer key-a")
			response, err := api.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			scanner := bufio.NewScanner(response.Body)
			var wire strings.Builder
			sawPing, sawDone := false, false
			for scanner.Scan() {
				line := scanner.Text()
				wire.WriteString(line + "\n")
				if line == ping {
					sawPing = true
					unblock()
				}
				if line == done {
					sawDone = true
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || !sawPing || !sawDone || !strings.Contains(wire.String(), `"name":"Read"`) {
				t.Fatalf("missing keepalive or completed client tool: HTTP %d %s", response.StatusCode, wire.String())
			}
			if protocol == "openai" && !strings.Contains(wire.String(), `"finish_reason":"tool_calls"`) {
				t.Fatal("client tool finish reason was lost")
			}
			if protocol == "anthropic" && !strings.Contains(wire.String(), `"stop_reason":"tool_use"`) {
				t.Fatal("Anthropic client tool finish reason was lost")
			}
			if protocol == "openai" {
				row, err := f.store.GetChatSession(context.Background(), ownerKey("key-a"), response.Header.Get("X-Thread-Id"))
				if err != nil || row.Phase != "ready" {
					t.Fatalf("completed long turn is not reusable: %+v %v", row, err)
				}
			}
		})
	}
}

func TestInterruptedTurnNeverReturnsPartialBridgeCallAsSuccess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, cause := range []string{"timeout", "disconnect"} {
			t.Run(cause+map[bool]string{false: "_json", true: "_sse"}[stream], func(t *testing.T) {
				f := newNativeChatFixture(t)
				f.server.cfg.RequestTimeout = config.Duration(100 * time.Millisecond)
				f.server.streamKeepAliveInterval = 10 * time.Millisecond
				f.answer = func(_ capturedChat, c *websocket.Conn) {
					partial, _ := json.Marshal(map[string]any{"type": "text_delta", "payload": map[string]any{"text": `<client_tool_call>{"name":"Write","arguments":{"contents":"incomplete`}})
					_ = c.WriteMessage(websocket.TextMessage, partial)
					_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_usage","payload":{"input_tokens":4,"output_tokens":10,"total_tokens":14}}`))
					if cause == "disconnect" {
						// Wait past the first downstream keepalive, then cut off the
						// native stream before any completion event.
						time.Sleep(30 * time.Millisecond)
						return
					}
					for {
						if _, _, err := c.ReadMessage(); err != nil {
							return
						}
					}
				}
				tools := []Tool{{Type: "function", Function: ToolFunction{Name: "Write", Parameters: json.RawMessage(`{"type":"object"}`)}}}
				w, _ := f.request(t, []Message{{Role: "user", Content: "write a large document"}}, "key-a", "", stream, tools)
				code, status := "upstream_error", http.StatusBadGateway
				if cause == "timeout" {
					code, status = "upstream_timeout", http.StatusGatewayTimeout
				}
				if stream {
					status = 200 // The heartbeat has already opened the SSE stream.
				}
				wire := w.Body.String()
				if w.Code != status || !strings.Contains(wire, code) || strings.Contains(wire, `"finish_reason":"stop"`) || strings.Contains(wire, "incomplete") {
					t.Fatalf("partial output was accepted or error lost: HTTP %d %s", w.Code, wire)
				}
				row, err := f.store.GetChatSession(context.Background(), ownerKey("key-a"), w.Header().Get("X-Thread-Id"))
				if err != nil || row.Phase != "retired" {
					t.Fatalf("uncertain thread remained reusable: %+v %v", row, err)
				}
			})
		}
	}
}

func TestStreamKeepAliveWriteFailureCancelsAndJoinsUpstream(t *testing.T) {
	workerDone := make(chan struct{})
	result, err := streamWithKeepAlive(context.Background(), time.Millisecond, func(ctx context.Context, _ EventFunc) (ChatResult, error) {
		defer close(workerDone)
		<-ctx.Done()
		return ChatResult{Usage: Usage{CompletionTokens: 99}}, ctx.Err()
	}, nil, func() error { return io.ErrClosedPipe })
	if !errors.Is(err, io.ErrClosedPipe) || result.Usage.CompletionTokens != 99 {
		t.Fatalf("write error or partial usage lost: %+v %v", result, err)
	}
	select {
	case <-workerDone:
	default:
		t.Fatal("upstream worker outlived the response")
	}
}

func TestStreamKeepAliveWorkerPanicReturnsError(t *testing.T) {
	_, err := streamWithKeepAlive(context.Background(), time.Second, func(context.Context, EventFunc) (ChatResult, error) {
		panic("fixture private input must not be echoed")
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "panicked") || strings.Contains(err.Error(), "private input") {
		t.Fatalf("worker panic was lost or exposed input: %v", err)
	}
}

func TestCompletedTurnAcceptsNormalCloseDuringGrace(t *testing.T) {
	f := newNativeChatFixture(t)
	f.answer = func(_ capturedChat, c *websocket.Conn) {
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"text_complete","payload":{"text":"complete reply"}}`))
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"message_complete","payload":{"continuation_expected":false}}`))
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second))
	}
	w, _ := f.request(t, []Message{{Role: "user", Content: "short request"}}, "key-a", "", true, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"finish_reason":"stop"`) || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("completed response rejected: HTTP %d %s", w.Code, w.Body.String())
	}
}

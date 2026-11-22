package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/store"
)

type capturedChat struct {
	thread, account, prompt, system string
}

type nativeChatFixture struct {
	mu     sync.Mutex
	turns  []capturedChat
	server *Server
	store  *store.Store
	answer func(capturedChat, *websocket.Conn)
}

func newNativeChatFixture(t *testing.T) *nativeChatFixture {
	t.Helper()
	f := &nativeChatFixture{}
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var outer struct{ Payload struct{ Payload string } }
			if json.Unmarshal(body, &outer) != nil {
				continue
			}
			decoded, _ := base64.StdEncoding.DecodeString(outer.Payload.Payload)
			var frame struct {
				Type    string
				Payload map[string]any
			}
			if json.Unmarshal(decoded, &frame) != nil || frame.Type != "user_message" {
				continue
			}
			turn := capturedChat{stringValue(frame.Payload, "thread_id"), r.Header.Get("Authorization"), stringValue(frame.Payload, "text"), stringValue(frame.Payload, "custom_prompt")}
			f.mu.Lock()
			f.turns = append(f.turns, turn)
			answer := f.answer
			f.mu.Unlock()
			if answer != nil {
				answer(turn, conn)
			} else {
				nativeAnswer(conn, "ok")
			}
			return
		}
	}))
	t.Cleanup(ws.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	for _, email := range []string{"one@example.test", "two@example.test"} {
		a := &store.Account{Email: email, TenantURL: ws.URL, WSHost: "ws" + strings.TrimPrefix(ws.URL, "http"), WSPath: "/wire", Provider: "social", AccessToken: email}
		if err := st.Save(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	f.store, f.server = st, NewServer(&cfg, accounts.New(st, &cfg), st)
	// Cleanup has separate tests and must not race assertions about retired rows.
	f.server.chatCleanupRunning.Store(true)
	return f
}

func nativeAnswer(c *websocket.Conn, text string) {
	body, _ := json.Marshal(map[string]any{"type": "text_complete", "payload": map[string]any{"text": text}})
	_ = c.WriteMessage(websocket.TextMessage, body)
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_completed","payload":{}}`))
}

func (f *nativeChatFixture) request(t *testing.T, history []Message, key, thread string, stream bool, tools []Tool) (*httptest.ResponseRecorder, Message) {
	t.Helper()
	body, _ := json.Marshal(ChatRequest{Model: "smart", Messages: history, Tools: tools, Stream: stream})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-Thread-Id", thread)
	w := httptest.NewRecorder()
	f.server.handleChat(w, r)
	if w.Code != 200 {
		return w, Message{}
	}
	if stream {
		observer := &chatReplyObserver{}
		recorder := observeChatWriter(httptest.NewRecorder(), observer, true)
		_, _ = recorder.Write(w.Body.Bytes())
		return w, observer.message()
	}
	var v struct{ Choices []struct{ Message Message } }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || len(v.Choices) != 1 {
		t.Fatalf("invalid response: %s", w.Body.String())
	}
	return w, v.Choices[0].Message
}

func TestNativeChatBindsAccountThreadAndOnlySendsNewMessages(t *testing.T) {
	f := newNativeChatFixture(t)
	history := []Message{{Role: "system", Content: "project-rule"}, {Role: "user", Content: "remember-first"}}
	w, a := f.request(t, history, "key-a", "", false, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	history = append(history, a, Message{Role: "user", Content: "second-only"})
	w, a = f.request(t, history, "key-a", w.Header().Get("X-Thread-Id"), true, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	history = append(history, a, Message{Role: "user", Content: "third-only"})
	w, _ = f.request(t, history, "key-a", "", false, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(f.turns) != 3 {
		t.Fatal(len(f.turns))
	}
	for i := 1; i < 3; i++ {
		if f.turns[i].thread != f.turns[0].thread || f.turns[i].account != f.turns[0].account || strings.Contains(f.turns[i].prompt, "remember-first") || !strings.Contains(f.turns[i].system, "project-rule") {
			t.Fatalf("not incremental/pinned: %+v", f.turns)
		}
	}
}

func TestNativeChatForksEditedHistoryContextAndOwner(t *testing.T) {
	f := newNativeChatFixture(t)
	initial := []Message{{Role: "user", Content: "first"}}
	w, a := f.request(t, initial, "a", "shared", false, nil)
	continued := append(append([]Message(nil), initial...), a, Message{Role: "user", Content: "next"})
	_, _ = f.request(t, continued, "b", "shared", false, nil)
	edited := append([]Message(nil), continued...)
	edited[0].Content = "edited"
	_, _ = f.request(t, edited, "a", "shared", false, nil)
	changed := append([]Message{{Role: "system", Content: "changed rule"}}, continued...)
	_, _ = f.request(t, changed, "a", "shared", false, nil)
	_, _ = f.request(t, continued, "a", w.Header().Get("X-Thread-Id"), false, nil)
	if len(f.turns) != 5 {
		t.Fatal(len(f.turns))
	}
	for _, i := range []int{1, 2, 3} {
		if f.turns[i].thread == f.turns[0].thread {
			t.Fatalf("unsafe reuse: %+v", f.turns)
		}
	}
	if f.turns[4].thread != f.turns[0].thread {
		t.Fatal("valid parent history was lost")
	}
}

func TestNativeChatToolResultDeltaPreservesCallIDAndName(t *testing.T) {
	f := newNativeChatFixture(t)
	f.answer = func(turn capturedChat, c *websocket.Conn) {
		if strings.Contains(turn.prompt, "tool-result-marker") {
			nativeAnswer(c, "done")
			return
		}
		nativeAnswer(c, `<client_tool_call>{"name":"Read","arguments":{"path":"a.go"}}</client_tool_call>`)
	}
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "Read", Parameters: map[string]any{"type": "object"}}}}
	history := []Message{{Role: "user", Content: "read file"}}
	w, a := f.request(t, history, "a", "", true, tools)
	if w.Code != 200 || len(a.ToolCalls) != 1 {
		t.Fatalf("missing call: %s", w.Body.String())
	}
	history = append(history, a, Message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "tool-result-marker"})
	w, _ = f.request(t, history, "a", "", false, tools)
	if w.Code != 200 || len(f.turns) != 2 {
		t.Fatal(w.Body.String())
	}
	last := f.turns[1]
	if last.thread != f.turns[0].thread || !strings.Contains(last.prompt, a.ToolCalls[0].ID) || !strings.Contains(last.prompt, "name=Read") || strings.Contains(last.prompt, "Assistant tool call") {
		t.Fatalf("tool result was not incremental/correlated: %+v", last)
	}
}

func TestNativeChatUncertainDisconnectIsNotAutomaticallyReplayed(t *testing.T) {
	f := newNativeChatFixture(t)
	f.answer = func(_ capturedChat, c *websocket.Conn) { c.Close() }
	history := []Message{{Role: "user", Content: "one operation"}}
	w, _ := f.request(t, history, "a", "", false, nil)
	if w.Code != 502 || len(f.turns) != 1 {
		t.Fatalf("request replayed or error lost: %d %d", w.Code, len(f.turns))
	}
	f.mu.Lock()
	f.answer = nil
	f.mu.Unlock()
	w, _ = f.request(t, history, "a", "", false, nil)
	if w.Code != 200 || len(f.turns) != 2 || f.turns[1].thread == f.turns[0].thread {
		t.Fatalf("retry reused uncertain native history: %d %+v", w.Code, f.turns)
	}
}

func TestNativeChatConcurrentBranchAndRestartRecovery(t *testing.T) {
	f := newNativeChatFixture(t)
	history := []Message{{Role: "user", Content: "first"}}
	_, a := f.request(t, history, "a", "", false, nil)
	history = append(history, a, Message{Role: "user", Content: "next"})
	request := &ChatRequest{Messages: history}
	ctx := context.Background()
	one, err := f.server.beginChatTurn(ctx, request, "", ownerKey("a"), "smart", "", "")
	if err != nil || !one.resumed {
		t.Fatalf("parent missing: %v", err)
	}
	two, err := f.server.beginChatTurn(ctx, request, "", ownerKey("a"), "smart", "", "")
	if err != nil || two.resumed || two.row.ThreadID == one.row.ThreadID {
		t.Fatalf("concurrent append shared thread: %v", err)
	}
	if err := f.store.RecoverChatSessions(ctx); err != nil {
		t.Fatal(err)
	}
	three, err := f.server.beginChatTurn(ctx, request, "", ownerKey("a"), "smart", "", "")
	if err != nil || three.resumed {
		t.Fatalf("process restart reused an inflight request: %v", err)
	}
}

func TestNativeChatDeclaredToolDefaultsDoNotForkButArgumentEditsDo(t *testing.T) {
	f := newNativeChatFixture(t)
	f.answer = func(_ capturedChat, c *websocket.Conn) {
		nativeAnswer(c, `<client_tool_call>{"name":"Edit","arguments":{"path":"a.go"}}</client_tool_call>`)
	}
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "Edit", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "replace_all": map[string]any{"type": "boolean", "default": false}}}}}}
	history := []Message{{Role: "user", Content: "edit"}}
	_, a := f.request(t, history, "a", "", false, tools)
	if len(a.ToolCalls) != 1 {
		t.Fatal("missing tool")
	}
	a.ToolCalls[0].Function.Arguments = `{"replace_all":false,"path":"a.go"}`
	history = append(history, a, Message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "done"})
	w, _ := f.request(t, history, "a", "", false, tools)
	if w.Code != 200 || f.turns[0].thread != f.turns[1].thread {
		t.Fatal("declared default caused a branch")
	}
	history[1].ToolCalls[0].Function.Arguments = `{"replace_all":true,"path":"a.go"}`
	w, _ = f.request(t, history, "a", "", false, tools)
	if w.Code != 200 || f.turns[0].thread == f.turns[2].thread {
		t.Fatal("edited arguments reused native history")
	}
}

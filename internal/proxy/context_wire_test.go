package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"path/filepath"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/diagnostics"
)

func TestNativeWireCarriesApplicationContext(t *testing.T) {
	l, logErr := diagnostics.Open(diagnostics.Options{File: filepath.Join(t.TempDir(), "events.jsonl"), Level: "debug", MaxMB: 1, Backups: 1})
	if logErr != nil {
		t.Fatal(logErr)
	}
	previous := diagnostics.SetDefault(l)
	defer func() { diagnostics.SetDefault(previous); l.Close() }()
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			var outer struct {
				Payload struct {
					Payload string `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(body, &outer) != nil {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(outer.Payload.Payload)
			if err != nil {
				continue
			}
			var frame struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			}
			if json.Unmarshal(decoded, &frame) != nil || frame.Type != "user_message" {
				continue
			}
			received <- frame.Payload
			conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"text_complete","payload":{"text":"ok"}}`))
			conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_completed","payload":{}}`))
			return
		}
	}))
	defer server.Close()
	cfg := config.Default()
	ctx, cancel := context.WithTimeout(diagnostics.With(context.Background(), "request_id", "req_wire_fixture"), 3*time.Second)
	defer cancel()
	_, err := NewUpstream(&cfg).Chat(ctx, &Session{JWT: "test-token", WSHost: "ws" + strings.TrimPrefix(server.URL, "http"), WSPath: "/wire", Provider: "social"}, ChatInput{Prompt: "User task", SystemPrompt: "[SYSTEM INSTRUCTIONS]\nproject_nonce=CTX_WIRE_9102", Model: "smart"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, logErr := l.Query(diagnostics.Filter{RequestID: "req_wire_fixture"})
	if logErr != nil {
		t.Fatal(logErr)
	}
	seen := map[string]bool{}
	for _, entry := range events.Entries {
		event, _ := entry["event"].(string)
		seen[event] = true
		if event == "upstream.context.sent" && entry["custom_prompt_present"] != true {
			t.Fatal("system context metadata missing")
		}
		if event == "upstream.turn.finished" && entry["upstream_thread_id"] == nil {
			t.Fatal("native thread not correlated")
		}
	}
	for _, event := range []string{"upstream.context.sent", "upstream.event.received", "upstream.stream.summary", "upstream.turn.finished"} {
		if !seen[event] {
			t.Fatalf("missing trace event: %s", event)
		}
	}
	logJSON, _ := json.Marshal(events)
	if strings.Contains(string(logJSON), "CTX_WIRE_9102") {
		t.Fatal("raw application instructions leaked into logs")
	}
	select {
	case payload := <-received:
		if payload["custom_prompt"] != "[SYSTEM INSTRUCTIONS]\nproject_nonce=CTX_WIRE_9102" {
			t.Fatal("caller instructions did not reach the native context field")
		}
		if _, exists := payload["system_prompt"]; exists {
			t.Fatal("unsupported legacy system_prompt field was sent")
		}
		if payload["text"] != "User task" {
			t.Fatal("application instructions duplicated into conversation text")
		}
	case <-ctx.Done():
		t.Fatal("native user_message was not observed")
	}
}

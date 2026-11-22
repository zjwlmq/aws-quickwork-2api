package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"quickwork-openai/internal/store"
)

func TestNativeErrorShapesPreserveReasonWithoutEchoingInput(t *testing.T) {
	for _, raw := range []string{
		`{"type":"error","message":"custom prompt is too long"}`,
		`{"error":{"message":"custom prompt is too long","code":"invalid_request"}}`,
		`{"detail":[{"msg":"custom prompt is too long","input":"PRIVATE_PROJECT_CONTENT"}]}`,
		`"custom prompt is too long"`,
	} {
		if got := errorText(json.RawMessage(raw), "", ""); got != "custom prompt is too long" {
			t.Fatalf("lost error message for %s: %s", raw, got)
		}
	}
	if got := errorText(json.RawMessage(`{"input":"PRIVATE_PROJECT_CONTENT","type":"error"}`), "", ""); got != "unknown upstream error" || strings.Contains(got, "PRIVATE_PROJECT_CONTENT") {
		t.Fatalf("echoed arbitrary payload: %s", got)
	}
}

func TestNativeContentRefusalIsNotRetriedOrMarkedAsBadAccount(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			var calls atomic.Int32
			native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
						Payload struct{ Payload string } `json:"payload"`
					}
					_ = json.Unmarshal(body, &outer)
					decoded, _ := base64.StdEncoding.DecodeString(outer.Payload.Payload)
					var event struct{ Type string }
					_ = json.Unmarshal(decoded, &event)
					if event.Type != "user_message" {
						continue
					}
					calls.Add(1)
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"message":"The model's content filter stopped this reply. Rephrase, or start a new chat."},"message":"The model's content filter stopped this reply. Rephrase, or start a new chat."}`))
					return
				}
			}))
			defer native.Close()
			server := mediaTestServer(t)
			for _, email := range []string{"one@fixture.test", "two@fixture.test"} {
				if err := server.store.Save(context.Background(), &store.Account{Email: email, Provider: "social", WSHost: "ws" + strings.TrimPrefix(native.URL, "http"), WSPath: "/wire", AccessToken: "fixture-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
					t.Fatal(err)
				}
			}
			body, _ := json.Marshal(map[string]any{"model": "smart", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "harmless request fixture"}}})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer first-key")
			server.handleChat(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "content_filter") || calls.Load() != 1 {
				t.Fatalf("refusal was lost or retried: HTTP %d, %d calls, %s", response.Code, calls.Load(), response.Body.String())
			}
			accounts, err := server.store.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, account := range accounts {
				if account.LastError != "" || account.Disabled {
					t.Fatal("request refusal poisoned a valid account")
				}
			}
		})
	}
}

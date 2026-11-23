package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"quickwork-openai/internal/config"
)

func TestNativeApplicationHeartbeatKeepsLongTurnAlive(t *testing.T) {
	for _, provider := range []string{"social", "gateway"} {
		t.Run(provider, func(t *testing.T) {
			result := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					result <- err
					return
				}
				defer conn.Close()
				// This watchdog sees application data only. Gorilla answers control
				// Ping automatically, just as the upstream transport does.
				started, pings, userMessages := false, 0, 0
				for {
					_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					_, bytes, err := conn.ReadMessage()
					if err != nil {
						result <- fmt.Errorf("application-data watchdog: %w", err)
						return
					}
					var body []byte
					if provider == "gateway" {
						s, err := gatewayDecode(bytes)
						if err != nil {
							result <- err
							return
						}
						body = []byte(s)
					} else {
						var outer struct {
							Payload struct {
								Payload string `json:"payload"`
							} `json:"payload"`
						}
						if err := json.Unmarshal(bytes, &outer); err != nil {
							result <- err
							return
						}
						body, err = base64.StdEncoding.DecodeString(outer.Payload.Payload)
						if err != nil {
							result <- err
							return
						}
					}
					var frame struct {
						Type    string `json:"type"`
						Payload struct {
							Timestamp int64 `json:"timestamp"`
						} `json:"payload"`
					}
					if err := json.Unmarshal(body, &frame); err != nil {
						result <- err
						return
					}
					if frame.Type == "user_message" {
						started = true
						userMessages++
					}
					if frame.Type != "ping" {
						continue
					}
					if frame.Payload.Timestamp <= 0 {
						result <- fmt.Errorf("missing application ping timestamp")
						return
					}
					if started {
						pings++
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong","payload":{}}`))
					if pings < 6 {
						continue
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"text_complete","payload":{"text":"long turn complete"}}`))
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_completed","payload":{}}`))
					if userMessages != 1 {
						result <- fmt.Errorf("user message replayed %d times", userMessages)
					} else {
						result <- nil
					}
					// Drain the concurrent control Ping until the client observes
					// completion and closes; closing with unread data can cause RST.
					for {
						_ = conn.SetReadDeadline(time.Now().Add(time.Second))
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
					}
				}
			}))
			defer server.Close()
			cfg := config.Default()
			upstream := NewUpstream(&cfg)
			upstream.heartbeatInterval = 25 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			reply, err := upstream.Chat(ctx, &Session{JWT: "fixture", Provider: provider, WSHost: "ws" + strings.TrimPrefix(server.URL, "http"), WSPath: "/wire"}, ChatInput{Prompt: "wait for the long turn", Model: "smart", KeepSession: true}, nil)
			if serverErr := <-result; serverErr != nil {
				t.Fatal(serverErr)
			}
			if err != nil || reply.Text != "long turn complete" {
				t.Fatalf("long turn failed: %+v %v", reply, err)
			}
		})
	}
}

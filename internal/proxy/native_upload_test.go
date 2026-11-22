package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"quickwork-openai/internal/config"
)

func TestNativeAttachmentUploadChecksDLPAndBindsDeliveryToThread(t *testing.T) {
	for _, mode := range []string{"allow", "block", "wrong-thread", "relative"} {
		t.Run(mode, func(t *testing.T) {
			previous := nativeHTTP
			t.Cleanup(func() { nativeHTTP = previous })
			data := bytes.Repeat([]byte("uncompressed attachment bytes\n"), 5000)
			calls, delivered := 0, false
			nativeHTTP = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer private-token" {
					t.Error("wrong account authentication")
				}
				var value any
				switch r.URL.Path {
				case "/integration/quick-work/upload-attachment":
					if r.URL.Host != "tenant.test" || r.Method != "POST" || r.Header.Get("Content-Type") != "application/octet-stream" {
						t.Error("wrong upload endpoint")
					}
					body, _ := io.ReadAll(r.Body)
					if !bytes.Equal(body, data) {
						t.Error("upload bytes changed")
					}
					value = map[string]any{"attachmentId": "native-id"}
				case "/integration/quick-work/attachment-dlp-status":
					action := "ALLOW"
					if mode == "block" {
						action = "BLOCK"
					}
					value = map[string]any{"dlpJobStatus": "COMPLETED", "policyAction": action}
				case "/integration/quick-work/qbs-event":
					delivered = true
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					agent := body["eventBody"].(map[string]any)["invokeAgent"].(map[string]any)
					if r.URL.Host != "ws.test" || agent["path"] != "files/upload/bound-thread" || agent["body"].(map[string]any)["attachmentId"] != "native-id" {
						t.Error("delivery lost thread or attachment ID")
					}
					filePath := "/data/users/u/sessions/bound-thread/workspace/attached_files/input.json"
					if mode == "wrong-thread" {
						filePath = strings.Replace(filePath, "bound-thread", "other-thread", 1)
					}
					if mode == "relative" {
						filePath = ""
					}
					value = map[string]any{"workspace_path": "workspace/attached_files/input.json", "path": filePath}
				default:
					t.Error("unexpected endpoint", r.URL.Path)
				}
				body, _ := json.Marshal(value)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
			})}
			cfg := config.Default()
			a, err := NewUpstream(&cfg).uploadAttachment(context.Background(), &Session{JWT: "private-token", TenantURL: "https://tenant.test", WSHost: "wss://ws.test"}, "bound-thread", Attachment{Name: "input.json", Size: int64(len(data)), MIMEType: "application/json"}, data)
			if mode == "block" {
				if err == nil || delivered || calls != 2 || !requestRefused(err) {
					t.Fatalf("blocked file was delivered: %v", err)
				}
				return
			}
			if mode == "wrong-thread" {
				if err == nil {
					t.Fatal("other thread's path accepted")
				}
				return
			}
			if err != nil || !delivered || a.Data != "" || a.AttachmentID != "native-id" || !strings.Contains(a.Path, "workspace/attached_files/input.json") {
				t.Fatalf("upload failed: %+v %v", a, err)
			}
		})
	}
}

func TestUploadFailureCleansUnusedNativeThreadWithoutDispatch(t *testing.T) {
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer ws.Close()
	previous := nativeHTTP
	t.Cleanup(func() { nativeHTTP = previous })
	created, deleted, dispatched := false, false, false
	nativeHTTP = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `{}`
		if r.URL.Path == "/integration/quick-work/upload-attachment" {
			status = 503
			body = `{"errorMessage":"Temporary upload failure"}`
		} else {
			var request struct {
				EventBody struct {
					InvokeAgent struct{ Body struct{ Type string } }
				}
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			switch request.EventBody.InvokeAgent.Body.Type {
			case "session_create":
				created = true
				body = `{"frames":[{"type":"session_created"}]}`
			case "session_delete":
				deleted = true
				body = `{"frames":[{"type":"session_deleted"}]}`
			default:
				t.Error("unexpected native operation")
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	cfg := config.Default()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := NewUpstream(&cfg).Chat(ctx, &Session{JWT: "fixture", Provider: "social", WSHost: "ws" + strings.TrimPrefix(ws.URL, "http"), WSPath: "/wire", TenantURL: "https://tenant.test"}, ChatInput{Prompt: strings.Repeat("reference ", 10000), Model: "smart", ThreadID: "fresh-thread", KeepSession: true, OnDispatch: func() { dispatched = true }}, nil)
	if err == nil || !strings.Contains(err.Error(), "Temporary upload failure") || !created || !deleted || dispatched {
		t.Fatalf("unsafe upload retry state: created=%v deleted=%v dispatched=%v err=%v", created, deleted, dispatched, err)
	}
}

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestArtifactDownloadReissuesURLAndBoundsRejections(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		failures    int
		wantCalls   int
		wantSuccess bool
	}{
		{"403_then_published", 403, 2, 3, true},
		{"404_then_published", 404, 1, 2, true},
		{"persistent_403", 403, 10, 4, false},
		{"authentication_error", 401, 10, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := mediaTestServer(t)
			s.artifactRetryDelay = time.Millisecond
			previousNative, previousMedia := nativeHTTP, mediaHTTPClient
			t.Cleanup(func() { nativeHTTP, mediaHTTPClient = previousNative, previousMedia })
			nativeCalls, mediaCalls := 0, 0
			nativeHTTP = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				nativeCalls++
				if r.Header.Get("Authorization") != "Bearer fixture-upstream-token" {
					t.Error("missing native authentication")
				}
				body := fmt.Sprintf(`{"body":{"downloadUrl":"https://files.example.test/drawing.svg?signature=url-%d"}}`, nativeCalls)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			mediaHTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				mediaCalls++
				if r.Header.Get("Authorization") != "" {
					t.Error("native token leaked to download host")
				}
				if r.URL.Query().Get("signature") != "url-"+strconv.Itoa(mediaCalls) {
					t.Error("reused rejected download signature")
				}
				status, body := 200, `<svg xmlns="http://www.w3.org/2000/svg"><circle r="5"/></svg>`
				if mediaCalls <= test.failures {
					status, body = test.status, `<Error><Code>AccessDenied</Code><Message>Artifact unavailable</Message></Error>`
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			files, err := s.cacheArtifacts(context.Background(), &Session{AccountID: 1, WSHost: "tenant.example.test", JWT: "fixture-upstream-token"}, ownerKey("first-key"), "fixture-thread", []NativeArtifact{{Path: "workspace/artifacts/drawing.svg"}})
			if (err == nil) != test.wantSuccess || nativeCalls != test.wantCalls || mediaCalls != test.wantCalls {
				t.Fatalf("unexpected recovery: files=%d error=%v signatures=%d downloads=%d", len(files), err, nativeCalls, mediaCalls)
			}
			if test.wantSuccess {
				if len(files) != 1 {
					t.Fatal("recovered file was not saved")
				}
				data, err := os.ReadFile(s.mediaDiskPath(files[0].ID))
				if err != nil || !bytes.Contains(data, []byte("<svg")) {
					t.Fatalf("downloaded bytes lost: %v", err)
				}
			} else if entries, err := os.ReadDir(s.mediaDir()); err != nil || len(entries) != 0 {
				t.Fatalf("rejected download left partial files: %v %v", entries, err)
			}
		})
	}
}

func TestInternalInspectionImageDoesNotBlockClientWrite(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			f := newNativeChatFixture(t)
			f.answer = func(_ capturedChat, c *websocket.Conn) {
				_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"tool_result","payload":{"tool_id":"native-image","tool_name":"file_read_image","status":"complete","artifact":{"path":"workspace/artifacts/.metadata/_images/read_image_001.png"}}}`))
				nativeAnswer(c, `<client_tool_call>{"name":"Write","arguments":{"path":"drawing.svg","contents":"<svg xmlns=\"http://www.w3.org/2000/svg\"><circle r=\"5\"/></svg>"}}</client_tool_call>`)
			}
			tools := []Tool{{Type: "function", Function: ToolFunction{Name: "Write", Parameters: json.RawMessage(`{"type":"object"}`)}}}
			w, message := f.request(t, []Message{{Role: "user", Content: "Write the SVG in the client project"}}, "key-a", "", stream, tools)
			var args map[string]string
			if len(message.ToolCalls) == 1 {
				_ = json.Unmarshal([]byte(message.ToolCalls[0].Function.Arguments), &args)
			}
			if w.Code != 200 || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "Write" || !strings.Contains(args["contents"], "<svg") {
				t.Fatalf("internal preview blocked client write: HTTP %d %s", w.Code, w.Body.String())
			}
		})
	}
	refs := []NativeArtifact{{Path: "workspace/artifacts/.metadata/_images/read_image_001.png"}, {Path: "workspace/artifacts/drawing.svg"}, {Path: "workspace/artifacts/photo.png"}}
	if got := chatArtifacts(context.Background(), refs, true); len(got) != 2 || got[0].Path != refs[1].Path || got[1].Path != refs[2].Path {
		t.Fatal("real deliverables were filtered")
	}
	if got := chatArtifacts(context.Background(), refs, false); len(got) != len(refs) {
		t.Fatal("native media mode lost its preview")
	}
}

func TestEscapedClientToolJSONPreservesCompleteArguments(t *testing.T) {
	const contents = `<svg xmlns="http://www.w3.org/2000/svg"><text>literal __SVG__ requested by user</text></svg>`
	raw, _ := json.Marshal(map[string]any{"name": "Write", "arguments": map[string]any{"path": `D:\project\drawing.svg`, "contents": contents}})
	quoted, _ := json.Marshal(string(raw))
	for _, payload := range []string{string(raw), string(quoted), string(quoted[1 : len(quoted)-1])} {
		text, calls := parseClientToolCalls(clientToolCallOpen+payload+clientToolCallClose, map[string]bool{"Write": true})
		if text != "" || len(calls) != 1 {
			t.Fatalf("complete escaped tool JSON not decoded: %s %+v", text, calls)
		}
		var args map[string]string
		if json.Unmarshal([]byte(calls[0].Arguments), &args) != nil || args["contents"] != contents || args["path"] != `D:\project\drawing.svg` {
			t.Fatal("file contents or path changed while decoding")
		}
	}
	for _, payload := range []string{`{\"name\":\"Write\",\"arguments\":{\"contents\":\"unfinished`, `{"name":"Shell","arguments":{"command":"whoami"}}`} {
		if _, calls := parseClientToolCalls(clientToolCallOpen+payload+clientToolCallClose, map[string]bool{"Write": true}); len(calls) != 0 {
			t.Fatal("truncated or undeclared operation was accepted")
		}
	}
}

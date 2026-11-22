package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/store"
)

func mediaTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Database = filepath.Join(t.TempDir(), "test.db")
	cfg.APIKeys = []string{"first-key", "second-key"}
	st, err := store.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(&cfg, accounts.New(st, &cfg), st)
	if err = s.InitMedia(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAttachmentsCarryBytesAndDoNotModifyCallerMessages(t *testing.T) {
	data := []byte("Project ORCHID-6142, budget 8361")
	part := map[string]any{"type": "file", "file": map[string]any{"filename": "../notes.txt", "file_data": base64.StdEncoding.EncodeToString(data)}}
	messages := []Message{{Role: "user", Content: []any{map[string]any{"type": "text", "text": "read attached"}, part}}}
	prepared, attachments, err := prepareMedia(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 {
		t.Fatal(attachments)
	}
	actual, err := base64.StdEncoding.DecodeString(attachments[0].Data)
	if err != nil || !bytes.Equal(actual, data) || attachments[0].Size != int64(len(data)) || attachments[0].Name != "01_notes.txt" {
		t.Fatalf("lost file contents: %+v", attachments[0])
	}
	if part["type"] != "file" || !strings.Contains(contentText(prepared[0].Content), "notes.txt") {
		t.Fatal("caller mutated or attachment label missing")
	}
	if attachments[0].ContentSHA256 == "" {
		t.Fatal("missing content checksum")
	}
}

func TestMediaRejectsInternalURLsAndInvalidContent(t *testing.T) {
	for _, source := range []string{"http://127.0.0.1/secret", "http://169.254.169.254/latest/meta-data/", "http://[::1]/", "http://10.0.0.1/", "ftp://public.example/a", "https://public.example:8080/a", "https://user:pass@public.example/a"} {
		if _, _, err := readMedia(context.Background(), source, 100); err == nil {
			t.Errorf("allowed unsafe media URL %s", source)
		}
	}
	if _, err := publicDial(context.Background(), "tcp", "localhost:80"); err == nil {
		t.Fatal("dial allowed loopback DNS")
	}
	for _, part := range []map[string]any{
		{"type": "image_url", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not an image"))},
		{"type": "file", "file": map[string]any{"file_id": "file-other-provider"}},
	} {
		if _, err := parseAttachment(context.Background(), part, 1); err == nil {
			t.Fatal("invalid media accepted")
		}
	}
	if _, _, err := readMedia(context.Background(), "data:text/plain;base64,YWJjZA==", 3); err == nil {
		t.Fatal("size cap not enforced")
	}
}

func TestNativeArtifactsAndEncodedEnvelope(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"result": `<artifact type="canvas" doc_type="video" file_path="workspace/artifacts/.versions/movie.mp4" title="Video" /><artifact type="render_status" data="{&quot;status&quot;:&quot;complete&quot;,&quot;outputPath&quot;:&quot;workspace/artifacts/render.mp4&quot;}" /> [video](qw-file://workspace/artifacts/.versions/movie.mp4)`})
	found := nativeArtifacts(raw)
	paths := map[string]bool{}
	for _, ref := range found {
		paths[ref.Path] = true
	}
	if !paths["workspace/artifacts/.versions/movie.mp4"] || !paths["workspace/artifacts/render.mp4"] {
		t.Fatalf("missing native artifacts: %+v", found)
	}
	for _, p := range []string{"workspace/../secret", "workspace/a/../../secret", "workspace/%2e%2e/secret", "/etc/passwd", "workspace/a\\b"} {
		if _, err := cleanNativePath(p); err == nil {
			t.Errorf("accepted %s", p)
		}
	}
	body := base64.StdEncoding.EncodeToString([]byte(`{"downloadUrl":"https://files.example.test/movie.mp4"}`))
	response, _ := json.Marshal(map[string]any{"body": body, "body_encoding": "base64", "status": 200})
	got, err := unwrapNative(map[string]any{"eventResponse": map[string]any{"invokeAgentResponse": string(response)}})
	if err != nil || got.(map[string]any)["downloadUrl"] != "https://files.example.test/movie.mp4" {
		t.Fatalf("could not unwrap native response: %v %v", got, err)
	}
}

type mediaTransport func(*http.Request) (*http.Response, error)

func (f mediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeDownloadCacheSignaturesAndRanges(t *testing.T) {
	s := mediaTestServer(t)
	ctx := context.Background()
	data := []byte("0000ftyp-video-bytes-for-range-check")
	previousNative, previousMedia := nativeHTTP, mediaHTTPClient
	t.Cleanup(func() { nativeHTTP = previousNative; mediaHTTPClient = previousMedia })
	nativeCalls := 0
	nativeHTTP = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		nativeCalls++
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("native auth missing")
		}
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		agent := payload["eventBody"].(map[string]any)["invokeAgent"].(map[string]any)
		if agent["path"] != "files/download/thread-1" || agent["queryParams"].(map[string]any)["_method"] != "POST" {
			t.Error("wrong native download request")
		}
		encoded, _ := json.Marshal(map[string]any{"eventResponse": map[string]any{"invokeAgentResponse": map[string]any{"body": `{"downloadUrl":"https://files.example.test/sample.txt"}`}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encoded)), Header: make(http.Header)}, nil
	})}
	mediaHTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Error("upstream credential leaked to file host")
		}
		return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{"Content-Type": []string{"text/plain"}}}, nil
	})}
	session := &Session{AccountID: 1, WSHost: "tenant.example.test", JWT: "upstream-secret"}
	refs := []NativeArtifact{{Path: "workspace/artifacts/sample.txt"}}
	files, err := s.cacheArtifacts(ctx, session, ownerKey("first-key"), "thread-1", refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Size != int64(len(data)) {
		t.Fatalf("missing cached file: %+v", files)
	}
	if _, err = s.cacheArtifacts(ctx, session, ownerKey("first-key"), "thread-1", refs); err != nil || nativeCalls != 1 {
		t.Fatalf("cache miss: %v calls %d", err, nativeCalls)
	}
	request := func(target, key, rangeValue string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		if rangeValue != "" {
			r.Header.Set("Range", rangeValue)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if got := request(files[0].URL, "", "bytes=4-7"); got.Code != 206 || got.Body.String() != "ftyp" {
		t.Fatalf("signed range playback failed: %d %s", got.Code, got.Body.String())
	}
	plain := "/v1/media/" + files[0].ID
	if got := request(plain, "second-key", ""); got.Code != 401 {
		t.Fatal("other key accessed private file")
	}
	if got := request(plain, "first-key", ""); got.Code != 200 || !bytes.Equal(got.Body.Bytes(), data) {
		t.Fatal("owner could not download")
	}
	if got := request(files[0].URL+"x", "", ""); got.Code != 401 {
		t.Fatal("tampered signature accepted")
	}
	f, err := s.store.GetMedia(ctx, files[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	f.ID = "file-html"
	f.NativePath = "workspace/artifacts/page.html"
	f.Filename = "page.html"
	f.MIMEType = "text/html"
	if err = s.store.SaveMedia(ctx, f); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.mediaDiskPath(f.ID), []byte("<script>alert(1)</script>"), 0600)
	if got := request(s.mediaURL(f), "", ""); got.Code != 200 || !strings.HasPrefix(got.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("active HTML served inline")
	}
}

func TestStreamingNativeLinkSplitAcrossChunks(t *testing.T) {
	var output strings.Builder
	m := mediaStream{emit: func(text string) { output.WriteString(text) }}
	for _, chunk := range []string{"A [video](qw-", "fi", "le://workspace/artifacts/mo", "vie.mp4)", " after"} {
		m.push(chunk, false, nil)
	}
	if strings.Contains(output.String(), "qw-file") {
		t.Fatal("native URL leaked before rewrite")
	}
	m.push("", true, func(link string) string {
		if link != "qw-file://workspace/artifacts/movie.mp4" {
			t.Errorf("bad link %s", link)
		}
		return "https://download.example/video"
	})
	if output.String() != "A [video](https://download.example/video) after" {
		t.Fatal(output.String())
	}
}

func TestVideoJobAuthIsolationAndTerminalFailure(t *testing.T) {
	s := mediaTestServer(t)
	request := func(method, path, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if got := request("POST", "/v1/videos", "", `{"prompt":"hello"}`); got.Code != 401 {
		t.Fatal("unauthenticated video created")
	}
	for _, body := range []string{`{}`, `{"prompt":"test","seconds":31}`, `{"prompt":"test","model":"unavailable-model"}`} {
		if got := request("POST", "/v1/videos", "first-key", body); got.Code != 400 {
			t.Fatalf("bad request accepted: %s %d", body, got.Code)
		}
	}
	got := request("POST", "/v1/videos", "first-key", `{"prompt":"simple animation","seconds":2}`)
	if got.Code != 202 {
		t.Fatalf("not accepted: %d %s", got.Code, got.Body.String())
	}
	var job store.VideoJob
	if err := json.Unmarshal(got.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if got := request("GET", "/v1/videos/"+job.ID, "second-key", ""); got.Code != 404 {
		t.Fatal("job visible to another API key")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := s.store.GetVideo(context.Background(), job.ID)
		if j.Status == "failed" {
			if j.Error == "" || j.CompletedAt == 0 {
				t.Fatal("incomplete terminal state")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job stayed running after no-account failure")
}

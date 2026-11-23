package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

type NativeArtifact struct {
	Path  string
	Kind  string
	Title string
}
type ArtifactHandler func(context.Context, string, []NativeArtifact) ([]store.MediaFile, error)

// file_read_image emits internal inspection thumbnails. In client tool mode
// they are observations used by Quick, not files requested by the API caller.
func chatArtifacts(ctx context.Context, refs []NativeArtifact, clientTools bool) []NativeArtifact {
	if !clientTools {
		return refs
	}
	out := make([]NativeArtifact, 0, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref.Path, "workspace/artifacts/.metadata/_images/") {
			diagnostics.Event(ctx, "info", "media.preview.skipped", "native_path_sha256", diagnostics.Fingerprint(ref.Path), "reason", "internal_tool_inspection")
			continue
		}
		out = append(out, ref)
	}
	return out
}

var nativeLinks = regexp.MustCompile(`qw-file://([^\s\x60"'<>\)\]]+)`)
var nativeTags = regexp.MustCompile(`<artifact\b[^>]*>`)
var tagAttributes = regexp.MustCompile(`([a-zA-Z_]+)=["']([^"']*)["']`)

func cleanNativePath(p string) (string, error) {
	p = strings.TrimPrefix(p, "qw-file://")
	if decoded, err := url.PathUnescape(p); err == nil {
		p = decoded
	}
	if !strings.HasPrefix(p, "workspace/") || strings.Contains(p, "\\") || strings.ContainsAny(p, "\x00\r\n") || len(p) > 2048 {
		return "", errors.New("无效的 Quick 工作区文件路径")
	}
	clean := path.Clean(p)
	if clean != p || !strings.HasPrefix(clean, "workspace/") {
		return "", errors.New("无效的 Quick 工作区文件路径")
	}
	return clean, nil
}

func nativeArtifacts(raw json.RawMessage) []NativeArtifact {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	out := make([]NativeArtifact, 0)
	add := func(p, kind, title string) {
		if p, err := cleanNativePath(p); err == nil && len(out) < 20 {
			out = append(out, NativeArtifact{Path: p, Kind: kind, Title: title})
		}
	}
	var walk func(any, int)
	walk = func(v any, depth int) {
		if depth > 6 {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			for _, k := range []string{"path", "file_path", "workspace_path", "outputPath"} {
				if p, ok := x[k].(string); ok {
					add(p, stringValue(x, "doc_type"), stringValue(x, "title"))
				}
			}
			for _, child := range x {
				walk(child, depth+1)
			}
		case []any:
			for _, child := range x {
				walk(child, depth+1)
			}
		case string:
			for _, m := range nativeLinks.FindAllStringSubmatch(x, 20) {
				add(m[1], "", "")
			}
			for _, tag := range nativeTags.FindAllString(x, 20) {
				attrs := map[string]string{}
				for _, m := range tagAttributes.FindAllStringSubmatch(tag, -1) {
					attrs[m[1]] = html.UnescapeString(m[2])
				}
				add(attrs["file_path"], attrs["doc_type"], attrs["title"])
				if data := attrs["data"]; data != "" {
					var parsed any
					if json.Unmarshal([]byte(data), &parsed) == nil {
						walk(parsed, depth+1)
					}
				}
			}
		}
	}
	walk(root, 0)
	return out
}

func unwrapNative(v any) (any, error) {
	for range 10 {
		switch x := v.(type) {
		case string:
			var next any
			if json.Unmarshal([]byte(x), &next) != nil {
				return x, nil
			}
			v = next
		case map[string]any:
			if nested, ok := x["eventResponse"].(map[string]any); ok {
				if y, ok := nested["invokeAgentResponse"]; ok {
					v = y
					continue
				}
			}
			if status, ok := x["status"].(float64); ok && status >= 400 {
				return nil, fmt.Errorf("Quick 文件接口返回状态 %d", int(status))
			}
			if y, ok := x["body"]; ok {
				if x["body_encoding"] == "base64" {
					s, _ := y.(string)
					b, err := base64.StdEncoding.DecodeString(s)
					if err != nil {
						return nil, err
					}
					v = string(b)
				} else {
					v = y
				}
				continue
			}
			return x, nil
		default:
			return v, nil
		}
	}
	return nil, errors.New("Quick 文件接口响应嵌套过深")
}

var nativeHTTP = &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func nativeDownloadURL(ctx context.Context, sess *Session, thread, nativePath string) (string, error) {
	if _, err := cleanNativePath(nativePath); err != nil {
		return "", err
	}
	u, err := url.Parse("https://" + strings.TrimPrefix(strings.TrimPrefix(sess.WSHost, "wss://"), "https://"))
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" {
		return "", errors.New("Quick 文件服务地址无效")
	}
	u.Path = "/integration/quick-work/qbs-event"
	payload := map[string]any{"eventType": "invokeAgent", "eventBody": map[string]any{"invokeAgent": map[string]any{"path": "files/download/" + url.PathEscape(thread), "queryParams": map[string]string{"_method": "POST"}, "body": map[string]string{"filePath": nativePath}}}}
	body, _ := json.Marshal(payload)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	r.Header.Set("Authorization", "Bearer "+sess.jwt())
	r.Header.Set("Content-Type", "application/json")
	res, err := nativeHTTP.Do(r)
	if err != nil {
		diagnostics.Event(ctx, "error", "media.download_url.failed", "error", err)
		return "", errors.New("Quick 文件下载地址请求失败")
	}
	defer res.Body.Close()
	diagnostics.Event(ctx, "debug", "media.download_url.response", "http_status", res.StatusCode, "upstream_request_id", res.Header.Get("X-Amzn-Requestid"))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Quick 文件下载接口返回 HTTP %d", res.StatusCode)
	}
	var outer any
	if err := json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&outer); err != nil {
		return "", errors.New("Quick 文件接口响应格式无效")
	}
	inner, err := unwrapNative(outer)
	if err != nil {
		return "", err
	}
	obj, _ := inner.(map[string]any)
	link, _ := obj["downloadUrl"].(string)
	parsed, err := url.Parse(link)
	if err != nil || link == "" || validateMediaURL(parsed) != nil || parsed.Scheme != "https" {
		return "", errors.New("Quick 未返回有效的 HTTPS 文件下载地址")
	}
	return link, nil
}

func ownerKey(key string) string                 { sum := sha256.Sum256([]byte(key)); return fmt.Sprintf("%x", sum[:]) }
func (s *Server) mediaDir() string               { return filepath.Join(filepath.Dir(s.cfg.Database), "media") }
func (s *Server) mediaDiskPath(id string) string { return filepath.Join(s.mediaDir(), id+".asset") }

func (s *Server) mediaURL(f *store.MediaFile) string {
	expires := time.Now().Add(24 * time.Hour).Unix()
	message := f.ID + ":" + strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, s.mediaSecret)
	_, _ = mac.Write([]byte(message))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return strings.TrimRight(s.cfg.Admin.PublicURL, "/") + "/v1/media/" + url.PathEscape(f.ID) + "?expires=" + strconv.FormatInt(expires, 10) + "&signature=" + sig
}

func (s *Server) downloadArtifact(ctx context.Context, sess *Session, thread, nativePath string) (*http.Response, error) {
	delay := s.artifactRetryDelay
	if delay <= 0 {
		delay = time.Second
	}
	for attempt := 1; attempt <= 4; attempt++ {
		// Reissue the presigned URL on every attempt. Both an expired signature
		// and a workspace artifact still being published can initially return 403.
		link, err := nativeDownloadURL(ctx, sess, thread, nativePath)
		if err != nil {
			return nil, err
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if err != nil {
			return nil, err
		}
		res, err := mediaHTTPClient.Do(r)
		if err != nil {
			diagnostics.Event(ctx, "error", "media.download.failed", "download_attempt", attempt, "error", err)
			return nil, errors.New("Quick 生成文件下载失败")
		}
		if res.StatusCode == http.StatusOK {
			return res, nil
		}
		var upstream struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		res.Body.Close()
		_ = xml.Unmarshal(raw, &upstream)
		err = fmt.Errorf("生成文件下载返回 HTTP %d", res.StatusCode)
		if upstream.Code != "" {
			err = fmt.Errorf("%w (%s)", err, diagnostics.RedactText(upstream.Code, 128))
		}
		retry := attempt < 4 && (res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusNotFound)
		event := "media.download.rejected"
		if retry {
			event = "media.download.retry"
		}
		diagnostics.Event(ctx, "warn", event, "download_attempt", attempt, "http_status", res.StatusCode, "upstream_code", upstream.Code, "error_message", upstream.Message, "upstream_request_id", res.Header.Get("X-Amz-Request-Id"), "error", err)
		if !retry {
			return nil, err
		}
		timer := time.NewTimer(delay << (attempt - 1))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("生成文件下载重试次数已耗尽")
}

func (s *Server) cacheArtifacts(ctx context.Context, sess *Session, owner, thread string, refs []NativeArtifact) (cached []store.MediaFile, resultErr error) {
	ctx = diagnostics.With(ctx, "account_id", sess.AccountID, "upstream_thread_id", thread)
	started := time.Now()
	stage := "initialize"
	defer func() {
		level := "info"
		if resultErr != nil {
			level = "error"
		}
		diagnostics.Event(ctx, level, "media.cache.finished", "stage", stage, "artifact_count", len(refs), "cached_count", len(cached), "duration_ms", time.Since(started).Milliseconds(), "error", resultErr)
	}()
	if s.store == nil || len(refs) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(s.mediaDir(), 0700); err != nil {
		return nil, err
	}
	files := make([]store.MediaFile, 0)
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref.Path] {
			continue
		}
		seen[ref.Path] = true
		if _, err := cleanNativePath(ref.Path); err != nil {
			continue
		}
		if existing, err := s.store.FindMedia(ctx, owner, sess.AccountID, thread, ref.Path); err == nil {
			diagnostics.Event(ctx, "debug", "media.cache.reused", "file_id", existing.ID)
			existing.URL = s.mediaURL(existing)
			files = append(files, *existing)
			continue
		}
		stage = "download_url"
		diagnostics.Event(ctx, "info", "media.cache.started", "native_path_sha256", diagnostics.Fingerprint(ref.Path), "artifact_kind", ref.Kind)
		stage = "download"
		downloadStarted := time.Now()
		res, err := s.downloadArtifact(ctx, sess, thread, ref.Path)
		if err != nil {
			return nil, err
		}
		diagnostics.Event(ctx, "debug", "media.download.response", "http_status", res.StatusCode, "content_length", res.ContentLength, "content_type", res.Header.Get("Content-Type"))
		if res.ContentLength > 256<<20 {
			res.Body.Close()
			return nil, errors.New("生成文件超过 256 MiB 下载限制")
		}
		id := "file-" + strings.ReplaceAll(uuid(), "-", "")
		file, err := os.OpenFile(s.mediaDiskPath(id)+".part", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			res.Body.Close()
			return nil, err
		}
		stage = "save_file"
		size, copyErr := io.Copy(file, io.LimitReader(res.Body, (256<<20)+1))
		closeErr := file.Close()
		res.Body.Close()
		if copyErr != nil || closeErr != nil || size == 0 || size > 256<<20 {
			diagnostics.Event(ctx, "error", "media.file.save_failed", "file_id", id, "size_bytes", size, "read_error", copyErr, "close_error", closeErr)
			_ = os.Remove(s.mediaDiskPath(id) + ".part")
			return nil, errors.New("生成文件读取失败或超过大小限制")
		}
		diagnostics.Event(ctx, "info", "media.download.completed", "file_id", id, "size_bytes", size, "duration_ms", time.Since(downloadStarted).Milliseconds())
		if err := os.Rename(s.mediaDiskPath(id)+".part", s.mediaDiskPath(id)); err != nil {
			return nil, err
		}
		ct := mime.TypeByExtension(path.Ext(ref.Path))
		if ct == "" {
			ct = res.Header.Get("Content-Type")
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		if ct == "video/mp4" {
			stage = "video_encoding"
			size, err = browserMP4(diagnostics.With(ctx, "file_id", id), s.mediaDiskPath(id))
			if err != nil {
				_ = os.Remove(s.mediaDiskPath(id))
				return nil, err
			}
		}
		f := store.MediaFile{ID: id, Owner: owner, AccountID: sess.AccountID, ThreadID: thread, NativePath: ref.Path, Filename: path.Base(ref.Path), MIMEType: ct, Size: size}
		stage = "save_metadata"
		if err := s.store.SaveMedia(ctx, &f); err != nil {
			_ = os.Remove(s.mediaDiskPath(id))
			return nil, err
		}
		f.URL = s.mediaURL(&f)
		files = append(files, f)
		diagnostics.Event(ctx, "info", "media.cache.completed", "file_id", id, "size_bytes", size, "mime_type", ct)
	}
	stage = "complete"
	return files, nil
}

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, 405, "method_not_allowed", "GET required")
		return
	}
	if s.store == nil {
		http.NotFound(w, r)
		return
	}
	f, err := s.store.GetMedia(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	allowed := s.tokens.Allowed(r) && ownerKey(tokenFromRequest(r)) == f.Owner
	if !allowed {
		expires, err := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
		mac := hmac.New(sha256.New, s.mediaSecret)
		_, _ = mac.Write([]byte(f.ID + ":" + strconv.FormatInt(expires, 10)))
		want := mac.Sum(nil)
		got, decodeErr := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("signature"))
		allowed = err == nil && decodeErr == nil && expires >= time.Now().Unix() && expires <= time.Now().Add(25*time.Hour).Unix() && hmac.Equal(got, want)
	}
	if !allowed {
		writeOpenAIError(w, 401, "invalid_api_key", "valid API key or unexpired download signature required")
		return
	}
	w.Header().Set("Content-Type", f.MIMEType)
	disposition := "attachment"
	switch strings.Split(f.MIMEType, ";")[0] {
	case "video/mp4", "video/webm", "image/png", "image/jpeg", "image/webp", "image/gif", "audio/mpeg", "audio/wav", "audio/ogg", "audio/flac", "application/pdf", "text/plain":
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": f.Filename}))
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeFile(w, r, s.mediaDiskPath(f.ID))
}

func replaceNativeLinks(text string, files []store.MediaFile) string {
	links := map[string]string{}
	for _, f := range files {
		links[f.NativePath] = f.URL
	}
	return nativeLinks.ReplaceAllStringFunc(text, func(raw string) string {
		p, err := cleanNativePath(raw)
		if err == nil && links[p] != "" {
			return links[p]
		}
		return "#file-unavailable"
	})
}

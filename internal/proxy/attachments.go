package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"quickwork-openai/internal/diagnostics"
)

const maxMediaBytes = 50 << 20
const maxImageBytes = 10 << 20
const maxChatBody = 80 << 20

// Attachment matches Quick's native user_message attachment, including inline
// bytes. The model receives the actual media rather than a text placeholder.
type Attachment struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	MIMEType      string `json:"mimeType"`
	Data          string `json:"data,omitempty"`
	Path          string `json:"path,omitempty"`
	AttachmentID  string `json:"attachmentId,omitempty"`
	ContentSHA256 string `json:"contentSha256"`
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, prefix := range []string{"100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "2001:db8::/32"} {
		if netip.MustParsePrefix(prefix).Contains(ip) {
			return false
		}
	}
	return true
}

// Resolve and pin the actual dial destination, including after redirects. A
// hostname validation alone would leave downloads open to DNS rebinding.
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, errors.New("无法解析媒体地址")
	}
	if len(addresses) == 0 {
		return nil, errors.New("媒体地址没有可用的 IP")
	}
	for _, ip := range addresses {
		if !publicIP(ip) {
			return nil, errors.New("媒体 URL 不能访问内网、回环或保留地址")
		}
	}
	var last error
	for _, ip := range addresses {
		conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

var mediaHTTPClient = &http.Client{
	Timeout:   90 * time.Second,
	Transport: &http.Transport{Proxy: nil, DialContext: publicDial, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second, MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second},
	CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("媒体 URL 重定向过多")
		}
		return validateMediaURL(r.URL)
	},
}

func validateMediaURL(u *url.URL) error {
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("媒体地址必须是公开的 HTTP(S) URL 或 Base64 data URL")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicIP(ip) {
		return errors.New("媒体 URL 不能访问内网、回环或保留地址")
	}
	if u.Port() != "" && u.Port() != "80" && u.Port() != "443" {
		return errors.New("媒体 URL 仅允许 80 和 443 端口")
	}
	return nil
}

func readMedia(ctx context.Context, source string, limit int64) ([]byte, string, error) {
	if strings.HasPrefix(source, "data:") {
		header, body, ok := strings.Cut(source, ",")
		if !ok || len(header) > 256 || !strings.HasSuffix(header, ";base64") {
			return nil, "", errors.New("媒体 data URL 必须使用 Base64 编码")
		}
		if int64(base64.StdEncoding.DecodedLen(len(body))) > limit+2 {
			return nil, "", errors.New("媒体内容超过大小限制")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(body)
		if err != nil || len(data) == 0 {
			return nil, "", errors.New("媒体 Base64 内容无效或为空")
		}
		if int64(len(data)) > limit {
			return nil, "", errors.New("媒体内容超过大小限制")
		}
		return data, strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64"), nil
	}
	u, err := url.Parse(source)
	if err != nil {
		return nil, "", errors.New("无效的媒体 URL")
	}
	if err = validateMediaURL(u); err != nil {
		return nil, "", err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	res, err := mediaHTTPClient.Do(r)
	if err != nil {
		diagnostics.Event(ctx, "warn", "media.input.download_failed", "host", u.Hostname(), "error", err)
		return nil, "", errors.New("媒体下载失败：请确认地址可从服务器公开访问")
	}
	defer res.Body.Close()
	diagnostics.Event(ctx, "debug", "media.input.download_response", "host", u.Hostname(), "http_status", res.StatusCode, "content_length", res.ContentLength, "content_type", res.Header.Get("Content-Type"))
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("媒体下载返回 HTTP %d", res.StatusCode)
	}
	if res.ContentLength > limit {
		return nil, "", errors.New("媒体内容超过大小限制")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, "", errors.New("媒体内容读取失败")
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return nil, "", errors.New("媒体内容为空或超过大小限制")
	}
	ct, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	return data, ct, nil
}

func stringValue(m map[string]any, key string) string { s, _ := m[key].(string); return s }

func parseAttachment(ctx context.Context, part map[string]any, index int) (Attachment, error) {
	var a Attachment
	var source, filename, declared string
	typ := stringValue(part, "type")
	switch typ {
	case "image_url", "input_image":
		a.Type = "image"
		source = imagePartURL(part)
	case "input_audio":
		a.Type = "audio"
		v, _ := part["input_audio"].(map[string]any)
		format := stringValue(v, "format")
		switch format {
		case "mp3":
			declared = "audio/mpeg"
		case "wav":
			declared = "audio/wav"
		case "flac":
			declared = "audio/flac"
		case "ogg":
			declared = "audio/ogg"
		default:
			return a, errors.New("input_audio.format 必须是 mp3、wav、flac 或 ogg")
		}
		source = "data:" + declared + ";base64," + stringValue(v, "data")
	case "file", "input_file":
		a.Type = "file"
		v := part
		if nested, ok := part["file"].(map[string]any); ok {
			v = nested
		}
		if stringValue(v, "file_id") != "" {
			return a, errors.New("请使用 file_data 或 file_url 传入文件内容；此服务不接受其他平台的 file_id")
		}
		filename = stringValue(v, "filename")
		source = stringValue(v, "file_data")
		if source == "" {
			source = stringValue(v, "file_url")
		}
		declared = mime.TypeByExtension(path.Ext(filename))
		if source != "" && !strings.HasPrefix(source, "data:") && !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") {
			if declared == "" {
				declared = "application/octet-stream"
			}
			source = "data:" + declared + ";base64," + source
		}
	default:
		return a, errors.New("不支持此媒体输入类型")
	}
	if source == "" {
		return a, errors.New("媒体内容或 URL 不能为空")
	}
	limit := int64(maxMediaBytes)
	if a.Type == "image" {
		limit = maxImageBytes
	}
	data, contentType, err := readMedia(ctx, source, limit)
	if err != nil {
		return a, err
	}
	if a.Type == "image" {
		contentType = http.DetectContentType(data)
		switch contentType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			return a, errors.New("图片内容必须是 PNG、JPEG、GIF 或 WebP")
		}
	}
	if contentType == "" {
		contentType = declared
	}
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if filename == "" {
		ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif", "audio/mpeg": ".mp3", "audio/wav": ".wav", "audio/flac": ".flac", "audio/ogg": ".ogg", "application/pdf": ".pdf", "text/plain": ".txt"}[contentType]
		if ext == "" {
			ext = ".bin"
		}
		filename = fmt.Sprintf("attachment_%02d%s", index, ext)
	}
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	if len(filename) > 180 || filename == "." || filename == "/" || strings.ContainsAny(filename, "\x00\r\n") {
		return a, errors.New("附件文件名无效")
	}
	sum := sha256.Sum256(data)
	a.Name = fmt.Sprintf("%02d_%s", index, filename)
	a.Size = int64(len(data))
	a.MIMEType = contentType
	a.Data = base64.StdEncoding.EncodeToString(data)
	a.ContentSHA256 = hex.EncodeToString(sum[:])
	return a, nil
}

func prepareMedia(ctx context.Context, messages []Message) ([]Message, []Attachment, error) {
	cloned := append([]Message(nil), messages...)
	attachments := make([]Attachment, 0)
	var total int64
	for i, m := range messages {
		parts, ok := m.Content.([]any)
		if !ok {
			continue
		}
		updated := make([]any, 0, len(parts))
		for _, part := range parts {
			p, ok := part.(map[string]any)
			if !ok {
				updated = append(updated, part)
				continue
			}
			typ := stringValue(p, "type")
			switch typ {
			case "image_url", "input_image", "input_audio", "file", "input_file":
				if m.Role != "user" {
					return nil, nil, errors.New("媒体附件只能放在 user 消息中")
				}
				if len(attachments) >= 20 {
					return nil, nil, errors.New("每次请求最多可包含 20 个附件")
				}
				a, err := parseAttachment(ctx, p, len(attachments)+1)
				if err != nil {
					return nil, nil, fmt.Errorf("messages[%d] 附件：%w", i, err)
				}
				total += a.Size
				if total > maxMediaBytes {
					return nil, nil, errors.New("所有附件的总大小不能超过 50 MiB")
				}
				attachments = append(attachments, a)
				diagnostics.Event(ctx, "info", "media.input.prepared", "message_index", i, "attachment_index", len(attachments), "type", a.Type, "size_bytes", a.Size, "mime_type", a.MIMEType, "sha256", a.ContentSHA256)
				updated = append(updated, map[string]any{"type": "text", "text": "\n[附件 " + a.Name + "]\n"})
			case "video_url", "input_video", "video":
				return nil, nil, errors.New("暂未接通视频内容输入；视频生成请使用 /v1/videos")
			default:
				updated = append(updated, part)
			}
		}
		cloned[i].Content = updated
	}
	return cloned, attachments, nil
}

package requestlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"

	"quickwork-openai/internal/diagnostics"
)

const bodyLimit = 512 << 10

type capture struct {
	prefix bytes.Buffer
	total  int64
	digest hash.Hash
}

func (c *capture) Write(b []byte) (int, error) {
	if c.digest == nil {
		c.digest = sha256.New()
	}
	_, _ = c.digest.Write(b)
	c.total += int64(len(b))
	if c.prefix.Len() < bodyLimit {
		_, _ = c.prefix.Write(b[:min(len(b), bodyLimit-c.prefix.Len())])
	}
	return len(b), nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

func privateKey(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	switch key {
	case "authorization", "cookie", "setcookie", "apikey", "accesstoken", "refreshtoken", "idtoken", "password", "passwordhash", "clientsecret", "codeverifier", "secret", "token", "signature", "jwt":
		return true
	}
	return strings.HasPrefix(key, "xamz") || strings.Contains(key, "apikey") || strings.HasSuffix(key, "token") || strings.HasSuffix(key, "password") || strings.HasSuffix(key, "secret") || strings.HasSuffix(key, "secretkey") || strings.HasSuffix(key, "secretaccesskey")
}

func redact(value any, depth int) any {
	if depth > 32 {
		return "[depth limit]"
	}
	switch x := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		isBase64 := x["type"] == "base64"
		for key, v := range x {
			lower := strings.ToLower(key)
			if privateKey(key) {
				out[key] = "[REDACTED]"
				continue
			}
			if lower == "file_data" || lower == "image_data" || (lower == "data" && (isBase64 || x["format"] != nil)) {
				if text, ok := v.(string); ok {
					out[key] = map[string]any{"omitted": "binary data", "encoded_bytes": len(text), "sha256": diagnostics.Fingerprint(text)}
					continue
				}
			}
			if lower == "user_id" {
				if s, ok := v.(string); ok {
					out[key] = "user_" + diagnostics.Fingerprint(s)[:12]
					continue
				}
			}
			out[key] = redact(v, depth+1)
		}
		return out
	case []any:
		out := make([]any, 0, min(len(x), 2000))
		for i, v := range x {
			if i >= 2000 {
				out = append(out, "[array truncated]")
				break
			}
			out = append(out, redact(v, depth+1))
		}
		return out
	case string:
		trimmed := strings.TrimSpace(x)
		if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
			var parsed any
			_ = json.Unmarshal([]byte(trimmed), &parsed)
			b, _ := json.Marshal(redact(parsed, depth+1))
			return string(b)
		}
		return diagnostics.RedactText(x, 128<<10)
	default:
		return value
	}
}

func (c *capture) snapshot(contentType string) map[string]any {
	out := map[string]any{"bytes": c.total, "captured_bytes": c.prefix.Len(), "truncated": c.total > int64(c.prefix.Len())}
	if c.digest != nil {
		out["sha256"] = hex.EncodeToString(c.digest.Sum(nil))
	}
	if c.total == 0 {
		out["body"] = nil
		return out
	}
	if c.total > int64(c.prefix.Len()) {
		out["body_note"] = "正文超过 512 KiB，仅记录大小和摘要"
		return out
	}
	b := c.prefix.Bytes()
	if strings.Contains(contentType, "multipart/") {
		out["body_note"] = "multipart 文件正文已省略"
		return out
	}
	var obj any
	if json.Unmarshal(b, &obj) == nil {
		out["body"] = redact(obj, 0)
		return out
	}
	if strings.Contains(contentType, "text/event-stream") {
		events := make([]any, 0)
		kind := "message"
		for _, line := range bytes.Split(b, []byte{'\n'}) {
			if bytes.HasPrefix(line, []byte("event:")) {
				kind = strings.TrimSpace(string(line[6:]))
			}
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(line[5:])
			var parsed any
			if json.Unmarshal(data, &parsed) != nil {
				parsed = string(data)
			}
			events = append(events, map[string]any{"event": kind, "data": redact(parsed, 0)})
			if len(events) >= 2000 {
				out["events_truncated"] = true
				break
			}
		}
		out["events"] = events
		return out
	}
	if bytes.Contains(b, []byte("base64")) || bytes.Contains(b, []byte("file_data")) {
		out["body_note"] = "无法解析的媒体正文已省略"
		return out
	}
	out["body_text"] = diagnostics.RedactText(string(b), 128<<10)
	return out
}

func safeHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, key := range []string{"Content-Type", "User-Agent", "Anthropic-Version", "Anthropic-Beta", "X-Thread-Id", "X-Request-Id", "Request-Id", "X-Quickwork-Compatibility", "X-Token-Count-Method"} {
		if v := h.Get(key); v != "" {
			out[key] = diagnostics.RedactText(v, 1024)
		}
	}
	return out
}

func textValue(v any) string {
	if e, ok := v.(error); ok {
		return e.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func number(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}
func cleanText(v any) string { return diagnostics.RedactText(textValue(v), 2048) }
func safeID(id string) bool {
	if len(id) < 1 || len(id) > 100 {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func keyID(r *http.Request) string {
	key := r.Header.Get("X-Api-Key")
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		key = parts[1]
	}
	if key == "" {
		return ""
	}
	return "key_" + diagnostics.Fingerprint(key)[:12]
}
func detailName(id string) string { return fmt.Sprintf("%s.json", id) }

func responseError(body []byte, contentType string) (string, bool) {
	extract := func(raw []byte) (string, bool) {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			return "", false
		}
		value, ok := obj["error"]
		if !ok {
			return "", false
		}
		if e, ok := value.(map[string]any); ok {
			return textValue(e["message"]), true
		}
		return textValue(value), true
	}
	if message, failed := extract(body); failed {
		return message, true
	}
	if strings.Contains(contentType, "text/event-stream") {
		for _, line := range bytes.Split(body, []byte{'\n'}) {
			if bytes.HasPrefix(line, []byte("data:")) {
				if message, failed := extract(bytes.TrimSpace(line[5:])); failed {
					return message, true
				}
			}
		}
	}
	return "", false
}

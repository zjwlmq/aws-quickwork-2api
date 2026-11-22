// Package diagnostics records bounded, redacted JSON events and request traces.
package diagnostics

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	Level   string
	File    string
	MaxMB   int
	Backups int
	Stdout  io.Writer
}

type Logger struct {
	mu             sync.Mutex
	file           *os.File
	path           string
	size           int64
	maxBytes       int64
	backups        int
	level          int
	levelName      string
	stdout         io.Writer
	boot           string
	seq            uint64
	lastWriteError string
	secretMu       sync.RWMutex
	secrets        map[string]struct{}
}

var current atomic.Pointer[Logger]

type fieldsKey struct{}
type requestKey struct{}
type requestState struct {
	errors   atomic.Int64
	warnings atomic.Int64
}

func SetDefault(l *Logger) *Logger { return current.Swap(l) }
func Default() *Logger             { return current.Load() }
func Enabled(level string) bool    { l := Default(); return l != nil && levelNumber(level) >= l.level }

func levelNumber(level string) int {
	switch strings.ToLower(level) {
	case "debug":
		return 0
	case "warn":
		return 2
	case "error":
		return 3
	case "off":
		return 100
	default:
		return 1
	}
}

func Open(o Options) (*Logger, error) {
	if o.MaxMB < 1 {
		o.MaxMB = 20
	}
	if o.Backups < 0 {
		o.Backups = 0
	}
	if o.Level == "" {
		o.Level = "info"
	}
	l := &Logger{path: o.File, maxBytes: int64(o.MaxMB) << 20, backups: o.Backups, level: levelNumber(o.Level), levelName: strings.ToLower(o.Level), stdout: o.Stdout, boot: ID(), secrets: make(map[string]struct{})}
	if l.path != "" && l.level < 100 {
		if err := os.MkdirAll(filepath.Dir(l.path), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		if err := f.Chmod(0600); err != nil {
			f.Close()
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		l.file, l.size = f, info.Size()
	}
	return l, nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func ID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func Fingerprint(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func With(ctx context.Context, fields ...any) context.Context {
	previous, _ := ctx.Value(fieldsKey{}).([]any)
	all := append(append([]any(nil), previous...), fields...)
	return context.WithValue(ctx, fieldsKey{}, all)
}

func RequestID(ctx context.Context) string {
	fields, _ := ctx.Value(fieldsKey{}).([]any)
	for i := len(fields) - 2; i >= 0; i -= 2 {
		if fields[i] == "request_id" {
			value, _ := fields[i+1].(string)
			return value
		}
	}
	return ""
}

// RegisterSecrets protects even upstream errors which echo an opaque credential.
func RegisterSecrets(values ...string) {
	l := Default()
	if l == nil {
		return
	}
	l.secretMu.Lock()
	defer l.secretMu.Unlock()
	for _, value := range values {
		if len(value) >= 8 && len(l.secrets) < 4096 {
			l.secrets[value] = struct{}{}
		}
	}
}

var bearerRE = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[^\s"'<>\\,;]+`)
var keyRE = regexp.MustCompile(`(?i)(["']?(?:access_token|refresh_token|id_token|api_key|apikey|authorization|password|client_secret|code_verifier|signature|x-amz-[a-z-]+|token|secret|code)["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s&,;<>}]+)`)
var jwtRE = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
var apiKeyRE = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
var urlQueryRE = regexp.MustCompile(`((?:https?|wss?)://[^\s"'<>?]+)\?[^\s"'<>]*`)
var dataRE = regexp.MustCompile(`(?i)data:[^\s,;]+(?:;base64)?,[^\s"'<>]+`)

func sensitive(key string) bool {
	k := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(key))
	switch k {
	case "authorization", "cookie", "setcookie", "apikey", "accesskey", "accesstoken", "refreshtoken", "idtoken", "password", "passwordhash", "clientsecret", "codeverifier", "signature", "secret", "token", "jwt", "code", "body", "payload", "prompt", "text", "content", "arguments", "result", "data":
		return true
	}
	return strings.HasPrefix(k, "xamz")
}

func (l *Logger) cleanString(s string) string {
	if len(s) > 64<<10 {
		s = strings.ToValidUTF8(s[:64<<10], "") + "…[truncated]"
	}
	l.secretMu.RLock()
	for secret := range l.secrets {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	l.secretMu.RUnlock()
	s = dataRE.ReplaceAllString(s, "[MEDIA OMITTED]")
	s = bearerRE.ReplaceAllString(s, "Bearer [REDACTED]")
	s = urlQueryRE.ReplaceAllString(s, "$1?[REDACTED]")
	s = keyRE.ReplaceAllString(s, "${1}[REDACTED]")
	s = jwtRE.ReplaceAllString(s, "[REDACTED]")
	s = apiKeyRE.ReplaceAllString(s, "[REDACTED]")
	if len(s) > 2048 {
		s = strings.ToValidUTF8(s[:2048], "") + "…[truncated]"
	}
	return s
}

func (l *Logger) clean(key string, v any, depth int) any {
	if sensitive(key) {
		return "[REDACTED]"
	}
	if depth > 7 {
		return "[depth limit]"
	}
	switch x := v.(type) {
	case nil, bool, int, int64, uint64, float64:
		return v
	case string:
		return l.cleanString(x)
	case error:
		return l.cleanString(x.Error())
	case []string:
		out := make([]any, 0, min(len(x), 100))
		for i, s := range x {
			if i >= 100 {
				break
			}
			out = append(out, l.cleanString(s))
		}
		return out
	case []any:
		out := make([]any, 0, min(len(x), 100))
		for i, s := range x {
			if i >= 100 {
				break
			}
			out = append(out, l.clean("", s, depth+1))
		}
		return out
	case map[string]any:
		out := make(map[string]any)
		n := 0
		for k, s := range x {
			if n >= 100 {
				break
			}
			out[l.cleanString(k)] = l.clean(k, s, depth+1)
			n++
		}
		return out
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "[unsupported value]"
		}
		var decoded any
		if json.Unmarshal(b, &decoded) != nil {
			return "[invalid value]"
		}
		return l.clean(key, decoded, depth+1)
	}
}

func Event(ctx context.Context, level, event string, fields ...any) {
	l := Default()
	if l == nil || levelNumber(level) < l.level {
		return
	}
	if state, ok := ctx.Value(requestKey{}).(*requestState); ok {
		if levelNumber(level) >= 3 {
			state.errors.Add(1)
		} else if levelNumber(level) == 2 {
			state.warnings.Add(1)
		}
	}
	record := make(map[string]any)
	add := func(f []any) {
		for i := 0; i+1 < len(f); i += 2 {
			if k, ok := f[i].(string); ok {
				record[k] = l.clean(k, f[i+1], 0)
			}
		}
	}
	ctxFields, _ := ctx.Value(fieldsKey{}).([]any)
	add(ctxFields)
	add(fields)
	record["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["level"] = strings.ToUpper(level)
	record["event"] = event
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	record["entry_id"] = fmt.Sprintf("%s-%d", l.boot, l.seq)
	b, err := json.Marshal(record)
	if err != nil {
		return
	}
	if len(b) > 64<<10 {
		compact := map[string]any{"entry_truncated": true, "original_bytes": len(b)}
		for _, key := range []string{"time", "level", "event", "entry_id", "request_id", "completion_id", "account_id", "attempt", "job_id", "upstream_thread_id", "upstream_trace_id", "error", "error_message"} {
			if value, ok := record[key]; ok {
				compact[key] = value
			}
		}
		b, _ = json.Marshal(compact)
	}
	b = append(b, '\n')
	if l.file != nil {
		if l.size+int64(len(b)) > l.maxBytes {
			if err = l.rotate(); err != nil {
				l.lastWriteError = err.Error()
			}
		}
		if l.file != nil {
			n, writeErr := l.file.Write(b)
			l.size += int64(n)
			if writeErr != nil {
				l.lastWriteError = writeErr.Error()
			}
		}
	}
	if l.stdout != nil {
		_, _ = l.stdout.Write(b)
	}
}

func (l *Logger) rotate() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	l.file = nil
	// Always reopen the current file, including after a failed rename.
	defer func() {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			l.file = f
			info, e := f.Stat()
			if e == nil {
				l.size = info.Size()
			}
		} else {
			l.lastWriteError = err.Error()
		}
	}()
	if l.backups == 0 {
		return os.Truncate(l.path, 0)
	}
	if err := os.Remove(fmt.Sprintf("%s.%d", l.path, l.backups)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := l.backups - 1; i >= 1; i-- {
		if err := os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(l.path, l.path+".1")
}

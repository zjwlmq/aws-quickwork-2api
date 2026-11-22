package diagnostics

import (
	"context"
	"regexp"
	"strings"
)

type EventObserver interface {
	Observe(context.Context, string, string, map[string]any)
}
type observerKey struct{}

func WithObserver(ctx context.Context, observer EventObserver) context.Context {
	return context.WithValue(ctx, observerKey{}, observer)
}

func observeEvent(ctx context.Context, level, event string, fields []any) {
	observer, ok := ctx.Value(observerKey{}).(EventObserver)
	if !ok {
		return
	}
	values := make(map[string]any)
	add := func(items []any) {
		for i := 0; i+1 < len(items); i += 2 {
			if key, ok := items[i].(string); ok {
				values[key] = items[i+1]
			}
		}
	}
	parent, _ := ctx.Value(fieldsKey{}).([]any)
	add(parent)
	add(fields)
	observer.Observe(ctx, level, event, values)
}

var detailCredentialRE = regexp.MustCompile(`(?i)(["']?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|auth[_-]?token|api[_-]?key|secret[_-]?(?:access[_-]?)?key|access[_-]?key|authorization|cookie|set-cookie|password|client[_-]?secret|code_verifier|signature|x-amz-[a-z-]+|token|secret)["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s&,;<>}]+)`)
var urlCredentialsRE = regexp.MustCompile(`((?:https?|wss?)://)[^/\s"'<>@]+@`)

// RedactText preserves diagnostic text while masking credentials. Unlike event
// logging, the request-detail channel deliberately allows prompt/code text.
func RedactText(s string, limit int) string {
	if l := Default(); l != nil {
		l.secretMu.RLock()
		for secret := range l.secrets {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
		l.secretMu.RUnlock()
	}
	s = dataRE.ReplaceAllString(s, "[MEDIA OMITTED]")
	s = urlCredentialsRE.ReplaceAllString(s, "${1}[REDACTED]@")
	s = bearerRE.ReplaceAllString(s, "Bearer [REDACTED]")
	s = urlQueryRE.ReplaceAllString(s, "${1}?[REDACTED]")
	s = detailCredentialRE.ReplaceAllString(s, "${1}[REDACTED]")
	s = jwtRE.ReplaceAllString(s, "[REDACTED]")
	s = apiKeyRE.ReplaceAllString(s, "[REDACTED]")
	if len(s) > limit {
		s = strings.ToValidUTF8(s[:limit], "") + "…[truncated]"
	}
	return s
}

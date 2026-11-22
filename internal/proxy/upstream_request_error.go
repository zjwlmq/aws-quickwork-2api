package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"quickwork-openai/internal/accounts"
)

// These failures belong to this request, not to the selected login account.
// Retrying another account cannot change a native input-size or content refusal.
type upstreamRequestError struct {
	code    string
	message string
}

func (e *upstreamRequestError) Error() string { return e.message }

func nativeReplyError(kind, message string) error {
	lower := strings.ToLower(message)
	code := ""
	switch {
	case strings.Contains(lower, "content filter stopped this reply"):
		code = "content_filter"
	case strings.Contains(lower, "message is over the 64 kb size limit"):
		code = "context_length_exceeded"
	}
	if code != "" {
		return &upstreamRequestError{code: code, message: "Quick: " + message}
	}
	return fmt.Errorf("upstream %s: %s", kind, message)
}

func requestRefused(err error) bool {
	var refusal *upstreamRequestError
	return errors.As(err, &refusal)
}

func chatErrorStatus(err error) (int, string) {
	var refusal *upstreamRequestError
	if errors.As(err, &refusal) {
		return http.StatusBadRequest, refusal.code
	}
	if errors.Is(err, accounts.ErrNoAccounts) {
		return http.StatusUnauthorized, "upstream_session_unavailable"
	}
	return http.StatusBadGateway, "upstream_error"
}

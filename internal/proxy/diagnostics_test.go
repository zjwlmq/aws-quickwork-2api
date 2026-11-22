package proxy

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStreamGraceTimeoutIsNotAConnectionFailure(t *testing.T) {
	ctx := context.Background()
	expired := time.Now().Add(-time.Second)
	if !expectedStreamEnd(ctx, os.ErrDeadlineExceeded, true, expired) {
		t.Fatal("expected grace timeout reported as failure")
	}
	if expectedStreamEnd(ctx, os.ErrDeadlineExceeded, false, expired) || expectedStreamEnd(ctx, os.ErrDeadlineExceeded, true, time.Time{}) || expectedStreamEnd(ctx, os.ErrDeadlineExceeded, true, time.Now().Add(time.Second)) {
		t.Fatal("unexpected timeout hidden")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if expectedStreamEnd(cancelled, os.ErrDeadlineExceeded, true, expired) {
		t.Fatal("cancellation hidden")
	}
	if expectedStreamEnd(ctx, &websocket.CloseError{Code: websocket.CloseAbnormalClosure}, true, expired) {
		t.Fatal("abnormal close hidden")
	}
}

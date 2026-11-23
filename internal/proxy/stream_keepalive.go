package proxy

import (
	"context"
	"errors"
	"time"

	"quickwork-openai/internal/diagnostics"
)

// Keep all response callbacks on the caller's goroutine. The upstream worker
// sends events and its final result through one channel to preserve ordering.
func streamWithKeepAlive(ctx context.Context, interval time.Duration, chat func(context.Context, EventFunc) (ChatResult, error), emit EventFunc, keepAlive func() error) (ChatResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type update struct {
		event  StreamEvent
		result ChatResult
		err    error
		done   bool
	}
	updates := make(chan update)
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		final := update{done: true}
		defer func() {
			if recover() != nil {
				final.err = errors.New("upstream stream processing panicked")
				diagnostics.Event(ctx, "error", "chat.stream.worker_panicked", "error", final.err)
			}
			select {
			case updates <- final:
			case <-stopped:
			}
		}()
		result, err := chat(ctx, func(event StreamEvent) error {
			select {
			case updates <- update{event: event}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-stopped:
				return context.Canceled
			}
		})
		final.result, final.err = result, err
	}()
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var callbackErr error
	for {
		select {
		case next := <-updates:
			if next.done {
				if callbackErr != nil {
					return next.result, callbackErr
				}
				return next.result, next.err
			}
			if callbackErr == nil && emit != nil {
				callbackErr = emit(next.event)
				if callbackErr != nil {
					cancel()
				}
			}
		case <-ticker.C:
			if callbackErr == nil && ctx.Err() == nil && keepAlive != nil {
				callbackErr = keepAlive()
				if callbackErr != nil {
					cancel()
				}
			}
		}
	}
}

package imageproxy

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Observer receives only bounded pipeline labels. Implementations must be safe
// for concurrent use and must not derive labels from a source URL or error text.
type Observer interface {
	ObserveStage(stage, result string, duration time.Duration)
	ObserveCache(layer, result string)
}

// AdmissionObserver is an optional extension. Existing stage/cache observers
// remain compatible. The only supported rejection reason is "busy".
type AdmissionObserver interface {
	ObserveAdmissionRejected(reason string)
}

type observerKey struct{}

// WithObserver attaches an observer to this request and its derived contexts.
func WithObserver(ctx context.Context, observer Observer) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, observerKey{}, observer)
}

func observerFromContext(ctx context.Context) Observer {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(observerKey{}).(Observer)
	return observer
}

func noStage(error) {}

// StartStage starts one actual operation. Unknown stage names are ignored. The
// returned function records at most once and uses ctx.Err when err is nil.
// Without an observer this does not read the clock or allocate a timer closure.
func StartStage(ctx context.Context, stage string) func(error) {
	switch stage {
	case "fetch", "decode", "transform", "encode", "pack", "write":
	default:
		return noStage
	}
	observer := observerFromContext(ctx)
	if observer == nil {
		return noStage
	}
	start := time.Now()
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			if err == nil && ctx != nil {
				err = ctx.Err()
			}
			observer.ObserveStage(stage, Result(err), time.Since(start))
		})
	}
}

// Result classifies an error without exposing any of its text as a label.
func Result(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrResponseTooLarge), errors.Is(err, ErrImageTooLarge), errors.Is(err, ErrTooManyFrames):
		return "limit"
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	return "error"
}

func observeCache(ctx context.Context, layer, result string) {
	if layer != "source" && layer != "transformed" {
		return
	}
	switch result {
	case "hit", "miss", "revalidated", "bypass", "error":
	default:
		result = "error"
	}
	if observer := observerFromContext(ctx); observer != nil {
		observer.ObserveCache(layer, result)
	}
}

func observeAdmissionRejected(ctx context.Context, reason string) {
	if reason != "busy" {
		return
	}
	if observer, ok := observerFromContext(ctx).(AdmissionObserver); ok {
		observer.ObserveAdmissionRejected(reason)
	}
}

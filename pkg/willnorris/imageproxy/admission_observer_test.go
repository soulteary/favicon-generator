package imageproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type admissionRecorder struct {
	recordingObserver
	mu      sync.Mutex
	reasons []string
}

func (o *admissionRecorder) ObserveAdmissionRejected(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reasons = append(o.reasons, reason)
}

func TestAdmissionObserverOptionalExtensionAndSharedSlot(t *testing.T) {
	o := new(admissionRecorder)
	data := policyPNG(t)
	p := NewProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return cacheableObserverResponse(r, data), nil
	}), nil)
	p.MaxConcurrentRequests = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	u := sourceURL(t, "https://origin.test/logo")
	go func() {
		done <- p.WithSource(ctx, u, "", func(ctx context.Context, _ []byte, _ SourceLimits) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("source callback did not acquire its slot")
	}
	if w := serveObserved(p, o, "/32,png/https://origin.test/logo"); w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("legacy rejection changed its HTTP behavior")
	}
	if err := p.WithSource(WithObserver(ctx, o), u, "", func(context.Context, []byte, SourceLimits) error { t.Error("busy callback ran"); return nil }); !errors.Is(err, ErrSourceBusy) {
		t.Fatal("source rejection changed its error behavior", err)
	}
	// Original two-method observers remain usable on the same busy branch.
	if err := p.WithSource(WithObserver(ctx, new(recordingObserver)), u, "", func(context.Context, []byte, SourceLimits) error { return nil }); !errors.Is(err, ErrSourceBusy) {
		t.Fatal("legacy observer no longer satisfies Observer", err)
	}
	observeAdmissionRejected(WithObserver(ctx, o), "URL_SECRET")
	observeAdmissionRejected(nil, "busy")
	o.mu.Lock()
	if len(o.reasons) != 2 || o.reasons[0] != "busy" || o.reasons[1] != "busy" {
		t.Errorf("unbounded or duplicated rejection reasons: %v", o.reasons)
	}
	o.mu.Unlock()
	if o.stageCount("fetch", "") != 0 || o.stageCount("decode", "") != 0 {
		t.Fatal("rejected requests entered pipeline work")
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal("admitted operation failed", err)
	}
	if w := serveObserved(p, o, "/32,png/https://origin.test/logo"); w.Code != 200 {
		t.Fatal("slot did not become available after the admitted callback")
	}
	// Exercise the direct HTTP entry without the optional interface as well.
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/health-check", nil))
	if w.Code != 200 {
		t.Fatal("observer extension changed health behavior")
	}
}

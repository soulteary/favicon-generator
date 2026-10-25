package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type lifecycleListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *lifecycleListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

type lifecycleService struct {
	obs       *observability
	listeners []*lifecycleListener
	cancel    context.CancelFunc
	done      chan struct{}
	err       error // Published by closing done.
}

func lifecycleConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := parseConfig([]string{"-addr=127.0.0.1:0", "-adminAddr=127.0.0.1:0", "-shutdownTimeout=2s"}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func lifecycleObservability(t *testing.T) *observability {
	t.Helper()
	obs, err := newObservability(io.Discard, "info")
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

func startLifecycleService(t *testing.T, cfg Config, handler http.Handler) *lifecycleService {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &lifecycleService{obs: lifecycleObservability(t), cancel: cancel, done: make(chan struct{})}
	bound := make(chan *lifecycleListener, 2)
	go func() {
		var lc net.ListenConfig
		s.err = serveWithListener(ctx, cfg, s.obs, handler, func(ctx context.Context, network, address string) (net.Listener, error) {
			listener, err := lc.Listen(ctx, network, address)
			if err != nil {
				return nil, err
			}
			tracked := &lifecycleListener{Listener: listener, closed: make(chan struct{})}
			bound <- tracked
			return tracked, nil
		})
		close(s.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			t.Error("service did not stop")
		}
	})
	count := 1
	if cfg.AdminAddr != "" {
		count++
	}
	for range count {
		select {
		case listener := <-bound:
			s.listeners = append(s.listeners, listener)
		case <-s.done:
			t.Fatalf("service failed before binding: %v", s.err)
		case <-time.After(3 * time.Second):
			t.Fatal("service did not bind")
		}
	}
	return s
}

func lifecycleWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
	}
}

func lifecycleClient(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 4 * time.Second}
}

func TestGracefulShutdownDrainsWithoutCancelingRequest(t *testing.T) {
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	s := startLifecycleService(t, lifecycleConfig(t), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.Context()
		select {
		case <-release:
			_, _ = io.WriteString(w, "finished")
		case <-r.Context().Done():
		}
	}))
	result := make(chan error, 1)
	client := lifecycleClient(t)
	go func() {
		resp, err := client.Get("http://" + s.listeners[0].Addr().String() + "/image")
		if err == nil {
			defer resp.Body.Close()
			var body []byte
			body, err = io.ReadAll(resp.Body)
			if err == nil && (resp.StatusCode != 200 || string(body) != "finished") {
				err = errors.New("active response was not delivered intact")
			}
		}
		result <- err
	}()
	var requestCtx context.Context
	select {
	case requestCtx = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("image request did not start")
	}
	s.cancel()
	lifecycleWait(t, s.listeners[0].closed)
	if err := requestCtx.Err(); err != nil {
		t.Fatalf("graceful drain canceled active work: %v", err)
	}
	rr := httptest.NewRecorder()
	s.obs.PublicHandler(http.NotFoundHandler()).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining readiness = %d, want 503", rr.Code)
	}
	select {
	case <-s.done:
		t.Fatal("service stopped before active request finished")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	lifecycleWait(t, s.done)
	if s.err != nil {
		t.Fatal(s.err)
	}
	for _, listener := range s.listeners {
		lifecycleWait(t, listener.closed)
	}
}

func TestForcedShutdownCancelsAndJoinsActiveWork(t *testing.T) {
	cfg := lifecycleConfig(t)
	cfg.ShutdownTimeout = 25 * time.Millisecond
	started, canceled, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var cleanupOnce sync.Once
	defer cleanupOnce.Do(func() { close(cleanup) })
	s := startLifecycleService(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
		<-cleanup
	}))
	client := lifecycleClient(t)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, err := client.Get("http://" + s.listeners[0].Addr().String() + "/image")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	lifecycleWait(t, started)
	s.cancel()
	lifecycleWait(t, canceled)
	select {
	case <-s.done:
		t.Fatal("service exited before canceled handler completed cleanup")
	default:
	}
	cleanupOnce.Do(func() { close(cleanup) })
	lifecycleWait(t, s.done)
	lifecycleWait(t, requestDone)
	if !errors.Is(s.err, context.DeadlineExceeded) {
		t.Fatalf("forced shutdown error = %v, want deadline exceeded", s.err)
	}
}

func TestAdminBindFailureClosesPublicListener(t *testing.T) {
	obs := lifecycleObservability(t)
	var public *lifecycleListener
	bindErr := errors.New("admin port unavailable")
	calls := 0
	err := serveWithListener(context.Background(), lifecycleConfig(t), obs, http.NotFoundHandler(), func(ctx context.Context, network, address string) (net.Listener, error) {
		calls++
		if calls == 2 {
			return nil, bindErr
		}
		var lc net.ListenConfig
		listener, err := lc.Listen(ctx, network, address)
		if err != nil {
			return nil, err
		}
		public = &lifecycleListener{Listener: listener, closed: make(chan struct{})}
		return public, nil
	})
	if !errors.Is(err, bindErr) {
		t.Fatalf("startup error = %v", err)
	}
	if public == nil {
		t.Fatal("public listener was not bound")
	}
	lifecycleWait(t, public.closed)
	rr := httptest.NewRecorder()
	obs.PublicHandler(http.NotFoundHandler()).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed startup readiness = %d", rr.Code)
	}
}

type lifecycleFailedListener struct{ err error }

func (l lifecycleFailedListener) Accept() (net.Conn, error) { return nil, l.err }
func (l lifecycleFailedListener) Close() error              { return nil }
func (l lifecycleFailedListener) Addr() net.Addr            { return &net.TCPAddr{} }

func TestServeFailureStopsAllListeners(t *testing.T) {
	obs := lifecycleObservability(t)
	serveErr := errors.New("listener failed")
	var admin *lifecycleListener
	calls := 0
	err := serveWithListener(context.Background(), lifecycleConfig(t), obs, http.NotFoundHandler(), func(ctx context.Context, network, address string) (net.Listener, error) {
		calls++
		if calls == 1 {
			return lifecycleFailedListener{serveErr}, nil
		}
		var lc net.ListenConfig
		listener, err := lc.Listen(ctx, network, address)
		if err != nil {
			return nil, err
		}
		admin = &lifecycleListener{Listener: listener, closed: make(chan struct{})}
		return admin, nil
	})
	if !errors.Is(err, serveErr) {
		t.Fatalf("serve error = %v", err)
	}
	lifecycleWait(t, admin.closed)
}

func TestCanceledStartupDoesNotListen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := serveWithListener(ctx, lifecycleConfig(t), lifecycleObservability(t), http.NotFoundHandler(), func(context.Context, string, string) (net.Listener, error) {
		t.Fatal("canceled startup opened a listener")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestDrainingRejectsNewWork(t *testing.T) {
	active := new(activeRequests)
	active.drain()
	rr := httptest.NewRecorder()
	active.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("new work was admitted while draining") })).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/image", nil))
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "1" {
		t.Fatalf("draining response = %d, headers %v", rr.Code, rr.Header())
	}
}

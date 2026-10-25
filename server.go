package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

type listenFunc func(context.Context, string, string) (net.Listener, error)

type managedServer struct {
	name     string
	server   *http.Server
	listener net.Listener
}

func serve(ctx context.Context, cfg Config, obs *observability, proxy http.Handler) error {
	var lc net.ListenConfig
	return serveWithListener(ctx, cfg, obs, proxy, lc.Listen)
}

func serveWithListener(ctx context.Context, cfg Config, obs *observability, proxy http.Handler, listen listenFunc) error {
	obs.SetReady(false)
	if err := ctx.Err(); err != nil {
		return err
	}
	// Stop signals start draining, rather than immediately canceling image work.
	// On a forced shutdown we cancel this context and join admitted handlers.
	requestCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	active := new(activeRequests)
	makeServer := func(addr string, handler http.Handler) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           active.wrap(handler),
			ErrorLog:          obs.ServerErrorLogger,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      cfg.Timeout + 5*time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    16 << 10,
			BaseContext:       func(net.Listener) context.Context { return requestCtx },
		}
	}
	servers := []managedServer{{name: "public", server: makeServer(cfg.Addr, obs.PublicHandler(proxy))}}
	if cfg.AdminAddr != "" {
		servers = append(servers, managedServer{name: "admin", server: makeServer(cfg.AdminAddr, obs.AdminHandler())})
	}
	// Bind every listener before declaring readiness or launching Serve workers.
	// A management-port failure must not leave a half-started image service.
	for i := range servers {
		listener, err := listen(ctx, "tcp", servers[i].server.Addr)
		if err != nil {
			for j := 0; j < i; j++ {
				_ = servers[j].listener.Close()
			}
			return fmt.Errorf("listen on %s address: %w", servers[i].name, err)
		}
		servers[i].listener = listener
	}
	results := make(chan error, len(servers))
	for _, entry := range servers {
		go func(entry managedServer) {
			err := entry.server.Serve(entry.listener)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			if err != nil {
				err = fmt.Errorf("serve %s: %w", entry.name, err)
			}
			results <- err
		}(entry)
	}
	obs.SetReady(true)
	obs.Logger.Info().Msg("image service started")

	var serveErr error
	completed := 0
	select {
	case <-ctx.Done():
	case serveErr = <-results:
		completed++
	}
	obs.SetReady(false)
	active.drain()
	obs.Logger.Info().Msg("image service draining")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelShutdown()
	shutdownResults := make(chan error, len(servers))
	for _, entry := range servers {
		go func(entry managedServer) {
			if err := entry.server.Shutdown(shutdownCtx); err != nil {
				shutdownResults <- fmt.Errorf("shutdown %s: %w", entry.name, err)
				return
			}
			shutdownResults <- nil
		}(entry)
	}
	var shutdownErr error
	for range servers {
		shutdownErr = errors.Join(shutdownErr, <-shutdownResults)
	}
	if shutdownErr != nil {
		// Shutdown does not cancel active handlers on timeout. Close connections
		// and cancel their base context, then wait for cooperative work to stop.
		cancelRequests()
		for _, entry := range servers {
			_ = entry.server.Close()
		}
		obs.Logger.Warn().Msg("graceful shutdown timed out; active requests canceled")
	}
	for completed < len(servers) {
		serveErr = errors.Join(serveErr, <-results)
		completed++
	}
	cancelRequests()
	active.wait.Wait()
	obs.Logger.Info().Msg("image service stopped")
	return errors.Join(serveErr, shutdownErr)
}

// Admission and draining share a lock so no Add can race a shutdown Wait.
type activeRequests struct {
	mu       sync.Mutex
	stopping bool
	wait     sync.WaitGroup
}

func (a *activeRequests) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		if a.stopping {
			a.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "service shutting down", http.StatusServiceUnavailable)
			return
		}
		a.wait.Add(1)
		a.mu.Unlock()
		defer a.wait.Done()
		next.ServeHTTP(w, r)
	})
}

func (a *activeRequests) drain() {
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
}

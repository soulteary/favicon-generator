package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	healthkit "github.com/soulteary/health-kit/v4"
	loggerkit "github.com/soulteary/logger-kit/v3"
	metricskit "github.com/soulteary/metrics-kit/v3"
)

type observability struct {
	Logger            *loggerkit.Logger
	ProxyLogger       *log.Logger
	ServerErrorLogger *log.Logger

	ready       atomic.Bool
	metrics     *metricskit.HTTPMetrics
	metricsPage http.Handler
	liveness    http.Handler
	readiness   http.Handler
}

// Construct observability before starting request or log goroutines. logger-kit
// configures zerolog's process-wide timestamp format during construction; this
// service does not reconfigure loggers or log levels while serving requests.
func newObservability(out io.Writer, level string) (*observability, error) {
	if strings.TrimSpace(level) == "" {
		level = "info"
	}
	parsedLevel, err := loggerkit.ParseLevel(level)
	if err != nil {
		// Configuration values can themselves contain secrets.
		return nil, errors.New("invalid log level")
	}
	if out == nil {
		out = os.Stderr
	}
	loggerConfig := loggerkit.DefaultConfig()
	loggerConfig.Level = parsedLevel
	loggerConfig.Output = &synchronizedLogWriter{out: out}
	loggerConfig.ServiceName = "favicon-generator"
	o := &observability{Logger: loggerkit.New(loggerConfig)}
	o.ProxyLogger = log.New(safeDiagnosticWriter{logger: o.Logger, category: "image_proxy"}, "", 0)
	o.ServerErrorLogger = log.New(safeDiagnosticWriter{logger: o.Logger, category: "http_server", server: true}, "", 0)

	metricConfig := metricskit.DefaultHTTPMetricsConfig()
	metricConfig.Registry = metricskit.NewRegistryWithSubsystem("favicon_generator", "http")
	metricConfig.IncludeRequestSize = true
	metricConfig.IncludeResponseSize = true
	o.metrics = metricskit.NewHTTPMetrics(metricConfig)
	o.metricsPage = metricskit.HandlerFor(o.metrics.Registry)

	o.liveness = healthkit.LivenessHandler("favicon-generator")
	checks := healthkit.NewAggregator(healthkit.DefaultConfig().
		WithServiceName("favicon-generator").
		WithCriticalChecks([]string{"ready"}))
	checks.AddChecker(healthkit.NewCustomChecker("ready", func(context.Context) error {
		if !o.ready.Load() {
			return errors.New("service is not ready")
		}
		return nil
	}))
	o.readiness = healthkit.ReadinessHandler(checks)
	return o, nil
}

func (o *observability) SetReady(ready bool) { o.ready.Store(ready) }

func (o *observability) PublicHandler(proxy http.Handler) http.Handler {
	image := o.observe("image", proxy)
	health := o.management("health", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "OK")
	}))
	live := o.management("livez", o.liveness)
	ready := o.management("readyz", o.readiness)
	// Do not use ServeMux here: its slash canonicalization changes the source
	// URL embedded in an imageproxy path, for example /png/https://host/image.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health-check":
			health.ServeHTTP(w, r)
		case "/livez":
			live.ServeHTTP(w, r)
		case "/readyz":
			ready.ServeHTTP(w, r)
		case "/metrics":
			// Never send this reserved administration route to a configured
			// proxy BaseURL, even when the public listener has no metrics page.
			http.NotFound(w, r)
		default:
			image.ServeHTTP(w, r)
		}
	})
}

func (o *observability) AdminHandler() http.Handler {
	live := o.management("livez", o.liveness)
	ready := o.management("readyz", o.readiness)
	metrics := managementMethods(o.metricsPage)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			// Scraping must not add observations to the metrics being scraped.
			metrics.ServeHTTP(w, r)
		case "/livez":
			live.ServeHTTP(w, r)
		case "/readyz":
			ready.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (o *observability) management(route string, handler http.Handler) http.Handler {
	return o.observe(route, managementMethods(handler))
}

func managementMethods(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodHead {
			w = &headResponseWriter{ResponseWriter: w}
		}
		next.ServeHTTP(w, r)
	})
}

func (o *observability) observe(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		response := &observedResponseWriter{ResponseWriter: w}
		method := metricMethod(r.Method)
		o.metrics.RequestsInFlight.Inc()
		completed := false
		defer func() {
			o.metrics.RequestsInFlight.Dec()
			status := response.status
			if status == 0 {
				status = http.StatusOK
				if !completed {
					status = http.StatusInternalServerError
				}
			}
			duration := time.Since(start)
			o.metrics.RecordRequest(method, route, strconv.Itoa(status), duration)
			// Count known request-body lengths without reading or buffering a
			// body. Record zero-sized requests and HEAD responses as well.
			requestBytes := max(r.ContentLength, 0)
			o.metrics.RequestSize.WithLabelValues(method, route).Observe(float64(requestBytes))
			responseBytes := response.bytes
			if r.Method == http.MethodHead {
				responseBytes = 0
			}
			o.metrics.ResponseSize.WithLabelValues(method, route).Observe(float64(responseBytes))
			event := o.Logger.Debug
			if status >= http.StatusBadRequest {
				event = o.Logger.Warn
			}
			event().Str("route", route).Str("method", method).Int("status", status).
				Dur("duration", duration).Msg("http request")
		}()
		next.ServeHTTP(response, r)
		completed = true
	})
}

func metricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

type synchronizedLogWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *synchronizedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

// Standard-library diagnostics can contain url.Error values, panic values, and
// complete request targets. They are deliberately never copied into log fields.
type safeDiagnosticWriter struct {
	logger   *loggerkit.Logger
	category string
	server   bool
}

func (w safeDiagnosticWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	event := w.logger.Debug
	if w.server {
		event = w.logger.Error
	}
	event().Str("category", w.category).Msg("diagnostic details omitted")
	return len(p), nil
}

type headResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *headResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *headResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.ResponseWriter.WriteHeader(status)
	if status < 100 || status >= 200 || status == http.StatusSwitchingProtocols {
		w.wroteHeader = true
	}
}

func (w *headResponseWriter) Write(p []byte) (int, error) {
	// Keep the status and headers produced by the management handler, while
	// enforcing HEAD semantics even with ResponseRecorder or other adapters.
	w.WriteHeader(http.StatusOK)
	return len(p), nil
}

type observedResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *observedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *observedResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.ResponseWriter.WriteHeader(status)
	w.status = status
}

func (w *observedResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *observedResponseWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err == nil && w.status == 0 {
		w.status = http.StatusOK
	}
	return err
}

func (w *observedResponseWriter) Flush() { _ = w.FlushError() }

func (w *observedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil && w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (w *observedResponseWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/willnorris/imageproxy"
)

type iconHandler struct{ proxy *imageproxy.Proxy }

func (h *iconHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/icons/") {
		h.proxy.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodHead {
		w = &headResponseWriter{ResponseWriter: w}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	filename := strings.TrimPrefix(r.URL.Path, "/icons/")
	contentType := "image/png"
	if _, ok := iconPNGSizes[filename]; !ok {
		switch filename {
		case "favicon.ico":
			contentType = "image/vnd.microsoft.icon"
		case "site.webmanifest":
			contentType = "application/manifest+json"
		case "icons.zip":
			contentType = "application/zip"
		default:
			http.NotFound(w, r)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q, err := parseIconQuery(r.URL.RawQuery, filename)
	if err != nil {
		http.Error(w, "invalid icon parameters", http.StatusBadRequest)
		return
	}
	source, err := url.Parse(q.Get("url"))
	if err != nil {
		http.Error(w, "invalid source URL", http.StatusBadRequest)
		return
	}
	links := url.Values{"url": {q.Get("url")}}
	if q.Get("s") != "" {
		links.Set("s", q.Get("s"))
	}
	var output []byte
	err = h.proxy.WithSource(r.Context(), source, q.Get("s"), func(ctx context.Context, data []byte, limits imageproxy.SourceLimits) error {
		var err error
		output, err = generateIcon(ctx, data, limits, filename, q.Get("name"), q.Get("shortName"), links)
		return err
	})
	if err != nil {
		status, message := iconError(err)
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "1")
		}
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(output)))
	if filename == "icons.zip" {
		w.Header().Set("Content-Disposition", `attachment; filename="icons.zip"`)
	}
	if r.Method == http.MethodGet {
		_, _ = w.Write(output)
	}
}

func parseIconQuery(raw, filename string) (url.Values, error) {
	invalid := errors.New("invalid icon parameters")
	if len(raw) > 16384 {
		return nil, invalid
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, invalid
	}
	for key, values := range q {
		if len(values) != 1 {
			return nil, invalid
		}
		switch key {
		case "url", "s":
		case "name", "shortName":
			if filename != "icons.zip" && filename != "site.webmanifest" {
				return nil, invalid
			}
		default:
			return nil, invalid
		}
	}
	if q.Get("url") == "" || len(q.Get("url")) > 4096 || len(q.Get("s")) > 128 {
		return nil, invalid
	}
	for key, limit := range map[string]int{"name": 128, "shortName": 32} {
		value, exists := q[key]
		if !exists {
			q.Set(key, "App")
			continue
		}
		if strings.TrimSpace(value[0]) == "" || !utf8.ValidString(value[0]) || utf8.RuneCountInString(value[0]) > limit {
			return nil, invalid
		}
		for _, r := range value[0] {
			if unicode.IsControl(r) {
				return nil, invalid
			}
		}
	}
	return q, nil
}

func iconError(err error) (int, string) {
	switch {
	case errors.Is(err, imageproxy.ErrSourceForbidden):
		return http.StatusForbidden, "source not permitted"
	case errors.Is(err, imageproxy.ErrSourceNotFound):
		return http.StatusNotFound, "source not found"
	case errors.Is(err, imageproxy.ErrSourceBusy):
		return http.StatusServiceUnavailable, "image service busy"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusGatewayTimeout, "image request expired"
	case errors.Is(err, imageproxy.ErrResponseTooLarge), errors.Is(err, imageproxy.ErrImageTooLarge), errors.Is(err, imageproxy.ErrTooManyFrames):
		return http.StatusRequestEntityTooLarge, "image resource limit exceeded"
	case errors.Is(err, imageproxy.ErrInvalidDimensions):
		return http.StatusBadRequest, "invalid image dimensions"
	default:
		return http.StatusBadGateway, "image generation failed"
	}
}

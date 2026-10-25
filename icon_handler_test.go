package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willnorris/imageproxy"
)

type iconTransport func(*http.Request) (*http.Response, error)

func (f iconTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func iconSourcePNG(t *testing.T) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			m.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestIconHandler(data []byte) (*iconHandler, *atomic.Int32) {
	calls := new(atomic.Int32)
	p := imageproxy.NewProxy(iconTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}},
			Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}, nil
	}), nil)
	return &iconHandler{proxy: p}, calls
}

func iconRequest(h http.Handler, method, filename string, q url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, "/icons/"+filename+"?"+q.Encode(), nil))
	return w
}

func checkGeneratedPNG(t *testing.T, data []byte, size int) {
	t.Helper()
	m, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if m.Bounds() != image.Rect(0, 0, size, size) {
		t.Fatalf("PNG bounds = %v, want %dx%d", m.Bounds(), size, size)
	}
	_, _, _, alpha := m.At(0, 0).RGBA()
	if alpha != 0 {
		t.Fatal("rectangular logo lost its transparent padding")
	}
	r, _, _, a := m.At(size/2, size/2).RGBA()
	if r != 65535 || a != 65535 {
		t.Fatal("logo was cropped, lost, or not scaled to fit")
	}
}

func TestIconDownloadsAndPackage(t *testing.T) {
	h, _ := newTestIconHandler(iconSourcePNG(t))
	q := url.Values{"url": {"https://origin.test/logo.png?token=private"}, "name": {"网站 <测试>"}, "shortName": {"图标"}}
	w := iconRequest(h, "GET", "icons.zip", q)
	if w.Code != 200 {
		t.Fatalf("ZIP response = %d: %s", w.Code, w.Body.String())
	}
	disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] != "icons.zip" || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatal("missing download headers")
	}
	if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing private download policy")
	}
	if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
		t.Fatal("wrong download length")
	}
	z, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"favicon-16x16.png": 16, "favicon-32x32.png": 32, "favicon-48x48.png": 48,
		"apple-touch-icon.png": 180, "android-chrome-192x192.png": 192, "android-chrome-512x512.png": 512,
		"favicon.ico": 0, "site.webmanifest": 0}
	if len(z.File) != len(want) {
		t.Fatalf("members = %d, want %d", len(z.File), len(want))
	}
	for _, file := range z.File {
		size, ok := want[file.Name]
		if !ok {
			t.Fatalf("unexpected member %s", file.Name)
		}
		delete(want, file.Name)
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		switch file.Name {
		case "site.webmanifest":
			var m iconManifest
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			if m.Name != q.Get("name") || m.ShortName != q.Get("shortName") || len(m.Icons) != 2 {
				t.Fatalf("manifest = %+v", m)
			}
			for _, icon := range m.Icons {
				if strings.ContainsAny(icon.Src, "/?") || icon.Purpose != "any" {
					t.Fatalf("bundle manifest references external or maskable asset: %+v", icon)
				}
			}
			if bytes.Contains(data, []byte("private")) || bytes.Contains(data, []byte("origin.test")) {
				t.Fatal("bundle retains source credentials")
			}
		case "favicon.ico":
			if len(data) < 6 || binary.LittleEndian.Uint16(data[2:4]) != 1 || binary.LittleEndian.Uint16(data[4:6]) != 4 {
				t.Fatal("package does not contain multi-frame ICO")
			}
		default:
			checkGeneratedPNG(t, data, size)
		}
	}
}

func TestIndividualIconPNGAndHead(t *testing.T) {
	h, _ := newTestIconHandler(iconSourcePNG(t))
	q := url.Values{"url": {"https://origin.test/logo.png"}}
	for filename, size := range map[string]int{"favicon-16x16.png": 16, "favicon-32x32.png": 32, "favicon-48x48.png": 48, "apple-touch-icon.png": 180, "android-chrome-192x192.png": 192, "android-chrome-512x512.png": 512} {
		t.Run(filename, func(t *testing.T) {
			get := iconRequest(h, "GET", filename, q)
			if get.Code != 200 || get.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("GET = %d: %s", get.Code, get.Body.String())
			}
			checkGeneratedPNG(t, get.Body.Bytes(), size)
			head := iconRequest(h, "HEAD", filename, q)
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
				t.Fatal("HEAD differs from GET headers or has a body")
			}
		})
	}
	for _, filename := range []string{"favicon.ico", "icons.zip", "site.webmanifest"} {
		get, head := iconRequest(h, "GET", filename, q), iconRequest(h, "HEAD", filename, q)
		if get.Code != 200 || head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
			t.Fatalf("%s HEAD differs from GET", filename)
		}
	}
}

func TestStandaloneManifestLinksRetainSignature(t *testing.T) {
	h, _ := newTestIconHandler(iconSourcePNG(t))
	h.proxy.SignatureKeys = [][]byte{[]byte("key")}
	source := "https://origin.test/logo?x=1&name=source"
	mac := hmac.New(sha256.New, []byte("key"))
	_, _ = mac.Write([]byte(source))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	q := url.Values{"url": {source}, "s": {sig}, "name": {"Example"}, "shortName": {"Ex"}}
	w := iconRequest(h, "GET", "site.webmanifest", q)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest = %d %s", w.Code, w.Body.String())
	}
	var m iconManifest
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, icon := range m.Icons {
		u, err := url.Parse(icon.Src)
		if err != nil || u.Query().Get("url") != source || u.Query().Get("s") != sig {
			t.Fatalf("bad signed icon URL %q", icon.Src)
		}
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", icon.Src, nil))
		if r.Code != 200 {
			t.Fatalf("manifest asset = %d: %s", r.Code, r.Body.String())
		}
	}
	q.Del("s")
	if got := iconRequest(h, "GET", "site.webmanifest", q).Code; got != 403 {
		t.Fatalf("unsigned manifest bypassed policy: %d", got)
	}
}

func TestIconInputValidationBeforeFetch(t *testing.T) {
	h, calls := newTestIconHandler(iconSourcePNG(t))
	for _, query := range []string{"", "url=https://origin.test/a&url=https://other.test/a", "url=x&unknown=y", "url=x&name=", "url=x&shortName=" + strings.Repeat("界", 33), "url=x&name=%0A", "url=x&name=%FF", "url=x&s=" + strings.Repeat("x", 129), "url=%zz", "url=x&name=x&name=y", "url=" + strings.Repeat("x", 16385)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/icons/icons.zip?"+query, nil))
		if w.Code != 400 {
			t.Fatalf("invalid query accepted: status=%d", w.Code)
		}
	}
	for _, source := range []string{"file:///etc/passwd", "https://user:secret@origin.test/a", "http://127.0.0.1/a", "http://origin.test/a#png", "relative.png"} {
		if got := iconRequest(h, "GET", "favicon.ico", url.Values{"url": {source}}).Code; got != 403 {
			t.Fatalf("unsafe source status=%d", got)
		}
	}
	if got := iconRequest(h, "POST", "icons.zip", url.Values{"url": {"https://origin.test/a"}}); got.Code != 405 || got.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("wrong method policy")
	}
	if got := iconRequest(h, "GET", "../escape.zip", nil).Code; got != 404 {
		t.Fatalf("invalid path status = %d", got)
	}
	if got := iconRequest(h, "GET", "favicon-128x128.png", nil).Code; got != 404 {
		t.Fatalf("unsupported size status = %d", got)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached origin")
	}
	if head := iconRequest(h, "HEAD", "icons.zip", nil); head.Code != 400 || head.Body.Len() != 0 {
		t.Fatal("HEAD error contains a body or has wrong status")
	}
}

func TestIconAggregateBudgets(t *testing.T) {
	data := iconSourcePNG(t)
	for _, filename := range []string{"favicon.ico", "icons.zip"} {
		h, _ := newTestIconHandler(data)
		h.proxy.MaxPixels = 256 * 256
		if filename == "icons.zip" {
			h.proxy.MaxPixels = 512 * 512
		}
		w := iconRequest(h, "GET", filename, url.Values{"url": {"https://origin.test/a"}})
		if w.Code != 413 {
			t.Fatalf("%s ignored aggregate output pixels: %d", filename, w.Code)
		}
	}
	limits := imageproxy.SourceLimits{MaxResponseBytes: 20 << 20, MaxPixels: 16_000_000, MaxFrames: 100}
	var maxFile int
	for filename := range iconPNGSizes {
		p, err := generateIcon(context.Background(), data, limits, filename, "App", "App", nil)
		if err != nil {
			t.Fatal(err)
		}
		maxFile = max(maxFile, len(p))
	}
	limits.MaxResponseBytes = int64(maxFile + 1)
	if _, err := generateIcon(context.Background(), data, limits, "icons.zip", "App", "App", nil); !errors.Is(err, imageproxy.ErrResponseTooLarge) {
		t.Fatalf("aggregate encoded bytes error = %v", err)
	}
	h, _ := newTestIconHandler(data)
	h.proxy.MaxResponseBytes = int64(len(data) + 1)
	if w := iconRequest(h, "GET", "android-chrome-512x512.png", url.Values{"url": {"https://origin.test/a"}}); w.Code != 413 {
		t.Fatalf("PNG output bytes not bounded: %d", w.Code)
	}
}

func TestIconAnimationUsesFirstFrameAndEnforcesFrameBudget(t *testing.T) {
	palette := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	a, b := image.NewPaletted(image.Rect(0, 0, 4, 2), palette), image.NewPaletted(image.Rect(0, 0, 4, 2), palette)
	for i := range b.Pix {
		b.Pix[i] = 1
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{a, b}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	h, _ := newTestIconHandler(buf.Bytes())
	q := url.Values{"url": {"https://origin.test/a.gif"}}
	w := iconRequest(h, "GET", "favicon-32x32.png", q)
	if w.Code != 200 {
		t.Fatalf("GIF = %d: %s", w.Code, w.Body.String())
	}
	checkGeneratedPNG(t, w.Body.Bytes(), 32)
	h, _ = newTestIconHandler(buf.Bytes())
	h.proxy.MaxFrames = 1
	if w := iconRequest(h, "GET", "favicon-32x32.png", q); w.Code != 413 {
		t.Fatalf("GIF frame budget ignored: %d", w.Code)
	}
}

func TestIconErrorsAreRedacted(t *testing.T) {
	for _, tc := range []struct {
		status       int
		transportErr error
		want         int
	}{
		{404, nil, 404}, {500, nil, 502}, {200, errors.New("secret upstream URL https://private/token"), 502},
	} {
		p := imageproxy.NewProxy(iconTransport(func(r *http.Request) (*http.Response, error) {
			if tc.transportErr != nil {
				return nil, tc.transportErr
			}
			return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("secret")), Request: r}, nil
		}), nil)
		w := iconRequest(&iconHandler{proxy: p}, "GET", "icons.zip", url.Values{"url": {"https://origin.test/a?token=secret"}})
		if w.Code != tc.want || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "origin.test") {
			t.Fatalf("unsafe error = %d %q", w.Code, w.Body.String())
		}
	}
}

func TestIconServiceSharesAdmissionAndKeepsHealthAvailable(t *testing.T) {
	data := iconSourcePNG(t)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	p := imageproxy.NewProxy(iconTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}, nil
	}), nil)
	p.MaxConcurrentRequests = 1
	s := startLifecycleService(t, lifecycleConfig(t), &iconHandler{proxy: p})
	client := lifecycleClient(t)
	base := "http://" + s.listeners[0].Addr().String()
	q := url.Values{"url": {"https://origin.test/logo?token=TOKEN_SECRET"}}
	finished := make(chan error, 1)
	go func() {
		resp, err := client.Get(base + "/icons/icons.zip?" + q.Encode())
		if err == nil {
			defer resp.Body.Close()
			var data []byte
			data, err = io.ReadAll(resp.Body)
			if err == nil && resp.StatusCode != 200 {
				err = errors.New("icon package failed")
			}
			if err == nil {
				_, err = zip.NewReader(bytes.NewReader(data), int64(len(data)))
			}
		}
		finished <- err
	}()
	lifecycleWait(t, started)
	for _, path := range []string{"/icons/favicon-32x32.png?" + q.Encode(), "/32,png/https://origin.test/logo"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
			t.Fatalf("concurrent image request = %d", resp.StatusCode)
		}
	}
	resp, err := client.Get(base + "/health-check")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("full image capacity blocks health checks")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("icon request did not finish")
	}
	resp, err = client.Get("http://" + s.listeners[1].Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(metrics, []byte(`path="icons"`)) || bytes.Contains(metrics, []byte("TOKEN_SECRET")) || bytes.Contains(metrics, []byte("origin.test")) {
		t.Fatal("icons metrics are missing or expose source URLs")
	}
}

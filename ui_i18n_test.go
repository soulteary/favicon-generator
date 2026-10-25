package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/willnorris/imageproxy"
)

func TestUILanguageNegotiation(t *testing.T) {
	for _, tt := range []struct{ query, accept, want string }{
		{"", "", "zh-CN"}, {"lang=en", "zh-CN", "en"}, {"lang=zh-CN", "en", "zh-CN"},
		{"", "fr, en-US;q=0.8, zh-CN;q=0.6", "en"}, {"", "en;q=0,zh-CN;q=0.4", "zh-CN"},
		{"", "zh-CN;q=0,en;q=0.1", "en"}, {"", "en;q=0.5,zh-CN;q=0.5", "en"},
		{"", "zh-Hans;q=0.8,en;q=0.7", "zh-CN"}, {"", "fr, *;q=1", "zh-CN"},
		{"", "en;q=NaN", "zh-CN"}, {"", "en;q=2", "zh-CN"}, {"", "en;q=-1", "zh-CN"},
		{"", "en;q=.5", "zh-CN"}, {"", "en;q=0.1234", "zh-CN"}, {"", "en;q=1.001", "zh-CN"},
		{"", "en;q=0.9;extra=1", "zh-CN"}, {"", "en;q=1.000", "en"},
		{"", "en-,en--US,en-ABCDEFGHI,en-@;q=1", "zh-CN"},
		{"url=SOURCE_SECRET&s=SIGNATURE_SECRET", "en-GB", "en"},
		{"", strings.Repeat("en,", 2049), "zh-CN"},
	} {
		t.Run(tt.query+"/"+tt.accept, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/?"+tt.query, nil)
			r.Header.Set("Accept-Language", tt.accept)
			got, err := uiRequestLocale(r)
			if err != nil || got != tt.want {
				t.Fatalf("locale=%q, want %q, err=%v", got, tt.want, err)
			}
		})
	}
	for _, query := range []string{"lang=fr", "lang=", "lang=en&lang=zh-CN", "lang=%zz"} {
		if _, err := uiRequestLocale(httptest.NewRequest("GET", "/?"+query, nil)); err == nil {
			t.Fatalf("invalid language query accepted: %q", query)
		}
	}
}

func TestUILocalizedResourcesAndHead(t *testing.T) {
	h, calls := newTestIconHandler(iconSourcePNG(t))
	for _, locale := range uiLocaleNames {
		for _, path := range []string{"/?lang=" + locale, "/assets/locales/" + locale + ".json"} {
			get := httptest.NewRecorder()
			h.ServeHTTP(get, httptest.NewRequest("GET", path, nil))
			head := httptest.NewRecorder()
			h.ServeHTTP(head, httptest.NewRequest("HEAD", path, nil))
			if get.Code != 200 || head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
				t.Fatalf("localized HEAD differs: %s", path)
			}
			for _, w := range []*httptest.ResponseRecorder{get, head} {
				if w.Header().Get("Content-Language") != locale || w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("Content-Security-Policy") != uiContentSecurityPolicy {
					t.Fatalf("localized resource headers differ: %s", path)
				}
			}
			if strings.HasPrefix(path, "/?") {
				if get.Header().Get("Vary") != "Accept-Language" || !strings.Contains(get.Body.String(), `<html lang="`+locale+`">`) || strings.Contains(get.Body.String(), "{{") {
					t.Fatalf("HTML did not render the negotiated locale: %s", path)
				}
			} else {
				var messages map[string]string
				if get.Header().Get("Content-Type") != "application/json; charset=utf-8" || get.Header().Get("Vary") != "" || json.Unmarshal(get.Body.Bytes(), &messages) != nil || messages["error.source_forbidden"] == "" {
					t.Fatalf("invalid fixed locale resource: %s", path)
				}
			}
		}
	}
	for _, path := range []string{"/assets/locales/fr.json", "/assets/locales/en.json/extra", "/assets/locales/../go.mod", "/assets/ui_i18n.go"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("unexpected locale asset exposed: %s", path)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("language resources reached the image source")
	}
}

func TestUILanguageConcurrentIsolation(t *testing.T) {
	h, _ := newTestIconHandler(iconSourcePNG(t))
	var workers sync.WaitGroup
	failures := make(chan string, 32)
	for index := range 32 {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			locale, title := "zh-CN", "从一张图片，生成整套图标。"
			if index%2 == 1 {
				locale, title = "en", "One image. A complete set of icons."
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/?lang="+locale+"&url=SOURCE_SECRET&s=SIGNATURE_SECRET", nil)
			h.ServeHTTP(w, r)
			if w.Code != 200 || w.Header().Get("Content-Language") != locale || !strings.Contains(w.Body.String(), title) || strings.Contains(w.Body.String(), "SECRET") {
				failures <- locale
			}
		}(index)
	}
	workers.Wait()
	close(failures)
	for locale := range failures {
		t.Errorf("concurrent request did not isolate %s", locale)
	}
}

func TestUICatalogValidationAndTemplateEscaping(t *testing.T) {
	files := fstest.MapFS{}
	for _, locale := range uiLocaleNames {
		name := "web/locales/" + locale + ".json"
		data, err := uiLocaleFiles.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: data}
	}
	var english map[string]string
	if err := json.Unmarshal(files["web/locales/en.json"].Data, &english); err != nil {
		t.Fatal(err)
	}
	english["intro.title"] = `<script>SECRET</script>`
	data, _ := json.Marshal(english)
	files["web/locales/en.json"] = &fstest.MapFile{Data: data}
	translations, err := newUITranslations(files)
	if err != nil || strings.Contains(translations.pages["en"], `<script>SECRET`) || !strings.Contains(translations.pages["en"], "&lt;script&gt;SECRET&lt;/script&gt;") {
		t.Fatal("template did not escape translated text")
	}
	english["status.progress"] = "SECRET {unmatched}"
	data, _ = json.Marshal(english)
	files["web/locales/en.json"] = &fstest.MapFile{Data: data}
	if _, err := newUITranslations(files); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("invalid placeholder data was accepted or leaked")
	}
	delete(english, "status.progress")
	data, _ = json.Marshal(english)
	files["web/locales/en.json"] = &fstest.MapFile{Data: data}
	if _, err := newUITranslations(files); err == nil {
		t.Fatal("mismatched key sets were accepted")
	}
}

func TestIconErrorCodesKeepEnglishProtocol(t *testing.T) {
	for _, tt := range []struct {
		err  error
		code string
	}{
		{imageproxy.ErrSourceForbidden, "source_forbidden"}, {imageproxy.ErrSourceNotFound, "source_not_found"},
		{imageproxy.ErrSourceBusy, "busy"}, {imageproxy.ErrResponseTooLarge, "resource_limit"},
		{imageproxy.ErrInvalidDimensions, "invalid_parameters"}, {errors.New("SOURCE_SECRET"), "upstream_failure"},
	} {
		status, message := iconError(tt.err)
		w := httptest.NewRecorder()
		writeIconError(w, message, status)
		if w.Code != status || w.Body.String() != message+"\n" || w.Header().Get(iconErrorCodeHeader) != tt.code || strings.Contains(w.Body.String(), "SECRET") {
			t.Fatalf("changed error protocol for %s", tt.code)
		}
	}
	h, calls := newTestIconHandler(iconSourcePNG(t))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/icons/icons.zip?lang=en", nil))
		if w.Code != 400 || w.Header().Get(iconErrorCodeHeader) != "invalid_parameters" || method == http.MethodHead && w.Body.Len() != 0 {
			t.Fatal("invalid icon query did not keep safe code/header semantics")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid icon query reached source")
	}
}

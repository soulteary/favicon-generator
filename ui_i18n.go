package main

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	i18n "github.com/soulteary/i18n-kit/v4"
)

//go:embed web/locales/zh-CN.json web/locales/en.json
var uiLocaleFiles embed.FS

var uiLocaleNames = [...]string{"zh-CN", "en"}

type uiTranslations struct {
	bundle *i18n.Bundle
	pages  map[string]string
	json   map[string]string
}

var uiTranslationState struct {
	sync.Once
	translations *uiTranslations
	err          error
}

func loadUITranslations() (*uiTranslations, error) {
	uiTranslationState.Do(func() {
		uiTranslationState.translations, uiTranslationState.err = newUITranslations(uiLocaleFiles)
	})
	return uiTranslationState.translations, uiTranslationState.err
}

func newUITranslations(files fs.FS) (*uiTranslations, error) {
	result := &uiTranslations{bundle: i18n.NewBundle(i18n.LangZH), pages: make(map[string]string), json: make(map[string]string)}
	var first map[string]string
	for _, locale := range uiLocaleNames {
		data, err := fs.ReadFile(files, "web/locales/"+locale+".json")
		if err != nil || !utf8.Valid(data) {
			return nil, errors.New("invalid embedded UI translations")
		}
		messages, err := i18n.DecodeJSON(data)
		if err != nil || len(messages) == 0 || first != nil && len(first) != len(messages) {
			return nil, errors.New("invalid embedded UI translations")
		}
		for key, value := range messages {
			if strings.TrimSpace(value) == "" || first != nil && first[key] == "" {
				return nil, errors.New("invalid embedded UI translations")
			}
			if first != nil && !sameUIParams(first[key], value) {
				return nil, errors.New("invalid embedded UI translation parameters")
			}
		}
		if first == nil {
			first = messages
		}
		if err := result.bundle.LoadJSON(uiKitLanguage(locale), data); err != nil {
			return nil, errors.New("invalid embedded UI translations")
		}
		result.json[locale] = string(data)
	}
	for _, locale := range uiLocaleNames {
		lang := uiKitLanguage(locale)
		page, err := template.New("index").Funcs(template.FuncMap{"t": func(key string) (string, error) {
			if !result.bundle.HasTranslation(lang, key) {
				return "", errors.New("missing embedded UI translation")
			}
			text, _ := result.bundle.LookupTranslation(lang, key)
			return text, nil
		}}).Parse(uiHTML)
		if err != nil {
			return nil, errors.New("invalid embedded UI template")
		}
		var rendered bytes.Buffer
		if err := page.Execute(&rendered, struct{ Locale string }{locale}); err != nil {
			return nil, errors.New("invalid embedded UI template")
		}
		result.pages[locale] = rendered.String()
	}
	return result, nil
}

func sameUIParams(a, b string) bool {
	aParams, bParams := i18n.ExtractParams(a), i18n.ExtractParams(b)
	if len(aParams) != len(bParams) {
		return false
	}
	for _, name := range aParams {
		found := false
		for _, other := range bParams {
			found = found || name == other
		}
		if !found {
			return false
		}
	}
	return true
}

func uiKitLanguage(locale string) i18n.Language {
	if locale == "en" {
		return i18n.LangEN
	}
	return i18n.LangZH
}

func normalizeUILocale(value string) string {
	value = strings.ToLower(value)
	if value == "zh" || value == "zh-cn" || value == "zh-hans" {
		return "zh-CN"
	}
	if value == "en" {
		return "en"
	}
	if strings.HasPrefix(value, "en-") {
		for _, part := range strings.Split(value[3:], "-") {
			if len(part) == 0 || len(part) > 8 {
				return ""
			}
			for _, char := range part {
				if char < 'a' || char > 'z' {
					if char < '0' || char > '9' {
						return ""
					}
				}
			}
		}
		return "en"
	}
	return ""
}

// Only two project locales are negotiated. Unlike the kit's general detector,
// q=0 never selects a language and unsupported languages do not hide a later
// supported preference. Explicit UI language is never forwarded to image URLs.
func uiRequestLocale(r *http.Request) (string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", errors.New("invalid UI language")
	}
	if values, exists := query["lang"]; exists {
		if len(values) != 1 || values[0] != "zh-CN" && values[0] != "en" {
			return "", errors.New("invalid UI language")
		}
		return values[0], nil
	}
	header := r.Header.Get("Accept-Language")
	if len(header) > 4096 {
		return "zh-CN", nil
	}
	best, bestQuality := "zh-CN", -1
	for i, item := range strings.Split(header, ",") {
		if i >= 32 {
			break
		}
		parts := strings.Split(strings.TrimSpace(item), ";")
		locale := normalizeUILocale(strings.TrimSpace(parts[0]))
		if locale == "" || len(parts) > 2 {
			continue
		}
		quality := 1000
		if len(parts) == 2 {
			q := strings.TrimSpace(parts[1])
			if !strings.HasPrefix(q, "q=") {
				continue
			}
			var ok bool
			quality, ok = uiLanguageQuality(q[2:])
			if !ok {
				continue
			}
		}
		if quality > 0 && quality > bestQuality {
			best, bestQuality = locale, quality
		}
	}
	return best, nil
}

func uiLanguageQuality(value string) (int, bool) {
	if value == "0" || value == "1" {
		return int(value[0]-'0') * 1000, true
	}
	if len(value) < 2 || len(value) > 5 || value[1] != '.' || value[0] != '0' && value[0] != '1' {
		return 0, false
	}
	fraction := value[2:]
	for _, digit := range fraction {
		if digit < '0' || digit > '9' || value[0] == '1' && digit != '0' {
			return 0, false
		}
	}
	if fraction == "" {
		fraction = "0"
	}
	n, err := strconv.Atoi(fraction + strings.Repeat("0", 3-len(fraction)))
	return int(value[0]-'0')*1000 + n, err == nil
}

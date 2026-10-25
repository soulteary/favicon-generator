package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/soulteary/cli-kit/validator"
	"github.com/willnorris/imageproxy"
)

// Config is the validated startup configuration. Cache allocation is deferred
// until NewCache is called, so help and version requests never create a cache.
type Config struct {
	Addr, AdminAddr, LogLevel                  string
	FollowRedirects, ScaleUp, Verbose, Version bool
	BaseURL                                    *url.URL
	AllowHosts, ContentTypes                   []string
	SignatureKey                               string
	Timeout, ShutdownTimeout                   time.Duration
	MaxResponseBytes, MaxPixels                int64
	MaxFrames, MaxConcurrent                   int
	Cache                                      []CacheConfig
}

type configStrings struct {
	baseURL, allowHosts, contentTypes string
}

func configFlags(output io.Writer) (*flag.FlagSet, *Config, *configStrings) {
	c := &Config{}
	raw := &configStrings{}
	fs := flag.NewFlagSet("favicon-generator", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&c.Addr, "addr", "localhost:8080", "TCP address to listen on")
	fs.StringVar(&c.AdminAddr, "adminAddr", "localhost:9090", "management address; empty disables management HTTP")
	fs.StringVar(&c.LogLevel, "logLevel", "info", "log level: trace, debug, info, warn, error, fatal, panic, disabled")
	fs.BoolVar(&c.FollowRedirects, "followRedirects", true, "follow upstream redirects")
	fs.BoolVar(&c.ScaleUp, "scaleUp", false, "allow images to exceed their original dimensions")
	fs.BoolVar(&c.Verbose, "verbose", false, "enable debug logging")
	fs.BoolVar(&c.Version, "version", false, "print build information and exit")
	fs.StringVar(&raw.baseURL, "baseURL", "", "absolute HTTP(S) base URL for relative image URLs")
	fs.StringVar(&raw.allowHosts, "allowHosts", "", "comma-separated remote host allowlist")
	fs.StringVar(&raw.contentTypes, "contentTypes", "image/*", "comma-separated allowed content type patterns")
	fs.StringVar(&c.SignatureKey, "signatureKey", "", "optional HMAC key required for image requests")
	fs.DurationVar(&c.Timeout, "timeout", imageproxy.DefaultTimeout, "time limit for fetching and processing an image")
	fs.DurationVar(&c.ShutdownTimeout, "shutdownTimeout", 15*time.Second, "maximum time to drain requests at shutdown")
	fs.Int64Var(&c.MaxResponseBytes, "maxResponseBytes", imageproxy.DefaultMaxResponseBytes, "maximum downloaded and returned image bytes")
	fs.Int64Var(&c.MaxPixels, "maxPixels", imageproxy.DefaultMaxPixels, "maximum decoded, intermediate and output pixels")
	fs.IntVar(&c.MaxFrames, "maxFrames", imageproxy.DefaultMaxFrames, "maximum GIF frames")
	fs.IntVar(&c.MaxConcurrent, "maxConcurrent", imageproxy.DefaultMaxConcurrentRequests, "maximum active image requests")
	fs.Var(cacheConfigValue{target: &c.Cache}, "cache", "memory:<MB>[:age]; repeat for multiple tiers; memory means 100 MB")
	fs.VisitAll(func(f *flag.Flag) {
		f.Usage += " [" + configEnvName(f.Name) + "]"
	})
	return fs, c, raw
}

func printConfigUsage(w io.Writer) {
	fs, _, _ := configFlags(w)
	fmt.Fprintln(w, "Usage: favicon-generator [options]")
	fs.PrintDefaults()
}

func configEnvName(name string) string {
	return "IMAGEPROXY_" + strings.ToUpper(name)
}

// parseConfig resolves only the winning source for each flag. No lower-priority
// value is parsed after an explicit CLI flag, and invalid values never fall back.
// Errors deliberately omit input values, which may contain credentials.
func parseConfig(args []string, lookupEnv func(string) (string, bool)) (Config, error) {
	fs, c, raw := configFlags(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, flag.ErrHelp
		}
		return Config{}, errors.New("invalid command-line options; use -h for help")
	}
	if fs.NArg() != 0 {
		return Config{}, errors.New("unexpected positional arguments")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	// Validate explicit CLI values before the version shortcut, but do not even
	// consult unrelated environment settings for a version-only invocation.
	if err := validateConfig(c, raw); err != nil {
		return Config{}, err
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if !set["version"] {
		if value, ok := lookupEnv(configEnvName("version")); ok {
			if err := fs.Set("version", value); err != nil {
				return Config{}, invalidConfig("version", "must be true or false")
			}
		}
	}
	if c.Version {
		return *c, nil
	}
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if envErr != nil || set[f.Name] || f.Name == "version" {
			return
		}
		if value, ok := lookupEnv(configEnvName(f.Name)); ok {
			if err := fs.Set(f.Name, value); err != nil {
				envErr = invalidConfig(f.Name, "invalid value")
			}
		}
	})
	if envErr != nil {
		return Config{}, envErr
	}
	if err := validateConfig(c, raw); err != nil {
		return Config{}, err
	}
	return *c, nil
}

func invalidConfig(name, reason string) error {
	return fmt.Errorf("invalid -%s / %s: %s", name, configEnvName(name), reason)
}

func validateConfig(c *Config, raw *configStrings) error {
	if !validListenAddress(c.Addr) {
		return invalidConfig("addr", "must be a valid host:port address")
	}
	if c.AdminAddr != "" && !validListenAddress(c.AdminAddr) {
		return invalidConfig("adminAddr", "must be empty or a valid host:port address")
	}
	level := strings.ToLower(strings.TrimSpace(c.LogLevel))
	// Kit validator errors include the rejected value. Translate failures to
	// our fixed diagnostics rather than returning those errors to the caller.
	if validator.ValidateEnumCaseSensitive(level, []string{"trace", "debug", "info", "warn", "error", "fatal", "panic", "disabled"}) != nil {
		return invalidConfig("logLevel", "unsupported log level")
	}
	c.LogLevel = level
	// The HTTP server adds a five-second write allowance to this deadline.
	if validator.ValidateInRangeInt64(int64(c.Timeout), 1, math.MaxInt64-int64(5*time.Second)) != nil {
		return invalidConfig("timeout", "must be positive and leave room for the HTTP write deadline")
	}
	if validator.ValidatePositiveInt64(int64(c.ShutdownTimeout)) != nil {
		return invalidConfig("shutdownTimeout", "must be positive")
	}
	if validator.ValidateInRangeInt64(c.MaxResponseBytes, 1, int64(math.MaxInt)-1) != nil {
		return invalidConfig("maxResponseBytes", "must be positive and fit in a byte buffer with overflow detection")
	}
	if validator.ValidateInRangeInt64(c.MaxPixels, 1, int64(math.MaxInt)/16) != nil {
		return invalidConfig("maxPixels", "must be positive and fit the decoder allocation bound")
	}
	if validator.ValidatePositive(c.MaxFrames) != nil {
		return invalidConfig("maxFrames", "must be positive")
	}
	if validator.ValidatePositive(c.MaxConcurrent) != nil {
		return invalidConfig("maxConcurrent", "must be positive")
	}
	c.BaseURL = nil
	if raw.baseURL != "" {
		u, err := url.Parse(raw.baseURL)
		if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil || strings.Contains(raw.baseURL, "#") ||
			(u.Scheme != "http" && u.Scheme != "https") || !validURLHost(u) {
			return invalidConfig("baseURL", "must be an absolute HTTP(S) URL without userinfo or a fragment")
		}
		c.BaseURL = u
	}
	c.AllowHosts = nil
	allowHosts := splitConfigList(raw.allowHosts)
	if raw.allowHosts != "" && len(allowHosts) == 0 {
		return invalidConfig("allowHosts", "must contain at least one hostname, wildcard or CIDR")
	}
	for _, host := range allowHosts {
		host = strings.ToLower(host)
		if _, err := netip.ParsePrefix(host); err != nil {
			if strings.HasPrefix(host, "*.") {
				host = "*." + strings.TrimSuffix(host[2:], ".")
				if !validConfigHost(host[2:]) || strings.Contains(host[2:], "*") {
					return invalidConfig("allowHosts", "invalid hostname, wildcard or CIDR")
				}
			} else if !validConfigHost(host) {
				return invalidConfig("allowHosts", "invalid hostname, wildcard or CIDR")
			}
		}
		c.AllowHosts = append(c.AllowHosts, host)
	}
	c.ContentTypes = splitConfigList(raw.contentTypes)
	if raw.contentTypes != "" && len(c.ContentTypes) == 0 {
		return invalidConfig("contentTypes", "must contain at least one content type pattern")
	}
	for _, pattern := range c.ContentTypes {
		if _, err := path.Match(pattern, "image/png"); err != nil {
			return invalidConfig("contentTypes", "invalid content type pattern")
		}
	}
	return nil
}

func splitConfigList(value string) []string {
	var result []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func validListenAddress(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || (host != "" && !validConfigHost(host)) || !validConfigPort(port) {
		return false
	}
	if strings.HasPrefix(value, "[") {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is6()
	}
	return true
}

func validConfigPort(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	port, err := strconv.ParseUint(value, 10, 16)
	return err == nil && port <= 65535
}

func validConfigHost(value string) bool {
	if strings.ContainsAny(value, "\x00\r\n\t /\\?#@") {
		return false
	}
	if _, err := netip.ParseAddr(value); err == nil {
		return true
	}
	value = strings.TrimSuffix(value, ".")
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if char != '-' && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
				return false
			}
		}
	}
	return true
}

func validURLHost(u *url.URL) bool {
	if !validConfigHost(u.Hostname()) {
		return false
	}
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil || !ip.Is6() {
			return false
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		return validConfigPort(port)
	}
	return true
}

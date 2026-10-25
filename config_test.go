package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/die-net/lrucache"
	"github.com/willnorris/imageproxy"
)

func configEnv(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func TestConfigDefaultsAndNoCache(t *testing.T) {
	cfg, err := parseConfig(nil, configEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "localhost:8080" || cfg.AdminAddr != "localhost:9090" || cfg.LogLevel != "info" || cfg.Timeout != 15*time.Second || cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("unexpected defaults: addr=%s, admin=%s, level=%s, timeouts=%s/%s", cfg.Addr, cfg.AdminAddr, cfg.LogLevel, cfg.Timeout, cfg.ShutdownTimeout)
	}
	if !cfg.FollowRedirects || cfg.ScaleUp || cfg.Verbose || cfg.Version || len(cfg.Cache) != 0 || cfg.NewCache() != imageproxy.NopCache {
		t.Fatal("default switches or no-cache behavior changed")
	}
	if cfg.MaxResponseBytes != imageproxy.DefaultMaxResponseBytes || cfg.MaxPixels != imageproxy.DefaultMaxPixels || cfg.MaxFrames != imageproxy.DefaultMaxFrames || cfg.MaxConcurrent != imageproxy.DefaultMaxConcurrentRequests {
		t.Fatal("image budget defaults changed")
	}
}

func TestConfigCLIOverridesOnlyMatchingEnvironment(t *testing.T) {
	cfg, err := parseConfig([]string{"-timeout", "2s", "-followRedirects=false", "-cache", "memory:2:2s", "-cache", "memory:3", "-signatureKey", "cli-key", "-adminAddr="}, configEnv(map[string]string{
		"IMAGEPROXY_TIMEOUT": "invalid", "IMAGEPROXY_FOLLOWREDIRECTS": "invalid", "IMAGEPROXY_CACHE": "invalid", "IMAGEPROXY_SIGNATUREKEY": "env-key", "IMAGEPROXY_ADMINADDR": "invalid", "IMAGEPROXY_MAXPIXELS": "42",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 2*time.Second || cfg.FollowRedirects || cfg.AdminAddr != "" || cfg.MaxPixels != 42 || cfg.SignatureKey != "cli-key" {
		t.Fatal("CLI / environment priority not respected")
	}
	if !reflect.DeepEqual(cfg.Cache, []CacheConfig{{maxBytes: 2_000_000, maxAgeSeconds: 2}, {maxBytes: 3_000_000}}) {
		t.Fatalf("cache tiers = %v; environment should not append a tier", cfg.Cache)
	}
}

func TestConfigEnvironmentNamesAndRepeatedCLI(t *testing.T) {
	calls := map[string]int{}
	values := map[string]string{
		"IMAGEPROXY_ADDR": ":0", "IMAGEPROXY_ADMINADDR": "[::1]:0", "IMAGEPROXY_BASEURL": "https://example.com/images/", "IMAGEPROXY_ALLOWHOSTS": "example.com, *.example.org,192.0.2.0/24", "IMAGEPROXY_CONTENTTYPES": "image/png,image/jpeg", "IMAGEPROXY_SCALEUP": "true", "IMAGEPROXY_VERBOSE": "true", "IMAGEPROXY_LOGLEVEL": "trace", "IMAGEPROXY_SHUTDOWNTIMEOUT": "2s", "IMAGEPROXY_MAXRESPONSEBYTES": "1024", "IMAGEPROXY_MAXFRAMES": "2", "IMAGEPROXY_MAXCONCURRENT": "3", "IMAGEPROXY_CACHE": "memory:2:500ms memory:3:1500ms",
	}
	cfg, err := parseConfig([]string{"-timeout=1s", "-timeout=3s"}, func(key string) (string, bool) { calls[key]++; return configEnv(values)(key) })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":0" || cfg.AdminAddr != "[::1]:0" || cfg.BaseURL.String() != "https://example.com/images/" || cfg.Timeout != 3*time.Second || cfg.ShutdownTimeout != 2*time.Second || !cfg.ScaleUp || cfg.LogLevel != "trace" {
		t.Fatal("legacy environment names or repeated CLI flags changed")
	}
	if !reflect.DeepEqual(cfg.ContentTypes, []string{"image/png", "image/jpeg"}) || len(cfg.AllowHosts) != 3 || cfg.MaxResponseBytes != 1024 || cfg.MaxFrames != 2 || cfg.MaxConcurrent != 3 {
		t.Fatal("environment values not applied")
	}
	if !reflect.DeepEqual(cfg.Cache, []CacheConfig{{maxBytes: 2_000_000, maxAgeSeconds: 1}, {maxBytes: 3_000_000, maxAgeSeconds: 2}}) {
		t.Fatalf("cache TTLs: %v", cfg.Cache)
	}
	for key, count := range calls {
		if count != 1 {
			t.Errorf("%s looked up %d times", key, count)
		}
	}
	if calls["IMAGEPROXY_TIMEOUT"] != 0 {
		t.Fatal("overridden environment was consulted")
	}
	verbose, err := parseConfig([]string{"-verbose", "-logLevel=error"}, configEnv(nil))
	if err != nil || !verbose.Verbose || verbose.LogLevel != "error" {
		t.Fatalf("configured verbose=%v, level=%s, err=%v", verbose.Verbose, verbose.LogLevel, err)
	}
}

func TestConfigExplicitEmptyLists(t *testing.T) {
	for _, cli := range []bool{false, true} {
		var args []string
		var env map[string]string
		if cli {
			args = []string{"-allowHosts=", "-contentTypes="}
		} else {
			env = map[string]string{"IMAGEPROXY_ALLOWHOSTS": "", "IMAGEPROXY_CONTENTTYPES": ""}
		}
		cfg, err := parseConfig(args, configEnv(env))
		if err != nil || len(cfg.AllowHosts) != 0 || len(cfg.ContentTypes) != 0 {
			t.Fatalf("explicit empty lists rejected (CLI=%v): %v", cli, err)
		}
	}
}

func TestConfigVersionDoesNotConsultUnrelatedEnvironment(t *testing.T) {
	for _, args := range [][]string{{"-version"}, nil} {
		cfg, err := parseConfig(args, func(key string) (string, bool) {
			if key == "IMAGEPROXY_VERSION" {
				return "true", true
			}
			t.Errorf("version consulted unrelated environment %s", key)
			return "invalid", true
		})
		if err != nil || !cfg.Version || len(cfg.Cache) != 0 {
			t.Fatalf("version=%v, tiers=%d, err=%v", cfg.Version, len(cfg.Cache), err)
		}
	}
	cfg, err := parseConfig([]string{"-version=false"}, configEnv(map[string]string{"IMAGEPROXY_VERSION": "invalid"}))
	if err != nil || cfg.Version {
		t.Fatalf("explicit false did not override ENV: %v", err)
	}
}

func TestConfigRejectsInvalidValuesWithoutLeakingInputs(t *testing.T) {
	const secret = "secret-do-not-print"
	cases := []struct{ name, value string }{
		{"addr", "not-an-address"}, {"addr", "http://localhost:8080"}, {"addr", "localhost:65536"}, {"addr", "localhost:-1"}, {"addr", "[invalid]:80"}, {"adminAddr", "localhost"},
		{"baseURL", "/relative"}, {"baseURL", "file:///tmp/image"}, {"baseURL", "https://user:" + secret + "@example.com/"}, {"baseURL", "https://example.com/#"}, {"baseURL", "https://example.com:70000/"}, {"baseURL", "https://[invalid]:80/"}, {"baseURL", "https://[127.0.0.1]:80/"},
		{"logLevel", secret}, {"timeout", "0s"}, {"timeout", "-1s"}, {"timeout", secret}, {"timeout", fmt.Sprint(time.Duration(math.MaxInt64))}, {"shutdownTimeout", "0s"}, {"shutdownTimeout", "9999999999999999999h"},
		{"maxPixels", "0"}, {"maxPixels", "-1"}, {"maxPixels", fmt.Sprint(math.MaxInt64)}, {"maxResponseBytes", fmt.Sprint(math.MaxInt64)}, {"maxResponseBytes", "0"}, {"maxFrames", "-1"}, {"maxConcurrent", "0"}, {"maxConcurrent", "9999999999999999999999999"},
		{"followRedirects", secret}, {"scaleUp", ""}, {"verbose", secret}, {"allowHosts", "https://" + secret + "/"}, {"allowHosts", ", ,"}, {"allowHosts", " "}, {"contentTypes", "["}, {"contentTypes", ", ,"}, {"contentTypes", " "},
		{"cache", "redis:" + secret}, {"cache", "memory:-1"}, {"cache", "memory:0"}, {"cache", "memory:1:-1s"}, {"cache", "memory:1:" + secret}, {"cache", "memory:1:"}, {"cache", "memory:1:9999999999999999999h"}, {"cache", "memory:9223372036855"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			for _, cli := range []bool{false, true} {
				var args []string
				var env map[string]string
				if cli {
					args = []string{"-" + tc.name + "=" + tc.value}
				} else {
					env = map[string]string{configEnvName(tc.name): tc.value}
				}
				_, err := parseConfig(args, configEnv(env))
				if err == nil {
					t.Fatalf("invalid %s accepted (CLI=%v)", tc.name, cli)
				}
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("input secret leaked: %v", err)
				}
			}
		})
	}
	_, err := parseConfig([]string{"-signatureKey", secret, "-unknown=" + secret}, configEnv(nil))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unknown flag error leaked input: %v", err)
	}
}

func TestConfigCacheBoundsAndOperations(t *testing.T) {
	maxMB := int64(math.MaxInt64 / cacheMegabyte)
	cfg, err := parseConfig([]string{"-cache", fmt.Sprintf("memory:%d:%s", maxMB, time.Duration(math.MaxInt64))}, configEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache[0].maxBytes != maxMB*cacheMegabyte || cfg.Cache[0].maxAgeSeconds != int64(math.MaxInt64/int64(time.Second))+1 {
		t.Fatal("cache capacity or age overflowed")
	}
	for _, option := range []string{"memory", "memory:1:0s", "memory:1:1ms", "memory:1:1s memory:2:2s"} {
		t.Run(option, func(t *testing.T) {
			cfg, err := parseConfig([]string{"-cache", option}, configEnv(nil))
			if err != nil {
				t.Fatal(err)
			}
			cache := cfg.NewCache()
			cache.Set("key", []byte("value"))
			if value, ok := cache.Get("key"); !ok || string(value) != "value" {
				t.Fatal("cache did not round-trip data")
			}
			cache.Delete("key")
			if _, ok := cache.Get("key"); ok {
				t.Fatal("cache did not delete data")
			}
			if option == "memory" {
				memory, ok := cache.(*lrucache.LruCache)
				if !ok || memory.MaxSize != 100_000_000 {
					t.Fatal("memory shortcut did not use 100 MB")
				}
			}
		})
	}
	cfg, err = parseConfig([]string{"-cache", "memory:1"}, configEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	cache := cfg.NewCache()
	cache.Set("oversized", make([]byte, 1_000_001))
	if _, ok := cache.Get("oversized"); ok {
		t.Fatal("cache exceeded capacity")
	}
}

func TestConfigHelpAndPositionalArguments(t *testing.T) {
	_, err := parseConfig([]string{"-h"}, func(string) (string, bool) { t.Fatal("help consulted environment"); return "", false })
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error=%v", err)
	}
	var output bytes.Buffer
	printConfigUsage(&output)
	if !strings.Contains(output.String(), "IMAGEPROXY_ADMINADDR") || !strings.Contains(output.String(), "shutdownTimeout") {
		t.Fatal("help missing new options / environment names")
	}
	if _, err := parseConfig([]string{"unexpected"}, configEnv(nil)); err == nil {
		t.Fatal("positional argument accepted")
	}
}

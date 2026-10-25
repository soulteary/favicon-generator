package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfigValidatorResourceBoundaries(t *testing.T) {
	maxTimeout := int64(math.MaxInt64) - int64(5*time.Second)
	for _, tt := range []struct {
		name  string
		value string
		valid bool
	}{
		{"timeout", "1ns", true},
		{"timeout", fmt.Sprintf("%dns", maxTimeout), true},
		{"timeout", fmt.Sprintf("%dns", maxTimeout+1), false},
		{"timeout", "0s", false},
		{"shutdownTimeout", "1ns", true},
		{"shutdownTimeout", fmt.Sprintf("%dns", int64(math.MaxInt64)), true},
		{"shutdownTimeout", "-1ns", false},
		{"maxResponseBytes", "1", true},
		{"maxResponseBytes", fmt.Sprint(int64(math.MaxInt) - 1), true},
		{"maxResponseBytes", fmt.Sprint(int64(math.MaxInt)), false},
		{"maxPixels", "1", true},
		{"maxPixels", fmt.Sprint(int64(math.MaxInt) / 16), true},
		{"maxPixels", fmt.Sprint(int64(math.MaxInt)/16 + 1), false},
		{"maxFrames", "1", true},
		{"maxFrames", "0", false},
		{"maxConcurrent", "1", true},
		{"maxConcurrent", "0", false},
	} {
		for _, cli := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s=%s/CLI=%t", tt.name, tt.value, cli), func(t *testing.T) {
				var args []string
				var env map[string]string
				if cli {
					args = []string{"-" + tt.name + "=" + tt.value}
				} else {
					env = map[string]string{configEnvName(tt.name): tt.value}
				}
				_, err := parseConfig(args, configEnv(env))
				if (err == nil) != tt.valid {
					t.Fatalf("valid=%t, error=%v", tt.valid, err)
				}
			})
		}
	}
}

func TestConfigValidatorLogLevelsPreserveNormalization(t *testing.T) {
	for _, level := range []string{"trace", "debug", "info", "warn", "error", "fatal", "panic", "disabled"} {
		for _, cli := range []bool{false, true} {
			value := " \t" + strings.ToUpper(level) + "\n"
			var args []string
			var env map[string]string
			if cli {
				args = []string{"-logLevel=" + value}
			} else {
				env = map[string]string{"IMAGEPROXY_LOGLEVEL": value}
			}
			cfg, err := parseConfig(args, configEnv(env))
			if err != nil || cfg.LogLevel != level {
				t.Fatalf("level=%q CLI=%t: got %q, error=%v", level, cli, cfg.LogLevel, err)
			}
		}
	}
}

func TestConfigAllExplicitValuesSkipEnvironmentLookup(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-addr=:0", "-adminAddr=", "-logLevel= WARN ", "-followRedirects=false", "-scaleUp=false", "-verbose=false", "-version=false",
		"-baseURL=", "-allowHosts=", "-contentTypes=", "-signatureKey=", "-cache=",
		"-timeout=2s", "-shutdownTimeout=1ns", "-maxResponseBytes=64", "-maxPixels=8", "-maxFrames=1", "-maxConcurrent=1",
	}, func(key string) (string, bool) {
		t.Errorf("explicit CLI option consulted lower-priority environment %s", key)
		return "SECRET_INVALID_ENV", true
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FollowRedirects || cfg.ScaleUp || cfg.Verbose || cfg.Version || cfg.SignatureKey != "" || cfg.AdminAddr != "" || cfg.BaseURL != nil || len(cfg.AllowHosts) != 0 || len(cfg.ContentTypes) != 0 || len(cfg.Cache) != 0 || cfg.LogLevel != "warn" {
		t.Fatalf("explicit false or empty value was lost: %+v", cfg)
	}
}

func TestConfigValidatorErrorsRemainRedacted(t *testing.T) {
	for _, tt := range []struct{ name, value string }{
		{"logLevel", "SECRET_INVALID_LEVEL"},
		{"maxFrames", "-314159"},
		{"maxConcurrent", "-271828"},
		{"maxPixels", "-161803"},
		{"maxResponseBytes", "-141421"},
		{"cache", "memory:-173205"},
	} {
		for _, cli := range []bool{false, true} {
			var args []string
			var env map[string]string
			if cli {
				args = []string{"-" + tt.name + "=" + tt.value}
			} else {
				env = map[string]string{configEnvName(tt.name): tt.value}
			}
			_, err := parseConfig(args, configEnv(env))
			if err == nil || strings.Contains(err.Error(), tt.value) || strings.Contains(err.Error(), "got ") || strings.Contains(err.Error(), "allowed values:") {
				t.Fatalf("validator input leaked for %s CLI=%t: %v", tt.name, cli, err)
			}
		}
	}
}

func TestConfigValidatorCacheWinningSourceAndAgeBounds(t *testing.T) {
	cfg, err := parseConfig([]string{"-cache=memory:1:0s", "-cache=memory:2:1ns"}, func(key string) (string, bool) {
		if key == "IMAGEPROXY_CACHE" {
			t.Error("CLI cache tiers consulted overridden environment")
			return "memory:SECRET", true
		}
		return "", false
	})
	if err != nil || !reflect.DeepEqual(cfg.Cache, []CacheConfig{{maxBytes: 1_000_000}, {maxBytes: 2_000_000, maxAgeSeconds: 1}}) {
		t.Fatalf("CLI tiers or zero/subsecond ages changed: %v %v", cfg.Cache, err)
	}
	if _, err := parseConfig(nil, configEnv(map[string]string{"IMAGEPROXY_MAXFRAMES": "0"})); err == nil {
		t.Fatal("invalid winning environment fell back to the default")
	}
}

func TestConfigShortcutsRemainIndependentOfValidatorEnvironment(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"-version"}} {
		cfg, err := parseConfig(args, func(string) (string, bool) {
			t.Fatal("help/version consulted unrelated environment")
			return "", false
		})
		if args[0] == "-h" && !errors.Is(err, flag.ErrHelp) || args[0] == "-version" && (err != nil || !cfg.Version) {
			t.Fatalf("shortcut %v: error=%v, version=%t", args, err, cfg.Version)
		}
	}
	if _, err := parseConfig([]string{"-version", "-maxPixels=0"}, configEnv(nil)); err == nil {
		t.Fatal("version bypassed an explicitly invalid CLI resource limit")
	}
}

package main

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactRedisConfigurationAndFactory(t *testing.T) {
	for _, mode := range []string{"off", "memory", "redis", "tiered"} {
		cfg, err := parseConfig([]string{"-artifactCache=" + mode}, configEnv(nil))
		if err != nil {
			t.Fatal(err)
		}
		service, err := cfg.newArtifactService(nil)
		if err != nil || service.backend != mode {
			t.Fatalf("factory for %s: %v", mode, err)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"-artifactRedisAddr=SECRET_INVALID_ADDRESS"}, {"-artifactRedisAddr=localhost:0"},
		{"-artifactRedisAddr=localhost:+6379"}, {"-artifactRedisAddr=host..invalid:6379"},
		{"-artifactRedisDB=-1"}, {"-artifactRedisTimeout=0"}, {"-artifactRedisTimeout=5s1ns"},
		{"-artifactRedisPrefix="}, {"-artifactRedisPrefix=SECRET/INVALID"},
		{"-artifactRedisCAFile=SECRET_CA"}, {"-artifactRedisServerName=localhost"},
		{"-artifactRedisTLS", "-artifactRedisServerName=SECRET/INVALID"},
		{"-artifactCache=tiered", "-artifactCacheBytes=2048"},
	} {
		if _, err := parseConfig(args, configEnv(nil)); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("invalid Redis option accepted or exposed: %v, %v", args, err)
		}
	}
	cfg, err := parseConfig([]string{"-artifactCache=redis", "-artifactRedisAddr=127.0.0.1:6379", "-artifactRedisPassword=CLI_SECRET", "-artifactRedisTimeout=5s"}, configEnv(map[string]string{
		"IMAGEPROXY_ARTIFACTREDISADDR": "SECRET_INVALID_ADDRESS", "IMAGEPROXY_ARTIFACTREDISPASSWORD": "ENV_SECRET",
	}))
	if err != nil || cfg.ArtifactRedisPassword != "CLI_SECRET" || cfg.ArtifactRedisTimeout.String() != "5s" {
		t.Fatalf("Redis CLI precedence: %v", err)
	}
	cfg, err = parseConfig(nil, configEnv(map[string]string{"IMAGEPROXY_ARTIFACTCACHE": "redis", "IMAGEPROXY_ARTIFACTREDISUSERNAME": "named-user", "IMAGEPROXY_ARTIFACTREDISPASSWORD": "ENV_SECRET"}))
	if err != nil || cfg.ArtifactRedisUsername != "named-user" || cfg.ArtifactRedisPassword != "ENV_SECRET" {
		t.Fatalf("Redis environment configuration: %v", err)
	}
}

func TestArtifactRedisCAFileIsBoundedAndRedacted(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "SECRET_CA.pem")
	cfg, err := parseConfig([]string{"-artifactCache=redis", "-artifactRedisTLS", "-artifactRedisCAFile=" + path}, configEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, prepare := range []func(){
		func() {},
		func() {
			if err := os.WriteFile(path, []byte("SECRET_INVALID_PEM"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		func() {
			if err := os.Truncate(path, maxArtifactRedisCABytes+1); err != nil {
				t.Fatal(err)
			}
		},
	} {
		prepare()
		if store, err := cfg.newRedisArtifactBackend(); store != nil || err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("bad CA file admitted or leaked: %v", err)
		}
	}
	cfg.ArtifactRedisCAFile = directory
	if _, err := cfg.newRedisArtifactBackend(); err == nil {
		t.Fatal("directory accepted as CA file")
	}
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Certificate().Raw})
	fixture.Close()
	if err := os.WriteFile(path, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.ArtifactRedisCAFile = path
	backend, err := cfg.newRedisArtifactBackend()
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil && !errors.Is(err, errRedisArtifactClosed) {
		t.Fatal(err)
	}
}

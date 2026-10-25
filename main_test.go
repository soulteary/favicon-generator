package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunVersionDoesNotReadServerEnvironment(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-version"}, func(string) (string, bool) {
		t.Fatal("version command read unrelated server environment")
		return "", false
	}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Version:") || !strings.Contains(stdout.String(), "Go version:") || stderr.Len() != 0 {
		t.Fatalf("unexpected version output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunHelpDoesNotStartServer(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-help"}, func(string) (string, bool) { return "invalid", true }, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "adminAddr") || stderr.Len() != 0 {
		t.Fatalf("unexpected help output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunInvalidConfigDoesNotStartServer(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-maxPixels=0"}, func(string) (string, bool) { return "", false }, &stdout, &stderr)
	if err == nil || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("invalid configuration: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func TestRunCanceledBeforeStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	err := run(ctx, nil, func(string) (string, bool) { return "", false }, &stdout, &stderr)
	if !errors.Is(err, context.Canceled) || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("canceled startup: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

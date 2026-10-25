// Copyright 2013 The imageproxy authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	version "github.com/soulteary/version-kit/v4"
	"github.com/willnorris/imageproxy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) error {
	cfg, err := parseConfig(args, lookupEnv)
	if errors.Is(err, flag.ErrHelp) {
		printConfigUsage(stdout)
		return nil
	}
	if err != nil {
		return err
	}
	if cfg.Version {
		_, err := io.WriteString(stdout, version.Default().Full())
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := loadUITranslations(); err != nil {
		return err
	}
	level := cfg.LogLevel
	if cfg.Verbose && level == "info" {
		level = "debug"
	}
	obs, err := newObservability(stderr, level)
	if err != nil {
		return err
	}
	p := imageproxy.NewProxy(nil, cfg.NewCache())
	p.DefaultBaseURL = cfg.BaseURL
	p.FollowRedirects = cfg.FollowRedirects
	p.ScaleUp = cfg.ScaleUp
	p.Timeout = cfg.Timeout
	p.AllowHosts = cfg.AllowHosts
	p.ContentTypes = cfg.ContentTypes
	p.MaxResponseBytes = cfg.MaxResponseBytes
	p.MaxPixels = cfg.MaxPixels
	p.MaxFrames = cfg.MaxFrames
	p.MaxConcurrentRequests = cfg.MaxConcurrent
	p.Verbose = cfg.Verbose
	p.Logger = obs.ProxyLogger
	if cfg.SignatureKey != "" {
		p.SignatureKeys = [][]byte{[]byte(cfg.SignatureKey)}
	}
	defer p.Client.CloseIdleConnections()
	return serve(ctx, cfg, obs, &iconHandler{proxy: p})
}

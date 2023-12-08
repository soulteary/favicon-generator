// Copyright 2013 The imageproxy authors.
// SPDX-License-Identifier: Apache-2.0

// imageproxy starts an HTTP server that proxies requests for remote images.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/lrucache"
	"github.com/die-net/lrucache/twotier"
	"github.com/willnorris/imageproxy"
	"github.com/willnorris/imageproxy/third_party/envy"
)

const defaultMemorySize = 100

var addr = flag.String("addr", "localhost:8080", "TCP address to listen on")
var followRedirects = flag.Bool("followRedirects", true, "follow redirects")
var baseURL = flag.String("baseURL", "", "default base URL for relative remote URLs")
var cache tieredCache
var scaleUp = flag.Bool("scaleUp", false, "allow images to scale beyond their original dimensions")
var timeout = flag.Duration("timeout", 0, "time limit for requests served by this proxy")
var verbose = flag.Bool("verbose", false, "print verbose logging messages")
var _ = flag.Bool("version", false, "Deprecated: this flag does nothing")
var contentTypes = flag.String("contentTypes", "image/*", "comma separated list of allowed content types")

func init() {
	flag.Var(&cache, "cache", "location to cache images (see https://github.com/willnorris/imageproxy#cache)")
}

func main() {
	envy.Parse("IMAGEPROXY")
	flag.Parse()

	p := imageproxy.NewProxy(nil, cache.Cache)

	if *contentTypes != "" {
		p.ContentTypes = strings.Split(*contentTypes, ",")
	}
	if *baseURL != "" {
		var err error
		p.DefaultBaseURL, err = url.Parse(*baseURL)
		if err != nil {
			log.Fatalf("error parsing baseURL: %v", err)
		}
	}

	p.FollowRedirects = *followRedirects
	p.Timeout = *timeout
	p.ScaleUp = *scaleUp
	p.Verbose = *verbose

	server := &http.Server{
		Addr:    *addr,
		Handler: p,

		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	fmt.Printf("imageproxy listening on %s\n", server.Addr)

	log.Fatal(server.ListenAndServe())
}

// tieredCache allows specifying multiple caches via flags, which will create
// tiered caches using the twotier package.
type tieredCache struct {
	imageproxy.Cache
}

func (tc *tieredCache) String() string {
	return fmt.Sprint(*tc)
}

func (tc *tieredCache) Set(value string) error {
	for _, v := range strings.Fields(value) {
		c, err := parseCache(v)
		if err != nil {
			return err
		}

		if tc.Cache == nil {
			tc.Cache = c
		} else {
			tc.Cache = twotier.New(tc.Cache, c)
		}
	}
	return nil
}

// parseCache parses c returns the specified Cache implementation.
func parseCache(c string) (imageproxy.Cache, error) {
	if c == "" {
		return nil, nil
	}

	if c == "memory" {
		c = fmt.Sprintf("memory:%d", defaultMemorySize)
	}

	u, err := url.Parse(c)
	if err != nil {
		return nil, fmt.Errorf("error parsing cache flag: %w", err)
	}
	return lruCache(u.Opaque)
}

// lruCache creates an LRU Cache with the specified options of the form
// "maxSize:maxAge".  maxSize is specified in megabytes, maxAge is a duration.
func lruCache(options string) (*lrucache.LruCache, error) {
	parts := strings.SplitN(options, ":", 2)
	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, err
	}

	var age time.Duration
	if len(parts) > 1 {
		age, err = time.ParseDuration(parts[1])
		if err != nil {
			return nil, err
		}
	}

	return lrucache.New(size*1e6, int64(age.Seconds())), nil
}

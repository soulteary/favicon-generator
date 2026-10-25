// Copyright 2026 The imageproxy authors.
// SPDX-License-Identifier: Apache-2.0

package imageproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var errUnsafeURL = errors.New("remote URL is not allowed")

// validateRemoteURL performs checks which do not require a DNS lookup. The
// transport separately checks every resolved address immediately before dialing.
func validateRemoteURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil {
		return errUnsafeURL
	}
	return validateRemoteHost(u.Hostname())
}

func validateRemoteHost(host string) error {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		host == "localhost.localdomain" || strings.HasSuffix(host, ".localhost.localdomain") ||
		strings.ContainsAny(host, "%\\/\x00\r\n\t ") {
		return errUnsafeURL
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return validateRemoteIP(addr)
	}
	// A colon is only valid inside an IP literal; an unparseable IPv6 address
	// must never be passed to a resolver as a hostname.
	if strings.Contains(host, ":") {
		return errUnsafeURL
	}
	return nil
}

// Special-use ranges are blocked conservatively, including ranges with a few
// globally reachable exceptions. IPv6 is additionally restricted to the global
// unicast allocation, excluding translation and transition address spaces.
var blockedRemotePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3ffe::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

var globalUnicastIPv6 = netip.MustParsePrefix("2000::/3")

func validateRemoteIP(addr netip.Addr) error {
	if !addr.IsValid() || addr.Zone() != "" || addr.Is4In6() || !addr.IsGlobalUnicast() ||
		(addr.Is6() && !globalUnicastIPv6.Contains(addr)) {
		return errUnsafeURL
	}
	for _, prefix := range blockedRemotePrefixes {
		if prefix.Contains(addr) {
			return errUnsafeURL
		}
	}
	return nil
}

type remoteIPResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// safeRemoteDialer resolves once, checks all answers, and dials a checked IP
// literal. Passing the original hostname to net.Dialer after validation would
// introduce a second DNS lookup and permit DNS rebinding.
type safeRemoteDialer struct {
	resolver remoteIPResolver
	dial     func(context.Context, string, string) (net.Conn, error)
}

func (d *safeRemoteDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errUnsafeURL
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid remote address", errUnsafeURL)
	}
	if err := validateRemoteHost(host); err != nil {
		return nil, err
	}
	var addresses []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{addr}
	} else {
		lookupNetwork := "ip"
		if network == "tcp4" {
			lookupNetwork = "ip4"
		} else if network == "tcp6" {
			lookupNetwork = "ip6"
		}
		addresses, err = d.resolver.LookupNetIP(ctx, lookupNetwork, host)
		if err != nil {
			return nil, fmt.Errorf("resolving remote host: %w", err)
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("remote host has no addresses")
	}
	for _, addr := range addresses {
		if err := validateRemoteIP(addr); err != nil {
			return nil, err
		}
	}
	for _, addr := range addresses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var conn net.Conn
		conn, err = d.dial(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, err
}

func newSafeTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	safe := &safeRemoteDialer{resolver: net.DefaultResolver, dial: dialer.DialContext}
	return &http.Transport{
		// Environment proxies can resolve destinations outside this dialer's
		// checks. All destination connections must go through DialContext.
		Proxy:                  nil,
		DialContext:            safe.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           100,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		ExpectContinueTimeout:  time.Second,
	}
}

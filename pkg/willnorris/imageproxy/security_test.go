// Copyright 2026 The imageproxy authors.
// SPDX-License-Identifier: Apache-2.0

package imageproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"testing"
)

func TestValidateRemoteURL(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/image.png", "https://EXAMPLE.com:8443/image.png",
		"https://8.8.8.8/image.png", "https://[2606:4700:4700::1111]/image.png",
	} {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateRemoteURL(u); err != nil {
				t.Fatalf("validateRemoteURL(%q): %v", raw, err)
			}
		})
	}
	for _, raw := range []string{
		"/image.png", "file:///etc/passwd", "ftp://example.com/image.png", "http:example.com",
		"http://user:password@example.com/", "http://user@example.com/", "http:///image.png",
		"http://localhost/", "http://LOCALHOST./", "http://image.localhost/",
		"http://localhost.localdomain/", "http://image.localhost.localdomain/",
		"http://127.0.0.1/", "http://10.0.0.1/", "http://169.254.169.254/",
		"http://127.0.0.1./", "http://[::1]/", "http://[::ffff:8.8.8.8]/",
		"http://[fe80::1%25en0]/", "http://[2001:db8::1]/",
	} {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateRemoteURL(u); !errors.Is(err, errUnsafeURL) {
				t.Fatalf("validateRemoteURL(%q) = %v, want errUnsafeURL", raw, err)
			}
		})
	}
	if err := validateRemoteURL(nil); !errors.Is(err, errUnsafeURL) {
		t.Fatalf("validateRemoteURL(nil) = %v, want errUnsafeURL", err)
	}
}

func TestValidateRemoteIP(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "0.1.2.3", "10.0.0.1", "100.64.0.1", "100.127.255.254", "127.0.0.1",
		"169.254.169.254", "172.16.0.1", "172.31.255.254", "192.0.0.9", "192.0.2.1",
		"192.88.99.1", "192.168.1.1", "198.18.0.1", "198.19.255.254", "198.51.100.1",
		"203.0.113.1", "224.0.0.1", "239.255.255.254", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::ffff:127.0.0.1", "::ffff:8.8.8.8", "64:ff9b::a00:1", "64:ff9b:1::1",
		"100::1", "2001::1", "2001:2::1", "2001:20::1", "2001:db8::1", "2002:a00:1::1",
		"3ffe::1", "3fff::1", "3fff:fff::1", "fc00::1", "fd00::1", "fe80::1", "fe80::1%en0", "ff02::1",
	} {
		t.Run(raw, func(t *testing.T) {
			if err := validateRemoteIP(netip.MustParseAddr(raw)); !errors.Is(err, errUnsafeURL) {
				t.Fatalf("validateRemoteIP(%q) = %v, want errUnsafeURL", raw, err)
			}
		})
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "100.63.255.254", "100.128.0.1", "172.15.255.254", "172.32.0.1", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		t.Run(raw, func(t *testing.T) {
			if err := validateRemoteIP(netip.MustParseAddr(raw)); err != nil {
				t.Fatalf("validateRemoteIP(%q): %v", raw, err)
			}
		})
	}
	if err := validateRemoteIP(netip.Addr{}); !errors.Is(err, errUnsafeURL) {
		t.Fatalf("validateRemoteIP(invalid) = %v, want errUnsafeURL", err)
	}
}

type remoteResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f remoteResolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func TestSafeRemoteDialerRejectsDNSAnswers(t *testing.T) {
	for _, addresses := range [][]netip.Addr{
		{netip.MustParseAddr("127.0.0.1")},
		{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")},
		{netip.MustParseAddr("::ffff:8.8.8.8")},
		{netip.MustParseAddr("64:ff9b::a00:1")},
		{netip.Addr{}},
	} {
		t.Run(addresses[0].String(), func(t *testing.T) {
			d := safeRemoteDialer{
				resolver: remoteResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return addresses, nil }),
				dial: func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("unsafe address reached the dialer")
					return nil, nil
				},
			}
			if _, err := d.DialContext(context.Background(), "tcp", "example.com:443"); !errors.Is(err, errUnsafeURL) {
				t.Fatalf("DialContext = %v, want errUnsafeURL", err)
			}
		})
	}
}

func TestSafeRemoteDialerPinsResolvedAddresses(t *testing.T) {
	var resolved, dialed int
	d := safeRemoteDialer{
		resolver: remoteResolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
			resolved++
			if network != "ip" || host != "example.com" {
				t.Fatalf("unexpected lookup: %s %s", network, host)
			}
			// Any second resolution would simulate a rebinding to loopback.
			if resolved > 1 {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("2606:4700:4700::1111")}, nil
		}),
		dial: func(_ context.Context, network, address string) (net.Conn, error) {
			dialed++
			if network != "tcp" {
				t.Fatalf("unexpected dial network %q", network)
			}
			if dialed == 1 {
				if address != "8.8.8.8:443" {
					t.Fatalf("first dial = %q", address)
				}
				return nil, errors.New("first address failed")
			}
			if address != "[2606:4700:4700::1111]:443" {
				t.Fatalf("second dial = %q", address)
			}
			return nil, nil
		},
	}
	if _, err := d.DialContext(context.Background(), "tcp", "example.com:443"); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 || dialed != 2 {
		t.Fatalf("resolved %d times and dialed %d times, want 1 and 2", resolved, dialed)
	}
}

func TestSafeRemoteDialerIPLiteralAndContext(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "request")
	var addresses []string
	d := safeRemoteDialer{
		resolver: remoteResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			t.Fatal("IP literal caused a DNS lookup")
			return nil, nil
		}),
		dial: func(got context.Context, network, address string) (net.Conn, error) {
			if got.Value(contextKey{}) != "request" {
				t.Fatal("request context was not passed to the dialer")
			}
			addresses = append(addresses, address)
			return nil, nil
		},
	}
	if _, err := d.DialContext(ctx, "tcp", "8.8.8.8:443"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(addresses, []string{"8.8.8.8:443"}) {
		t.Fatalf("dialed %v", addresses)
	}
	for _, address := range []string{"127.0.0.1:443", "[::1]:443", "localhost:443", "[fe80::1%en0]:443", "missing-port"} {
		if _, err := d.DialContext(ctx, "tcp", address); !errors.Is(err, errUnsafeURL) {
			t.Fatalf("DialContext(%q) = %v, want errUnsafeURL", address, err)
		}
	}
	if _, err := d.DialContext(ctx, "udp", "8.8.8.8:443"); !errors.Is(err, errUnsafeURL) {
		t.Fatalf("DialContext(udp) = %v, want errUnsafeURL", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := d.DialContext(canceled, "tcp", "8.8.8.8:443"); !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext(canceled) = %v, want context.Canceled", err)
	}
	if len(addresses) != 1 {
		t.Fatalf("invalid or canceled requests reached the dialer: %v", addresses)
	}
}

func TestSafeRemoteDialerResolverContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := safeRemoteDialer{
		resolver: remoteResolverFunc(func(got context.Context, _, _ string) ([]netip.Addr, error) {
			cancel()
			<-got.Done()
			return nil, got.Err()
		}),
		dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("canceled lookup reached the dialer")
			return nil, nil
		},
	}
	if _, err := d.DialContext(ctx, "tcp", "example.com:443"); !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext = %v, want context.Canceled", err)
	}
}

func TestNewSafeTransport(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:8080")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8080")
	transport := newSafeTransport()
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil || transport.DialContext == nil || transport.DialTLS != nil || transport.DialTLSContext != nil {
		t.Fatal("transport permits destination connections outside its safe dialer")
	}
	if transport.MaxResponseHeaderBytes != 64<<10 {
		t.Fatalf("upstream header byte limit = %d, want 65536", transport.MaxResponseHeaderBytes)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:80"); !errors.Is(err, errUnsafeURL) {
		t.Fatalf("transport dial = %v, want errUnsafeURL", err)
	}
}

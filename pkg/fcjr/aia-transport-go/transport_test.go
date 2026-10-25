//go:build !windows
// +build !windows

package aia_test

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcjr/aia-transport-go"
)

func TestTransport(t *testing.T) {
	for _, tc := range []struct {
		name          string
		completeChain bool
		untrusted     bool
		expired       bool
		wrongHost     bool
		invalidIssuer bool
		wantError     bool
		wantFetches   int32
	}{
		{name: "complete chain", completeChain: true},
		{name: "incomplete chain", wantFetches: 1},
		{name: "untrusted root", untrusted: true, wantError: true, wantFetches: 1},
		{name: "expired leaf", expired: true, wantError: true},
		{name: "wrong host", wrongHost: true, wantError: true},
		{name: "invalid issuer", invalidIssuer: true, wantError: true, wantFetches: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var issuer []byte
			var fetches atomic.Int32
			issuerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fetches.Add(1)
				_, _ = w.Write(issuer)
			}))
			t.Cleanup(issuerServer.Close)

			root, _, rootPEM, rootKey, err := genRootCA()
			if err != nil {
				t.Fatal(err)
			}
			intermediate, der, key, err := genIntermediate(root, rootKey, "Local intermediate", "")
			if err != nil {
				t.Fatal(err)
			}
			issuer = der
			if tc.invalidIssuer {
				issuer = []byte("not a certificate")
			}
			ip := net.IPv4(127, 0, 0, 1)
			if tc.wrongHost {
				ip = net.IPv4(192, 0, 2, 1)
			}
			leaf, leafKey, err := genLeafCertificate(intermediate, key, issuerServer.URL, ip)
			if err != nil {
				t.Fatal(err)
			}
			if tc.expired {
				leaf.NotBefore = time.Now().Add(-2 * time.Hour)
				leaf.NotAfter = time.Now().Add(-time.Hour)
				der, err := x509.CreateCertificate(rand.Reader, leaf, intermediate, &leafKey.PublicKey, key)
				if err != nil {
					t.Fatal(err)
				}
				leaf, err = x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
			}
			var chain []*x509.Certificate
			if tc.completeChain {
				chain = append(chain, intermediate)
			}
			cert, err := genTLSChain(leaf, leafKey, chain...)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "local TLS response")
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
			server.StartTLS()
			t.Cleanup(server.Close)

			tr, err := aia.NewTransport()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(tr.CloseIdleConnections)
			// Replace the shared pool in place: the transport also captures this
			// pointer in DialTLS. Avoid operating-system trust/AIA lookups.
			*tr.TLSClientConfig.RootCAs = *x509.NewCertPool()
			if !tc.untrusted && !tr.TLSClientConfig.RootCAs.AppendCertsFromPEM(rootPEM.Bytes()) {
				t.Fatal("failed to add local root")
			}
			client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
			res, err := client.Get(server.URL)
			if (err != nil) != tc.wantError {
				t.Fatalf("request error = %v, want error %v", err, tc.wantError)
			}
			if err == nil {
				defer res.Body.Close()
				body, err := io.ReadAll(res.Body)
				if err != nil || string(body) != "local TLS response" {
					t.Fatalf("response = %q, error = %v", body, err)
				}
			}
			if got := fetches.Load(); got != tc.wantFetches {
				t.Errorf("issuer fetches = %d, want %d", got, tc.wantFetches)
			}
		})
	}
}

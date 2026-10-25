package aia_test

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/fcjr/aia-transport-go"
)

func ExampleNewTransport() {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr, err := aia.NewTransport()
	if err != nil {
		log.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	// Trust the certificate used by this local example server.
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool()}
	}
	*tr.TLSClientConfig.RootCAs = *x509.NewCertPool()
	tr.TLSClientConfig.RootCAs.AddCert(server.Certificate())
	client := http.Client{Transport: tr}
	res, err := client.Get(server.URL)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()
	fmt.Println(res.Status)

	// Output:
	// 200 OK
}

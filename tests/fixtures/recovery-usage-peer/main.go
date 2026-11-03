// SPDX-License-Identifier: Elastic-2.0
// Opt-in disposable HTTPS peer. Never included in the installed application.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const response = `{"data":{"id":"gen-installed-usage","model":"example/usage-model","total_cost":0.0000001,"is_byok":true}}`

func main() {
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, "owned usage peer failed")
		os.Exit(1)
	}
}

func serve() error {
	invalid := fmt.Errorf("invalid owned peer")
	if os.Getenv("RUNVERGE_OWNED_RECOVERY_FIXTURE") != "1" || len(os.Args) != 1 {
		return invalid
	}
	// This directory is an explicitly mounted test-only input/output, not runtime
	// state. The synthetic key never enters response bytes, logs or process args.
	dir := "/peer"
	key, err := os.ReadFile(filepath.Join(dir, "synthetic-key"))
	if err != nil || len(key) != 64 || strings.Trim(string(key), "0123456789abcdef") != "" {
		return invalid
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return invalid
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(29), DNSNames: []string{"openrouter.ai"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &private.PublicKey, private)
	if err != nil {
		return invalid
	}
	write := func(name string, raw []byte) error {
		f, e := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		if _, e = f.Write(raw); e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		d, e := os.Open(dir)
		if e != nil {
			return e
		}
		e = d.Sync()
		closeErr = d.Close()
		if e != nil {
			return e
		}
		return closeErr
	}
	if err = write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return invalid
	}
	listener, err := net.Listen("tcp", ":8443")
	if err != nil {
		return invalid
	}
	defer listener.Close()
	var mu sync.Mutex
	count := 0
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		count++
		// Persist each request separately before replying. The checker rejects
		// extra files, including an unauthorized or repeated request.
		raw, _ := json.Marshal(map[string]int{"request_count": count})
		if write(fmt.Sprintf("request-%d.json", count), raw) != nil {
			http.Error(w, "counter unavailable", 500)
			return
		}
		if count != 1 || r.Method != "GET" || r.URL.Path != "/api/v1/generation" || r.URL.RawQuery != "id=gen-installed-usage" || r.Header.Get("Authorization") != "Bearer "+string(key) || r.Host != "openrouter.ai" {
			http.Error(w, "unexpected synthetic request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	})}
	tlsListener := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}})
	if err = write("ready", []byte("owned-usage-peer-v1\n")); err != nil {
		return invalid
	}
	return server.Serve(tlsListener)
}

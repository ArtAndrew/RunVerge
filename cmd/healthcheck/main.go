// SPDX-License-Identifier: Elastic-2.0
// Container readiness probe. It only reads this container's fixed API endpoint.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

func ready(client *http.Client, endpoint string) bool {
	response, err := client.Get(endpoint)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		return false
	}
	var result struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	return json.Unmarshal(body, &result) == nil && result.Data.Status == "ready"
}

func main() {
	endpoint := "http://127.0.0.1:8080/readyz"
	transport := &http.Transport{Proxy: nil}
	if len(os.Args) == 2 && os.Args[1] == "--organization" {
		endpoint = "https://127.0.0.1:8443/readyz"
		transport.TLSClientConfig = organizationTLS()
		if transport.TLSClientConfig == nil {
			os.Exit(1)
		}
	} else if len(os.Args) != 1 {
		os.Exit(1)
	}
	client := &http.Client{
		Timeout:       2 * time.Second,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if !ready(client, endpoint) {
		os.Exit(1)
	}
}

// Verify the configured leaf and its public name while dialing only loopback.
// The mounted leaf is trusted for this local readiness request alone.
func organizationTLS() *tls.Config {
	const directory = "/run/runverge/identity/"
	file, err := os.Open(directory + "oidc.json")
	if err != nil {
		return nil
	}
	defer file.Close()
	var config struct {
		PublicOrigin string `json:"public_origin"`
	}
	if json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&config) != nil {
		return nil
	}
	origin, err := url.Parse(config.PublicOrigin)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" {
		return nil
	}
	cert, err := os.Open(directory + "tls.crt")
	if err != nil {
		return nil
	}
	defer cert.Close()
	data, err := io.ReadAll(io.LimitReader(cert, 64<<10))
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &tls.Config{RootCAs: roots, ServerName: origin.Hostname(), MinVersion: tls.VersionTLS12}
}

// SPDX-License-Identifier: Elastic-2.0
// Test-only standard MCP server: reads one operator-selected document into
// memory before serving. No RunVerge execution/action envelope or receipt code.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "MCP read test server failed")
		os.Exit(1)
	}
}
func run() error {
	ready := flag.String("ready", "", "new private readiness file")
	document := flag.String("document", "", "operator-owned UTF-8 example document")
	flag.Parse()
	if *ready == "" || *document == "" {
		return fmt.Errorf("missing arguments")
	}
	source, err := os.Open(*document)
	if err != nil {
		return err
	}
	content, err := io.ReadAll(io.LimitReader(source, 16385))
	closeErr := source.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil || len(content) == 0 || len(content) > 16384 || !utf8.Valid(content) {
		return fmt.Errorf("invalid document")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Owned MCP read test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return err
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		return err
	}
	defer listener.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "owned-standard-document-reader", Version: "1"}, nil)
	var calls atomic.Int32
	mcp.AddTool(server, &mcp.Tool{Name: "read_document", Description: "Read the single operator-selected example document"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		n := calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(content)}, &mcp.TextContent{Text: fmt.Sprintf("Read invocation %d", n)}}}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	f, err := os.OpenFile(*ready, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(map[string]string{"endpoint": "https://" + listener.Addr().String(), "trusted_ca_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))})
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return (&http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 32768}).Serve(listener)
}

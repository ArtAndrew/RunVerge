// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"agentplatform.local/platform/internal/domain"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const MaxResponseBytes = 4 << 20

// ClientConfig is installed by a trusted operator, never constructed from a
// model's URL, command, tool annotations, or discovery metadata. Child processes
// are trusted code; this adapter is not an operating-system sandbox.
type ClientConfig struct {
	Endpoint            string
	Command             string
	Args                []string
	Environment         []string
	Directory           string
	AccessToken         string
	OrganizationID      string
	AllowPrivateNetwork bool
	TrustedRoots        *x509.CertPool
	Timeout             time.Duration
	MaxConcurrent       int
	// Optional single-occurrence host gateway permit. The same absolute deadline
	// bounds every POST's first application write, including after discovery/TLS.
	// This is not authorization by itself; AuthorizeCall remains mandatory.
	DispatchDeadline       time.Time
	DispatchIdempotencyKey string
	// AuthorizeCall must enter the host's common gateway. Nil denies all tool
	// calls. A readOnlyHint from the peer is never an authorization decision.
	AuthorizeCall func(context.Context, string, json.RawMessage) error
	// Optional trusted-host action policy applied to the original matching
	// tools/call JSON-RPC result before typed decoding. Requires the existing
	// finite HTTPS action permit. It cannot authorize a tool or a continuation.
	ValidateToolResult func(json.RawMessage) error
}
type Client struct {
	slots     chan struct{}
	session   *mcp.ClientSession
	authorize func(context.Context, string, json.RawMessage) error
	timeout   time.Duration
	closeHTTP func()
}

var errClient = errors.New("MCP peer request failed")

func Connect(ctx context.Context, c ClientConfig) (*Client, error) {
	if (c.Endpoint == "") == (c.Command == "") {
		return nil, errors.New("register exactly one MCP transport")
	}
	if c.ValidateToolResult != nil && (c.Endpoint == "" || c.DispatchDeadline.IsZero()) {
		return nil, errors.New("raw tool result validation requires a registered HTTPS action permit")
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 4
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 16 {
		return nil, errors.New("invalid MCP client concurrency")
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.Timeout < time.Millisecond || c.Timeout > 30*time.Second {
		return nil, errors.New("invalid MCP timeout")
	}
	sdk := mcp.NewClient(&mcp.Implementation{Name: "agent-platform", Version: domain.ContractVersion}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}})
	sdk.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "server/discover", "tools/list", "tools/call", "notifications/cancelled":
				return next(ctx, method, req)
			default:
				return nil, errors.New("MCP legacy fallback or unsupported method is disabled")
			}
		}
	})
	var transport mcp.Transport
	closeHTTP := func() {}
	if c.Endpoint != "" {
		client, closeIdle, err := registeredHTTPClient(c)
		if err != nil {
			return nil, err
		}
		closeHTTP = closeIdle
		transport = &mcp.StreamableClientTransport{Endpoint: c.Endpoint, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: MaxResponseBytes}
	} else {
		if !c.DispatchDeadline.IsZero() || c.DispatchIdempotencyKey != "" {
			return nil, errors.New("action permits require registered HTTPS")
		}
		if !filepath.IsAbs(c.Command) || c.AccessToken != "" || c.OrganizationID != "" {
			return nil, errors.New("invalid registered MCP process")
		}
		info, err := os.Stat(c.Command)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			return nil, errors.New("registered MCP executable is unavailable")
		}
		if c.Directory != "" && !filepath.IsAbs(c.Directory) {
			return nil, errors.New("MCP directory must be absolute")
		}
		for _, item := range c.Environment {
			key, _, ok := strings.Cut(item, "=")
			if !ok || key == "" || strings.ContainsAny(key, "\x00\r\n") || strings.ContainsRune(item, '\x00') {
				return nil, errors.New("invalid MCP process environment")
			}
		}
		cmd := exec.Command(c.Command, append([]string(nil), c.Args...)...)
		// Empty means an empty environment, not os.Environ(): platform credentials
		// and deployment secrets are never inherited by a registered subprocess.
		cmd.Env = append([]string{}, c.Environment...)
		cmd.Dir = c.Directory
		cmd.Stderr = io.Discard
		transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: 500 * time.Millisecond}
	}
	connectCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	session, err := sdk.Connect(connectCtx, transport, &mcp.ClientSessionOptions{ProtocolVersion: ProtocolVersion})
	if err != nil {
		closeHTTP()
		return nil, errClient
	}
	if session.InitializeResult() == nil || session.InitializeResult().ProtocolVersion != ProtocolVersion {
		session.Close()
		closeHTTP()
		return nil, errClient
	}
	return &Client{slots: make(chan struct{}, c.MaxConcurrent), session: session, authorize: c.AuthorizeCall, timeout: c.Timeout, closeHTTP: closeHTTP}, nil
}
func (c *Client) Close() error { err := c.session.Close(); c.closeHTTP(); return err }
func (c *Client) ListTools(ctx context.Context, cursor string) (*mcp.ListToolsResult, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return nil, errors.New("MCP client concurrency limit reached")
	}
	if len(cursor) > 4096 {
		return nil, errClient
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	result, err := c.session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
	if err != nil {
		return nil, errClient
	}
	if !boundedResult(result) {
		return nil, errClient
	}
	return result, nil
}
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return nil, errors.New("MCP client concurrency limit reached")
	}
	if c.authorize == nil {
		return nil, errors.New("MCP tool execution requires the host gateway")
	}
	if len(name) < 1 || len(name) > 128 || len(args) > MaxMessageBytes || checkJSONWithNull(args, true) != nil || len(bytes.TrimSpace(args)) == 0 || bytes.TrimSpace(args)[0] != '{' {
		return nil, errArguments
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.authorize(ctx, name, append(json.RawMessage(nil), args...)); err != nil {
		return nil, errors.New("MCP tool call was not authorized by the host gateway")
	}
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, errClient
	}
	if !boundedResult(result) {
		return nil, errClient
	}
	// The caller can inspect this bounded result, but no continuation is sent.
	if result.NeedsInput() {
		return nil, errors.New("MCP input-required continuation is not enabled")
	}
	return result, nil
}
func boundedResult(v any) bool {
	b, e := json.Marshal(v)
	return e == nil && len(b) <= MaxResponseBytes
}

func registeredHTTPClient(c ClientConfig) (*http.Client, func(), error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, nil, errors.New("registered MCP endpoint must be an exact HTTPS URL")
	}
	if strings.ContainsAny(c.AccessToken, "\r\n\x00 \t") || len(c.AccessToken) > 4096 || (c.OrganizationID != "" && !safeID.MatchString(c.OrganizationID)) {
		return nil, nil, errors.New("invalid registered MCP credentials")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if n, e := strconv.Atoi(port); e != nil || n < 1 || n > 65535 {
		return nil, nil, errors.New("invalid MCP endpoint port")
	}
	dialer := &net.Dialer{Timeout: c.Timeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.TrustedRoots}, TLSHandshakeTimeout: c.Timeout, ResponseHeaderTimeout: c.Timeout, MaxResponseHeaderBytes: 32768, MaxIdleConnsPerHost: 2}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != net.JoinHostPort(u.Hostname(), port) {
			return nil, errClient
		}
		addresses, e := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
		if e != nil || len(addresses) == 0 {
			return nil, errClient
		}
		for _, ip := range addresses {
			if !permittedIP(ip, c.AllowPrivateNetwork) {
				return nil, errors.New("registered MCP address is outside its network policy")
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
	}
	if !c.DispatchDeadline.IsZero() {
		if c.DispatchIdempotencyKey == "" || len(c.DispatchIdempotencyKey) > 128 || strings.ContainsAny(c.DispatchIdempotencyKey, " \t\r\n\x00") {
			return nil, nil, errClient
		}
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		tr.Protocols = protocols
		tr.DisableKeepAlives = true
		tr.DisableCompression = true
		dial := tr.DialContext
		tr.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if !time.Now().Before(c.DispatchDeadline) {
				return nil, errClient
			}
			bounded, cancel := context.WithDeadline(ctx, c.DispatchDeadline)
			defer cancel()
			conn, e := dial(bounded, network, address)
			if e != nil {
				return nil, errClient
			}
			tlsConfig := tr.TLSClientConfig.Clone()
			tlsConfig.ServerName = u.Hostname()
			secure := tls.Client(conn, tlsConfig)
			if e = secure.HandshakeContext(bounded); e != nil {
				conn.Close()
				return nil, errClient
			}
			if !time.Now().Before(c.DispatchDeadline) {
				secure.Close()
				return nil, errClient
			}
			return &clientStartBoundConnection{Conn: secure, deadline: c.DispatchDeadline}, nil
		}
	}
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != u.Scheme || r.URL.Host != u.Host || r.URL.Path != u.Path || r.URL.RawQuery != "" || r.URL.User != nil || r.Method != http.MethodPost {
			return nil, errClient
		}
		clone := r.Clone(r.Context())
		clone.Header = r.Header.Clone()
		clone.Header.Del("Cookie")
		clone.Header.Del("Authorization")
		clone.Header.Del("X-Organization-ID")
		var method string
		var requestID json.RawMessage
		if !c.DispatchDeadline.IsZero() {
			if !time.Now().Before(c.DispatchDeadline) {
				return nil, errClient
			}
			body, e := io.ReadAll(io.LimitReader(r.Body, MaxMessageBytes+1))
			r.Body.Close()
			if e != nil || len(body) > MaxMessageBytes || checkJSONWithNull(body, true) != nil {
				return nil, errClient
			}
			var frame struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
			}
			if json.Unmarshal(body, &frame) != nil {
				return nil, errClient
			}
			method, requestID = frame.Method, frame.ID
			clone.Body = io.NopCloser(bytes.NewReader(body))
			clone.GetBody = nil
			clone.Close = true
			clone.Header.Del("Idempotency-Key")
			if frame.Method == "tools/call" {
				clone.Header.Set("Idempotency-Key", c.DispatchIdempotencyKey)
			}
		}
		if c.AccessToken != "" {
			clone.Header.Set("Authorization", "Bearer "+c.AccessToken)
		}
		if c.OrganizationID != "" {
			clone.Header.Set("X-Organization-ID", c.OrganizationID)
		}
		response, e := tr.RoundTrip(clone)
		if e != nil {
			return nil, errClient
		}
		if response.ContentLength > MaxResponseBytes {
			response.Body.Close()
			return nil, errClient
		}
		if !c.DispatchDeadline.IsZero() {
			// The registered action contract gives structuredContent authority as
			// an effect observation. Validate its original wire JSON before the
			// SDK can collapse duplicate fields or repair invalid Unicode.
			raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
			response.Body.Close()
			if err != nil || len(raw) > MaxResponseBytes {
				return nil, errClient
			}
			valid := validActionResponse(raw, response.Header.Get("Content-Type"))
			if method == "tools/call" && c.ValidateToolResult != nil {
				valid = validToolResultResponse(raw, response.Header.Get("Content-Type"), requestID, c.ValidateToolResult)
			}
			if !valid {
				return nil, errClient
			}
			response.Body = io.NopCloser(bytes.NewReader(raw))
		}
		response.Body = &limitedBody{ReadCloser: response.Body, remaining: MaxResponseBytes}
		return response, nil
	})
	return &http.Client{Transport: rt, Timeout: c.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, tr.CloseIdleConnections, nil
}

// Action-mode responses are finite, bounded JSON or SSE documents. Generic
// interactive MCP streams remain supported by the ordinary read-only client.
func validActionResponse(raw []byte, contentType string) bool {
	return walkActionResponse(raw, contentType, nil)
}

func walkActionResponse(raw []byte, contentType string, visit func([]byte) bool) bool {
	checkMessage := func(body []byte) bool {
		return checkJSONWithNull(body, true) == nil && (visit == nil || visit(body))
	}
	media, _, e := mime.ParseMediaType(contentType)
	if e != nil {
		return false
	}
	if media == "application/json" {
		return checkMessage(raw)
	}
	if media != "text/event-stream" {
		return false
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var data []string
	seen := false
	check := func() bool {
		if len(data) == 0 {
			return true
		}
		body := strings.Join(data, "\n")
		data = nil
		seen = true
		return checkMessage([]byte(body))
	}
	for _, line := range lines {
		if line == "" {
			if !check() {
				return false
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data = append(data, value)
		}
	}
	return check() && seen
}

// Every action-mode POST has a fresh HTTP/1 connection. A discover request must
// never consume a reusable connection's guard and admit a later expired call.
type clientStartBoundConnection struct {
	net.Conn
	deadline time.Time
	started  bool
}

func (c *clientStartBoundConnection) Write(p []byte) (int, error) {
	if c.started {
		return c.Conn.Write(p)
	}
	if !time.Now().Before(c.deadline) {
		return 0, errClient
	}
	if e := c.Conn.SetWriteDeadline(c.deadline); e != nil {
		return 0, e
	}
	n, e := c.Conn.Write(p)
	if n > 0 {
		c.started = true
		_ = c.Conn.SetWriteDeadline(time.Time{})
	}
	return n, e
}
func permittedIP(ip netip.Addr, private bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return private
	}
	return ip.IsGlobalUnicast()
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type limitedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var probe [1]byte
		n, e := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("MCP response limit exceeded")
		}
		return 0, e
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, e := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, e
}

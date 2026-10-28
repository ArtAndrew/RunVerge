// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/jsonwire"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ServerConfig struct {
	Backend Backend
	// Mutation tools are absent unless explicitly enabled by their catalog name.
	AllowedMutations []string
	AllowedOrigins   []string
	RequestTimeout   time.Duration
	MaxConcurrent    int
}
type Server struct {
	backend Backend
	tools   []toolSpec
	origins map[string]bool
	timeout time.Duration
	slots   chan struct{}
}

func NewServer(c ServerConfig) (*Server, error) {
	if c.Backend == nil {
		return nil, errors.New("MCP backend is required")
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 10 * time.Second
	}
	if c.RequestTimeout < time.Millisecond || c.RequestTimeout > 30*time.Second {
		return nil, errors.New("invalid MCP timeout")
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 16
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 64 {
		return nil, errors.New("invalid MCP concurrency")
	}
	specs, err := catalog()
	if err != nil {
		return nil, errors.New("MCP catalog schema failed")
	}
	known := map[string]bool{}
	for _, spec := range specs {
		if spec.Mutation {
			known[spec.Name] = true
		}
	}
	enabled := map[string]bool{}
	for _, name := range c.AllowedMutations {
		if !known[name] || enabled[name] {
			return nil, errors.New("invalid MCP mutation allowlist")
		}
		enabled[name] = true
	}
	s := &Server{backend: c.Backend, origins: map[string]bool{}, timeout: c.RequestTimeout, slots: make(chan struct{}, c.MaxConcurrent)}
	for _, spec := range specs {
		if !spec.Mutation || enabled[spec.Name] {
			s.tools = append(s.tools, spec)
		}
	}
	for _, origin := range c.AllowedOrigins {
		u, e := url.Parse(origin)
		if e != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("invalid MCP allowed origin")
		}
		s.origins[origin] = true
	}
	return s, nil
}
func (s *Server) sdk(credentials Credentials) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-platform", Version: domain.ContractVersion}, &mcp.ServerOptions{SupportedProtocolVersions: []string{ProtocolVersion}, PageSize: 100, Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}})
	for _, spec := range s.tools {
		spec := spec
		server.AddTool(&mcp.Tool{Name: spec.Name, Description: spec.Description, InputSchema: spec.Schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !spec.Mutation}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if req.Params == nil || req.Params.RequestState != "" || len(req.Params.InputResponses) != 0 {
				return nil, &jsonrpc.Error{Code: -32602, Message: "MCP continuations are not enabled"}
			}
			in, e := spec.request(req.Params.Arguments)
			if e != nil {
				return nil, &jsonrpc.Error{Code: -32602, Message: errArguments.Error()}
			}
			out, e := s.backend.Call(ctx, credentials, in)
			if e != nil {
				return toolFailure("backend_unavailable", "Platform API response unavailable; a mutation may have committed. Reuse its original idempotency key to resolve the outcome."), nil
			}
			if len(out.Body) > MaxMessageBytes || !json.Valid(out.Body) {
				return toolFailure("invalid_backend_response", "Platform response is invalid or too large"), nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out.Body)}}, StructuredContent: out.Body, IsError: out.Status < 200 || out.Status >= 300}, nil
		})
	}
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "server/discover", "tools/list", "tools/call":
			case "notifications/cancelled":
				return next(ctx, method, req)
			default:
				return nil, &jsonrpc.Error{Code: -32601, Message: "Method is not enabled in this MCP profile"}
			}
			if versioned, ok := req.(interface{ ProtocolVersion() string }); !ok || versioned.ProtocolVersion() != ProtocolVersion {
				return nil, &jsonrpc.Error{Code: mcp.CodeUnsupportedProtocolVersion, Message: "Only MCP 2026-07-28 is enabled"}
			}
			select {
			case s.slots <- struct{}{}:
				defer func() { <-s.slots }()
			default:
				return nil, &jsonrpc.Error{Code: -32000, Message: "MCP concurrency limit reached"}
			}
			ctx, cancel := context.WithTimeout(ctx, s.timeout)
			defer cancel()
			// Discovery/listing also reauthenticate a stdio principal. The data from
			// this ordinary read is discarded; actual tool calls are authorized again.
			probe, e := s.backend.Call(ctx, credentials, Request{Method: http.MethodGet, Path: "/api/v1/me"})
			if e != nil || probe.Status < 200 || probe.Status >= 300 {
				return nil, &jsonrpc.Error{Code: -32001, Message: "Platform credentials are unavailable or no longer authorized"}
			}
			return next(ctx, method, req)
		}
	})
	return server
}
func toolFailure(code, message string) *mcp.CallToolResult {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message, "retryable": false}})
	return &mcp.CallToolResult{IsError: true, StructuredContent: json.RawMessage(body), Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}
}

// Handler is the private platform-token profile, not an OAuth resource server.
// The deployment must terminate TLS; browser cookies never authenticate MCP.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = privateResponseWriter{w}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "MCP requires POST", 405)
			return
		}
		for _, name := range []string{"MCP-Protocol-Version", "MCP-Method", "MCP-Name"} {
			if len(r.Header.Values(name)) > 1 {
				http.Error(w, "Ambiguous MCP metadata", 400)
				return
			}
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && !s.origins[origins[0]]) {
			http.Error(w, "MCP origin rejected", 403)
			return
		}
		auth, org := r.Header.Values("Authorization"), r.Header.Values("X-Organization-ID")
		if len(auth) != 1 || len(org) != 1 || len(r.Header.Values("Cookie")) != 0 || !strings.HasPrefix(auth[0], "Bearer ") {
			http.Error(w, "MCP credentials required", 401)
			return
		}
		credentials := Credentials{Token: strings.TrimPrefix(auth[0], "Bearer "), OrganizationID: org[0]}
		if !validCredentials(credentials) {
			http.Error(w, "MCP credentials rejected", 401)
			return
		}
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxMessageBytes))
		if e != nil {
			http.Error(w, "MCP message too large", 413)
			return
		}
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || trimmed[0] != '{' || !jsonwire.ValidUnicode(body) || checkJSON(body) != nil || validRequestFrame(body) != nil {
			http.Error(w, "Invalid MCP JSON", 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.sdk(credentials) }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: MaxMessageBytes, PropagateRequestCancellation: true})
		handler.ServeHTTP(w, r)
	})
}

// The SDK sets its own caching headers after transport admission. Enforce the
// platform's private-content policy at the actual header commit boundary.
type privateResponseWriter struct{ http.ResponseWriter }

func (w privateResponseWriter) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.ResponseWriter.WriteHeader(status)
}
func (w privateResponseWriter) Write(body []byte) (int, error) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	return w.ResponseWriter.Write(body)
}
func (w privateResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w privateResponseWriter) FlushError() error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (s *Server) RunStdio(ctx context.Context, c Credentials) error {
	if !validCredentials(c) {
		return errors.New("invalid MCP stdio credentials")
	}
	return s.ServeIO(ctx, c, os.Stdin, os.Stdout)
}

// ServeIO uses the same newline-delimited protocol as stdio and is useful when
// an embedding process already owns the standard streams.
func (s *Server) ServeIO(ctx context.Context, c Credentials, reader io.ReadCloser, writer io.WriteCloser) error {
	if !validCredentials(c) || reader == nil || writer == nil {
		return errors.New("invalid MCP stream configuration")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxMessageBytes+1)
	base := &mcp.IOTransport{Reader: &strictLines{source: reader, scanner: scanner}, Writer: writer, MaxLineLength: MaxMessageBytes}
	return s.sdk(c).Run(ctx, &guardedTransport{base: base, timeout: s.timeout})
}

// Validate the original newline frame before the SDK can collapse duplicate
// JSON fields. The SDK still owns JSON-RPC semantics, cancellation and routing.
type strictLines struct {
	source  io.ReadCloser
	scanner *bufio.Scanner
	pending []byte
}

func (s *strictLines) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		if !s.scanner.Scan() {
			if e := s.scanner.Err(); e != nil {
				return 0, errors.New("invalid MCP frame")
			}
			return 0, io.EOF
		}
		line := s.scanner.Bytes()
		trimmed := bytes.TrimSpace(line)
		if len(line) > MaxMessageBytes || len(trimmed) == 0 || trimmed[0] != '{' || !jsonwire.ValidUnicode(line) || checkJSON(line) != nil || validRequestFrame(line) != nil {
			return 0, errors.New("invalid MCP frame")
		}
		s.pending = append(append([]byte(nil), line...), '\n')
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
func (s *strictLines) Close() error { return s.source.Close() }

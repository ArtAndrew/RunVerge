// SPDX-License-Identifier: Elastic-2.0
// Package modelgateway implements a bounded, non-streaming message wire profile.
// It does not claim general provider compatibility or determine billing prices.
package modelgateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
)

const (
	maxRequestBytes  = 1 << 20
	maxResponseBytes = 2 << 20
	maxHeaderBytes   = 16 << 10
	maxOutputTokens  = 1_000_000
)

type Config struct {
	WireProfile              string
	OpenRouterDataCollection string
	OpenRouterPrice          OpenRouterPrice
	Endpoint                 string
	APIKey                   string
	Model                    string
	MaxOutputTokens          int
	MaxInputBytes            int
	Timeout                  time.Duration
	AllowInsecureLoopback    bool
	TokenLimitField          string
}

type Request struct {
	ID               string
	Instructions     string
	Goal             string
	DispatchDeadline time.Time
	Tools            []domain.ModelToolDefinition
	Messages         []domain.ModelMessage
}

type Result struct {
	Text              string
	ProviderRequestID string
	InputTokens       int64
	OutputTokens      int64
	ToolCalls         []domain.ModelToolCall
}

// OutcomeError intentionally contains no underlying error, URL, provider body,
// credentials, or user input. Unknown means the caller must preserve its
// reservation and reconcile the outcome; it is not permission to retry.
// DefinitiveRejection only proves that this adapter did not dispatch inference.
type OutcomeError struct {
	Code                string
	Unknown             bool
	DefinitiveRejection bool
	HTTPStatus          int
}

func (e *OutcomeError) Error() string {
	return "model gateway: " + e.Code
}

type Gateway struct {
	config Config
	client *http.Client
}

// New validates a trusted administrator-supplied endpoint without connecting.
// No limit is inferred from a provider or selected implicitly.
func New(config Config) (*Gateway, error) {
	if !safeASCII(config.APIKey, 8192) || !validText(config.Model, 256) ||
		config.MaxOutputTokens < 1 || config.MaxOutputTokens > maxOutputTokens ||
		config.MaxInputBytes < 1 || config.MaxInputBytes > maxRequestBytes ||
		config.Timeout <= 0 || config.Timeout > 10*time.Minute ||
		(config.TokenLimitField != "max_tokens" && config.TokenLimitField != "max_completion_tokens") {
		return nil, rejected("invalid_configuration")
	}
	if !validEndpoint(config.Endpoint, config.AllowInsecureLoopback) {
		return nil, rejected("invalid_endpoint")
	}
	if err := validateWireProfile(config); err != nil {
		return nil, err
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: config.Timeout}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    config.Timeout,
		ResponseHeaderTimeout:  config.Timeout,
		MaxResponseHeaderBytes: maxHeaderBytes,
		DisableCompression:     true,
		DisableKeepAlives:      true,
		Protocols:              protocols,
	}
	return &Gateway{
		config: config,
		client: &http.Client{
			Transport: transport,
			Timeout:   config.Timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Execute performs at most one HTTP request. A successful result has validated
// usage counts, not a verified currency cost or a claim of task acceptance.
func (g *Gateway) Execute(ctx context.Context, input Request) (Result, error) {
	return g.execute(ctx, input, nil)
}

// ExecutePrepared consumes a TLS connection prepared by this gateway exactly
// once. It still requires a fresh dispatch permit before any HTTP bytes leave.
func (g *Gateway) ExecutePrepared(ctx context.Context, input Request, connection *PreparedConnection) (Result, error) {
	if connection == nil || !connection.claim(g) {
		return Result{}, rejected("invalid_prepared_connection")
	}
	defer connection.Close()
	return g.execute(ctx, input, connection)
}

func (g *Gateway) execute(ctx context.Context, input Request, prepared *PreparedConnection) (Result, error) {
	if g == nil || g.client == nil || ctx == nil {
		return Result{}, rejected("invalid_request")
	}
	if ctx.Err() != nil {
		return Result{}, rejected("cancelled_before_send")
	}
	ctx, cancel := context.WithTimeout(ctx, g.config.Timeout)
	defer cancel()
	payload, err := g.BuildRequest(input)
	if err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return Result{}, rejected("cancelled_before_send")
	}
	var dispatchPossible atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		// Before a usable connection is obtained, DNS/dial/TLS failures cannot
		// have dispatched an HTTP inference request. From this point onward
		// treat even ambiguous pre-write failures conservatively as unknown.
		GotConn: func(_ httptrace.GotConnInfo) { dispatchPossible.Store(true) },
	})
	// Hide the underlying bytes.Reader from NewRequest so GetBody is not
	// automatically installed. No replay body, idempotency header, connection
	// reuse, HTTP/2 retry, application retry, or redirect is enabled.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.config.Endpoint, io.NopCloser(bytes.NewReader(payload)))
	if err != nil {
		return Result{}, rejected("invalid_request")
	}
	request.ContentLength = int64(len(payload))
	request.GetBody = nil
	request.Close = true
	request.Header.Set("Authorization", "Bearer "+g.config.APIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("X-Request-ID", input.ID)
	if input.DispatchDeadline.IsZero() || !time.Now().Before(input.DispatchDeadline) {
		return Result{}, rejected("dispatch_permit_expired")
	}
	// This absolute deadline authorizes only the start of inference I/O. It is
	// not reset after a stalled caller, DNS lookup or TLS handshake, and it does
	// not shorten the independently bounded response timeout after sending.
	client, cleanup := g.dispatchClient(input.DispatchDeadline)
	if prepared != nil {
		client, cleanup = g.preparedDispatchClient(input.DispatchDeadline, prepared)
	}
	defer cleanup()
	response, err := client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if !dispatchPossible.Load() {
			return Result{}, rejected("connection_not_established")
		}
		return Result{}, unknown("transport_outcome_unknown")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// Status alone cannot establish provider-specific billing semantics.
		// Only a bounded, fixed diagnostic discriminator may be read below;
		// provider messages and bodies must never be exposed.
		outcome := unknown("provider_status_unknown")
		switch {
		case response.StatusCode >= 300 && response.StatusCode <= 399:
			outcome.Code = "redirect_rejected"
		case response.StatusCode == http.StatusTooManyRequests:
			outcome.Code = "provider_rate_limited"
			if g.config.WireProfile == domain.ModelWireOpenRouterTextV1 {
				if code := openRouterLimitCode(response.Body); code != "" {
					outcome.Code = code
				}
			}
		case response.StatusCode >= 400 && response.StatusCode <= 499:
			outcome.Code = "provider_rejected"
		}
		outcome.HTTPStatus = response.StatusCode
		return Result{}, outcome
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" ||
		(response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		return Result{}, unknown("invalid_response_format")
	}
	if response.ContentLength > maxResponseBytes {
		return Result{}, unknown("response_too_large")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Result{}, unknown("response_read_unknown")
	}
	if len(body) > maxResponseBytes {
		return Result{}, unknown("response_too_large")
	}
	expectedModel := ""
	if g.config.WireProfile == domain.ModelWireOpenRouterTextV1 {
		expectedModel = g.config.Model
	}
	result, err := decodeProfileResult(body, g.config.MaxOutputTokens, input.Tools, input.Messages, expectedModel)
	if err != nil {
		return Result{}, err
	}
	if id := response.Header.Get("X-Request-ID"); expectedModel == "" && safeASCII(id, 256) {
		result.ProviderRequestID = id
	}
	return result, nil
}

func rejected(code string) *OutcomeError {
	return &OutcomeError{Code: code, DefinitiveRejection: true}
}

func unknown(code string) *OutcomeError {
	return &OutcomeError{Code: code, Unknown: true}
}

func safeASCII(value string, maxBytes int) bool {
	if len(value) < 1 || len(value) > maxBytes {
		return false
	}
	for _, char := range value {
		if char < 33 || char > 126 {
			return false
		}
	}
	return true
}

func validText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && strings.TrimSpace(value) != "" &&
		utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validEndpoint(endpoint string, allowHTTP bool) bool {
	if len(endpoint) < 1 || len(endpoint) > 2048 || !utf8.ValidString(endpoint) {
		return false
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" || !allowHTTP {
		return false
	}
	address, err := netip.ParseAddr(parsed.Hostname())
	return err == nil && address.Zone() == "" && address.Unmap().IsLoopback()
}

// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"agentplatform.local/platform/internal/jsonwire"
)

const openRouterUsageEndpoint = "https://openrouter.ai/api/v1/generation"
const maxUsageResponseBytes = 256 << 10

var openRouterGenerationID = regexp.MustCompile(`^gen-[0-9A-Za-z-]+$`)

// UsageReadError exposes fixed diagnostics only, never an upstream body, URL,
// generation ID, credential or wrapped transport error. A missing observation
// (including HTTP 404) says nothing about the original request's charge.
type UsageReadError struct {
	Code       string
	HTTPStatus int
}

func (e *UsageReadError) Error() string { return "provider usage: " + e.Code }

// OpenRouterUsageReader reads metadata; it cannot start generation or settle a
// reservation. There is no caller-selected endpoint, proxy, redirect or retry.
type OpenRouterUsageReader struct {
	key     string
	timeout time.Duration
	client  *http.Client
}

// UsageObservation is a provider-reported observation, NOT a final invoice,
// organization/account binding, source-window coverage or execution clearance.
// Exact monetary input and the raw response are retained without float parsing.
// RawResponse returns a copy; observation metadata is also returned by value.
type UsageObservation struct {
	record UsageObservationRecord
	raw    []byte
}

type UsageObservationRecord struct {
	Profile          string    `json:"profile"`
	GenerationID     string    `json:"generation_id"`
	Model            string    `json:"model"`
	TotalCostCredits string    `json:"total_cost_credits"`
	IsBYOK           bool      `json:"is_byok"`
	MicroUSDStatus   string    `json:"microusd_status"`
	ExactMicroUSD    string    `json:"exact_microusd,omitempty"`
	ResponseSHA256   string    `json:"response_sha256"`
	ObservedAt       time.Time `json:"observed_at"`
	SettlementState  string    `json:"settlement_state"`
}

func (o UsageObservation) Record() UsageObservationRecord { return o.record }
func (o UsageObservation) RawResponse() []byte            { return bytes.Clone(o.raw) }

// UsageObservationMatchesBytes checks retained representation consistency only.
// It establishes no network acquisition, credential ownership or final invoice,
// and deliberately does not manufacture an opaque acquisition from caller JSON.
func UsageObservationMatchesBytes(raw []byte, record UsageObservationRecord) bool {
	_, offset := record.ObservedAt.Zone()
	if record.ObservedAt.IsZero() || offset != 0 || len(record.GenerationID) > 128 || !openRouterGenerationID.MatchString(record.GenerationID) {
		return false
	}
	parsed, err := decodeOpenRouterUsage(raw, record.GenerationID, "")
	if err != nil {
		return false
	}
	parsed.record.ObservedAt = record.ObservedAt
	return parsed.record == record
}

func NewOpenRouterUsageReader(apiKey string, timeout time.Duration) (*OpenRouterUsageReader, error) {
	if !safeASCII(apiKey, 8192) || timeout <= 0 || timeout > 30*time.Second {
		return nil, &UsageReadError{Code: "invalid_configuration"}
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: timeout}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
		MaxResponseHeaderBytes: 16 << 10, DisableCompression: true,
		DisableKeepAlives: true, Protocols: protocols,
	}
	return &OpenRouterUsageReader{key: apiKey, timeout: timeout, client: &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Observe makes one bounded metadata GET for an already known generation ID.
// The caller must separately establish the ID's exact local execution/account
// binding before admitting this observation to a recovery evidence store.
func (r *OpenRouterUsageReader) Observe(ctx context.Context, generationID string) (UsageObservation, error) {
	fail := func(code string) (UsageObservation, error) { return UsageObservation{}, &UsageReadError{Code: code} }
	if r == nil || r.client == nil || ctx == nil || len(generationID) > 128 || !openRouterGenerationID.MatchString(generationID) {
		return fail("invalid_request")
	}
	if ctx.Err() != nil {
		return fail("cancelled")
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterUsageEndpoint+"?"+url.Values{"id": {generationID}}.Encode(), nil)
	if err != nil {
		return fail("invalid_request")
	}
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := r.client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return fail("observation_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return UsageObservation{}, &UsageReadError{Code: "provider_status", HTTPStatus: resp.StatusCode}
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || (resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") {
		return fail("invalid_response_format")
	}
	if resp.ContentLength > maxUsageResponseBytes {
		return fail("response_too_large")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageResponseBytes+1))
	if err != nil {
		return fail("observation_unavailable")
	}
	if len(raw) > maxUsageResponseBytes {
		return fail("response_too_large")
	}
	if ctx.Err() != nil {
		return fail("cancelled")
	}
	observation, err := decodeOpenRouterUsage(raw, generationID, r.key)
	if err != nil {
		return UsageObservation{}, err
	}
	if ctx.Err() != nil {
		return fail("cancelled")
	}
	observation.record.ObservedAt = time.Now().UTC()
	return observation, nil
}

func decodeOpenRouterUsage(raw []byte, generationID, key string) (UsageObservation, error) {
	fail := func() (UsageObservation, error) {
		return UsageObservation{}, &UsageReadError{Code: "invalid_observation"}
	}
	if len(raw) == 0 || len(raw) > maxUsageResponseBytes || !jsonwire.ValidUnicode(raw) {
		return fail()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return fail()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fail()
	}
	// Reject direct and JSON-escaped credential echoes before retaining bytes.
	// This is not a general-purpose secret scanner for arbitrary encodings.
	if key != "" && (bytes.Contains(raw, []byte(key)) || usageContainsSecret(value, key)) {
		return fail()
	}
	root, ok := value.(map[string]any)
	if !ok {
		return fail()
	}
	if _, present := root["error"]; present {
		return fail()
	}
	data, ok := root["data"].(map[string]any)
	if !ok || data["id"] != generationID {
		return fail()
	}
	if _, present := data["error"]; present {
		return fail()
	}
	model, validModel := data["model"].(string)
	byok, validBYOK := data["is_byok"].(bool)
	cost, validCost := data["total_cost"].(json.Number)
	if !validModel || !validText(model, 256) || !validBYOK || !validCost {
		return fail()
	}
	micros, status, err := usageExactMicroUSD(string(cost))
	if err != nil {
		return fail()
	}
	hash := sha256.Sum256(raw)
	return UsageObservation{record: UsageObservationRecord{
		Profile: "openrouter-generation-observation-v1", GenerationID: generationID,
		Model: model, TotalCostCredits: string(cost), IsBYOK: byok,
		MicroUSDStatus: status, ExactMicroUSD: micros, ResponseSHA256: hex.EncodeToString(hash[:]),
		SettlementState: "unverified",
	}, raw: bytes.Clone(raw)}, nil
}

func usageContainsSecret(value any, key string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, key)
	case map[string]any:
		for name, child := range v {
			if strings.Contains(name, key) || usageContainsSecret(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if usageContainsSecret(child, key) {
				return true
			}
		}
	}
	return false
}

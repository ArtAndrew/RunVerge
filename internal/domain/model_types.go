// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

// DispatchPermit is a short-lived, persisted admission for starting one request.
// ValidFor is mapped conservatively onto the caller's pre-admission monotonic
// clock; returning the permit after a delayed commit never starts a fresh TTL.
type DispatchPermit struct {
	OrganizationID, TaskID, OperationID, ActionID, ReservationID, IntentHash string
	FencingToken                                                             int64
	IssuedAt, NotAfter                                                       time.Time
	ValidFor                                                                 time.Duration
}

// ModelCandidate is untrusted model output with immutable execution provenance.
// Usage-based estimates are not final provider cost evidence or task acceptance.
type ModelCandidate struct {
	Text, ProviderRequestID, Model, PriceVersion, IntentDigest string
	InputTokens, OutputTokens, EstimatedMicroUSD               int64
	ToolCalls                                                  []ModelToolCall
}

// ModelConnection is an immutable administrator-installed connection version.
// CredentialEnvironment is a restricted secret reference, never the secret.
type ModelConnection struct {
	AccountingMode           string    `json:"accounting_mode,omitempty"`
	ID                       string    `json:"id"`
	OrganizationID           string    `json:"organization_id"`
	Version                  int       `json:"version"`
	Model                    string    `json:"model"`
	ProviderAccount          string    `json:"provider_account"`
	PriceVersion             string    `json:"price_version"`
	Endpoint                 string    `json:"endpoint"`
	CredentialEnvironment    string    `json:"credential_environment"`
	MaxChargeMicroUSD        int64     `json:"max_charge_microusd"`
	MaxInputTokens           int64     `json:"max_input_tokens"`
	InputMicroUSDPerMillion  int64     `json:"input_microusd_per_million"`
	OutputMicroUSDPerMillion int64     `json:"output_microusd_per_million"`
	FixedMicroUSD            int64     `json:"fixed_microusd"`
	MaxOutputTokens          int       `json:"max_output_tokens"`
	MaxInputBytes            int       `json:"max_input_bytes"`
	MaxOutputBytes           int       `json:"max_output_bytes"`
	TimeoutMilliseconds      int       `json:"timeout_milliseconds"`
	BoundAssumptions         string    `json:"bound_assumptions"`
	TokenLimitField          string    `json:"token_limit_field"`
	AllowInsecureLoopback    bool      `json:"allow_insecure_loopback"`
	Active                   bool      `json:"active"`
	CreatedAt                time.Time `json:"created_at"`
}

type AvailableModel struct {
	AccountingMode      string    `json:"accounting_mode"`
	MaxInputTokens      int64     `json:"max_input_tokens"`
	MaxOutputBytes      int       `json:"max_output_bytes"`
	TimeoutMilliseconds int       `json:"timeout_milliseconds"`
	ID                  string    `json:"id"`
	Model               string    `json:"model"`
	Version             int       `json:"version"`
	PriceVersion        string    `json:"price_version"`
	MaxChargeMicroUSD   int64     `json:"max_charge_microusd"`
	MaxInputBytes       int       `json:"max_input_bytes"`
	MaxOutputTokens     int       `json:"max_output_tokens"`
	BoundAssumptions    string    `json:"bound_assumptions"`
	CreatedAt           time.Time `json:"created_at"`
}

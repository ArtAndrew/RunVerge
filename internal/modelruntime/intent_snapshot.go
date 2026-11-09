// SPDX-License-Identifier: Elastic-2.0
package modelruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"agentplatform.local/platform/internal/domain"
)

// IntentSnapshot is the exact historical v1 hash representation. Field order,
// capitalization and omitted legacy fields are part of the stored protocol.
// It excludes leases and current admission/presentation state.
type IntentSnapshot struct {
	Version, OrganizationID, TaskID, RunID, OperationID, AgentID, TaskOwnerID string
	AgentVersion                                                              int
	Goal, Instructions, Endpoint, TokenLimitField                             string
	MaxInputBytes, MaxOutputBytes, MaxOutputTokens                            int
	RequestTimeoutNanos                                                       int64
	Price                                                                     PriceProfile
	WireProfile                                                               string                       `json:"WireProfile,omitempty"`
	OpenRouterDataCollection                                                  string                       `json:"OpenRouterDataCollection,omitempty"`
	RunInput                                                                  string                       `json:"RunInput,omitempty"`
	ModelTools                                                                []domain.ModelToolDefinition `json:"ModelTools,omitempty"`
	ModelMessages                                                             []domain.ModelMessage        `json:"ModelMessages,omitempty"`
}

// HistoricalIntentDigest performs only encoding and hashing. It does not
// authorize, execute, contact a model, normalize prices or assert the truth of
// historical observations. Callers validate immutable source bindings separately.
func HistoricalIntentDigest(snapshot IntentSnapshot) (string, error) {
	if snapshot.Version != "model-attempt-v1" && snapshot.Version != "model-turn-v1" {
		return "", runtimeError("unsupported_intent_snapshot", false)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", runtimeError("invalid_execution_request", false)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// SPDX-License-Identifier: Elastic-2.0
package toolruntime

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

type Runtime struct {
	store  Store
	config Config
}

func (*Runtime) String() string   { return "toolruntime.Runtime{redacted}" }
func (*Runtime) GoString() string { return "toolruntime.Runtime{redacted}" }
func New(store Store, c Config) (*Runtime, error) {
	if store == nil || c.PersistenceTimeout < time.Millisecond || c.PersistenceTimeout > 30*time.Second || len(c.Credential) > 4096 || strings.ContainsAny(c.Credential, "\r\n\x00 \t") {
		return nil, failed("invalid_configuration")
	}
	if e := ValidateConnection(c.Connection); e != nil {
		return nil, e
	}
	if c.Connection.TrustedCAPEM != "" {
		roots, e := connectionRoots(c.Connection.TrustedCAPEM)
		if e != nil {
			return nil, e
		}
		c.TrustedRoots = roots
	}
	if c.Connection.CredentialEnvironment != "" && c.Credential == "" {
		return nil, failed("credentials_unavailable")
	}
	return &Runtime{store: store, config: c}, nil
}

type wireRequest struct {
	ExecutionID string          `json:"execution_id"`
	ActionID    string          `json:"action_id"`
	IntentHash  string          `json:"intent_hash"`
	Arguments   json.RawMessage `json:"arguments"`
}
type wireResult struct {
	ExecutionID     string `json:"execution_id"`
	ActionID        string `json:"action_id"`
	IntentHash      string `json:"intent_hash"`
	Outcome         string `json:"outcome"`
	EffectReference string `json:"effect_reference"`
	Result          string `json:"result"`
}

func (r *Runtime) Execute(ctx context.Context, l *domain.ToolLease) (Outcome, error) {
	out := Outcome{Status: "not_dispatched"}
	if ctx == nil || l == nil || ctx.Err() != nil {
		return out, failed("invalid_execution")
	}
	e := l.Execution
	c := r.config.Connection
	if !c.Tool.Active || e.Status != "leased" || l.FencingToken < 1 || l.LeaseOwner == "" || e.ID == "" || e.OrganizationID != c.Tool.OrganizationID || e.ToolID != c.Tool.ID || e.ToolVersion != c.Tool.Version || e.ToolProfileDigest != domain.ToolConnectionDigest(c) || e.IntentHash != domain.ToolIntentDigest(e) || e.ArgumentsDigest != digest([]byte(e.ArgumentsJSON)) || e.MaxChargeMicroUSD != c.Tool.MaxChargeMicroUSD || e.Provider != c.Tool.Provider || e.PriceVersion != c.Tool.PriceVersion {
		return out, failed("intent_mismatch")
	}
	if _, err := CanonicalArguments(c, e.ArgumentsJSON); err != nil {
		return out, err
	}
	payload, err := json.Marshal(wireRequest{e.ID, e.ActionID, e.IntentHash, json.RawMessage(e.ArgumentsJSON)})
	if err != nil {
		return out, failed("invalid_execution")
	}
	if c.ResultContract == domain.ToolResultContractMCPReadTextV1 {
		payload = []byte(e.ArgumentsJSON)
	}
	admissionStarted := time.Now()
	permit, err := r.store.PrepareToolDispatch(ctx, l)
	if err != nil {
		out.Status = "unknown"
		r.unknownCost(e)
		return out, failed("dispatch_not_admitted")
	}
	deadline := admissionStarted.Add(permit.MaxAge)
	if permit.ExecutionID != e.ID || permit.ActionID != e.ActionID || permit.ReservationID != e.ReservationID || permit.IntentHash != e.IntentHash || permit.FencingToken != l.FencingToken || permit.IssuedAt.IsZero() || permit.MaxAge <= 0 || permit.MaxAge > 2*time.Second || permit.NotAfter.Sub(permit.IssuedAt) != permit.MaxAge || !time.Now().Before(deadline) {
		out.Status = "unknown"
		r.unknownCost(e)
		r.recordUnknown(e, l.FencingToken)
		return out, failed("dispatch_permit_rejected")
	}
	var raw []byte
	if c.Transport == "http_json" {
		raw, err = r.httpExecute(ctx, payload, e.ActionID, deadline)
	} else {
		raw, err = r.mcpExecute(ctx, payload, e.ActionID, deadline)
	}
	out.Status = "unknown"
	// A transport result never settles money, even a configured success receipt.
	// Accounting persistence is independent from lease cancellation and effect.
	// Financial evidence may have settled concurrently. Its state cannot erase
	// the independent effect observation; RecordToolOutcome preserves any hold.
	r.unknownCost(e)
	if err != nil {
		r.recordUnknown(e, l.FencingToken)
		return out, failed("external_outcome_unknown")
	}
	var result wireResult
	if c.ResultContract == domain.ToolResultContractMCPReadTextV1 {
		result, err = decodeReadObservation(raw, e, c.Tool.MaxOutputBytes)
	} else {
		result, err = decodeResult(raw, e, c.Tool.MaxOutputBytes)
	}
	if err != nil || result.Outcome == "unknown" {
		r.recordUnknown(e, l.FencingToken)
		return out, failed("external_outcome_unknown")
	}
	persist, cancel := context.WithTimeout(context.Background(), r.config.PersistenceTimeout)
	defer cancel()
	receipt, err := r.store.RecordToolOutcome(persist, domain.ToolOutcomeEvidence{OrganizationID: e.OrganizationID, ExecutionID: e.ID, IntentHash: e.IntentHash, FencingToken: l.FencingToken, Outcome: result.Outcome, Result: result.Result, EffectReference: result.EffectReference, EvidenceDigest: digest(raw)})
	if err != nil {
		return out, failed("effect_receipt_not_committed")
	}
	out.Status = result.Outcome
	out.ReceiptID = receipt.ID
	return out, nil
}
func (r *Runtime) unknownCost(e domain.ToolExecution) bool {
	ctx, cancel := context.WithTimeout(context.Background(), r.config.PersistenceTimeout)
	defer cancel()
	_, err := r.store.MarkCostUnknown(ctx, e.OrganizationID, e.ReservationID, "tool:accounting-pending", "tool_accounting_pending")
	return err == nil
}
func (r *Runtime) recordUnknown(e domain.ToolExecution, fence int64) {
	ctx, cancel := context.WithTimeout(context.Background(), r.config.PersistenceTimeout)
	defer cancel()
	_, _ = r.store.RecordToolOutcome(ctx, domain.ToolOutcomeEvidence{OrganizationID: e.OrganizationID, ExecutionID: e.ID, IntentHash: e.IntentHash, FencingToken: fence, Outcome: "unknown"})
}
func decodeResult(raw []byte, e domain.ToolExecution, max int) (wireResult, error) {
	var result wireResult
	value, err := strictJSON(raw, 8)
	if err != nil {
		return result, err
	}
	m, ok := value.(map[string]any)
	if !ok || len(m) != 6 {
		return result, failed("invalid_effect_receipt")
	}
	for _, key := range []string{"execution_id", "action_id", "intent_hash", "outcome", "effect_reference", "result"} {
		if _, ok := m[key].(string); !ok {
			return result, failed("invalid_effect_receipt")
		}
	}
	if json.Unmarshal(raw, &result) != nil || result.ExecutionID != e.ID || result.ActionID != e.ActionID || result.IntentHash != e.IntentHash || !text(result.Result, max) || !text(result.EffectReference, 256) || (result.Outcome != "succeeded" && result.Outcome != "rejected" && result.Outcome != "unknown") || result.Outcome != "unknown" && result.EffectReference == "" {
		return result, failed("invalid_effect_receipt")
	}
	return result, nil
}
func (r *Runtime) mcpExecute(ctx context.Context, payload []byte, key string, deadline time.Time) ([]byte, error) {
	c := r.config.Connection
	config := mcpbridge.ClientConfig{Endpoint: c.Endpoint, AccessToken: r.config.Credential, AllowPrivateNetwork: c.AllowPrivateNetwork, TrustedRoots: r.config.TrustedRoots, Timeout: time.Duration(c.Tool.TimeoutSeconds) * time.Second, MaxConcurrent: 1, DispatchDeadline: deadline, DispatchIdempotencyKey: key, AuthorizeCall: func(_ context.Context, name string, args json.RawMessage) error {
		if name != c.RemoteTool || !bytes.Equal(args, payload) || !time.Now().Before(deadline) {
			return failed("dispatch_mismatch")
		}
		return nil
	}}
	var original json.RawMessage
	var observationMu sync.Mutex
	if c.ResultContract == domain.ToolResultContractMCPReadTextV1 {
		config.ValidateToolResult = func(raw json.RawMessage) error {
			if _, _, err := readTextValue(raw, c.Tool.MaxOutputBytes); err != nil {
				return err
			}
			observationMu.Lock()
			original = append(json.RawMessage(nil), raw...)
			observationMu.Unlock()
			return nil
		}
	}
	client, err := mcpbridge.Connect(ctx, config)
	if err != nil {
		return nil, failed("mcp_connection_failed")
	}
	defer client.Close()
	result, err := client.CallTool(ctx, c.RemoteTool, payload)
	if err != nil || result == nil || result.NeedsInput() {
		return nil, failed("mcp_outcome_unknown")
	}
	if c.ResultContract == domain.ToolResultContractMCPReadTextV1 {
		observationMu.Lock()
		raw := append([]byte(nil), original...)
		observationMu.Unlock()
		if len(raw) == 0 {
			return nil, failed("mcp_outcome_unknown")
		}
		return raw, nil
	}
	if result.IsError || result.StructuredContent == nil {
		return nil, failed("mcp_outcome_unknown")
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil || len(raw) > 6*c.Tool.MaxOutputBytes+8192 {
		return nil, failed("response_limit")
	}
	return raw, nil
}

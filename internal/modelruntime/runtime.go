// SPDX-License-Identifier: Elastic-2.0
package modelruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/toolruntime"
)

type Runtime struct {
	store                    Store
	gateway                  *modelgateway.Gateway
	price                    PriceProfile
	endpoint                 string
	maxInputBytes            int
	maxOutputTokens          int
	maxOutputBytes           int
	tokenLimitField          string
	wireProfile              string
	openRouterDataCollection string
	requestTimeout           time.Duration
	persistenceTimeout       time.Duration
}

func (*Runtime) String() string   { return "modelruntime.Runtime{redacted}" }
func (*Runtime) GoString() string { return "modelruntime.Runtime{redacted}" }

func New(store Store, config Config) (*Runtime, error) {
	if store == nil || config.MaxOutputBytes < 1 || config.MaxOutputBytes > 2<<20 ||
		config.PersistenceTimeout <= 0 || config.PersistenceTimeout > 30*time.Second {
		return nil, runtimeError("invalid_runtime_config", false)
	}
	if config.Gateway.WireProfile == domain.ModelWireOpenRouterTextV1 {
		if domain.EffectiveModelAccountingMode(config.Price.AccountingMode) != domain.ModelAccountingMetered {
			return nil, runtimeError("invalid_openrouter_accounting_mode", false)
		}
		config.Gateway.OpenRouterPrice = modelgateway.OpenRouterPrice{
			InputMicroUSDPerMillion:  config.Price.InputMicroUSDPerMillion,
			OutputMicroUSDPerMillion: config.Price.OutputMicroUSDPerMillion,
			FixedMicroUSD:            config.Price.FixedMicroUSD,
		}
	}
	gw, err := modelgateway.New(config.Gateway)
	if err != nil {
		return nil, runtimeError("invalid_gateway_config", false)
	}
	if config.Gateway.Model == domain.FixtureModel {
		return nil, runtimeError("fixture_model_forbidden", false)
	}
	if err = config.Price.validate(config.Gateway.Model, config.Gateway.MaxOutputTokens); err != nil {
		return nil, err
	}
	if config.Price.AccountingMode == domain.ModelAccountingLocalUnmetered {
		u, parseErr := url.Parse(config.Gateway.Endpoint)
		port := 0
		if parseErr == nil {
			port, parseErr = strconv.Atoi(u.Port())
		}
		if parseErr != nil || port < 1 || port > 65535 || !config.Gateway.AllowInsecureLoopback ||
			config.Gateway.Endpoint != "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions" {
			return nil, runtimeError("invalid_local_unmetered_endpoint", false)
		}
	}
	return &Runtime{
		store: store, gateway: gw, price: config.Price,
		endpoint: config.Gateway.Endpoint, maxInputBytes: config.Gateway.MaxInputBytes,
		maxOutputTokens: config.Gateway.MaxOutputTokens, tokenLimitField: config.Gateway.TokenLimitField,
		wireProfile:              config.Gateway.WireProfile,
		openRouterDataCollection: config.Gateway.OpenRouterDataCollection,
		requestTimeout:           config.Gateway.Timeout, maxOutputBytes: config.MaxOutputBytes,
		persistenceTimeout: config.PersistenceTimeout,
	}, nil
}

// Execute admits at most one external attempt for this operation. A reclaimed
// lease reuses the same charge key and immutable intent; it cannot reset the
// action's sent/unknown state. Returning an error never means retry is safe.
func (r *Runtime) Execute(ctx context.Context, lease *domain.Lease) (Outcome, error) {
	out := Outcome{Status: NotDispatched}
	if ctx == nil || lease == nil {
		return out, runtimeError("invalid_execution_request", false)
	}
	if ctx.Err() != nil {
		return out, runtimeError("execution_cancelled", false)
	}
	l := cloneLease(*lease)
	chargeKey, intent, err := r.intent(l)
	if err != nil {
		return out, err
	}
	reservation, err := r.store.ReserveCost(ctx, &l, domain.ReserveCostRequest{
		ChargeKey: chargeKey, Category: "model", Provider: r.price.ProviderAccount,
		PriceVersion: r.price.Version, AmountMicroUSD: r.price.MaxChargeMicroUSD, IntentHash: intent,
	})
	if err != nil {
		// No dispatch follows an ambiguous reservation commit. Replay is subject
		// to the same stable key and durable pending->sent guard.
		return out, runtimeError("reservation_not_admitted", false)
	}
	out.ReservationID, out.CostStatus = reservation.ID, reservation.Status
	if !r.boundReservation(l, reservation, chargeKey) {
		return out, runtimeError("invalid_store_reservation", true)
	}
	if reservation.Status != "reserved" {
		out.Status = AlreadyAttempted
		return out, nil
	}
	var prepared *modelgateway.PreparedConnection
	if r.wireProfile == domain.ModelWireOpenRouterTextV1 {
		// DNS/TCP/TLS can outlast the short dispatch-start permit. Prepare only
		// the socket first, then obtain fresh authority before any HTTP bytes.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.requestTimeout)
		defer cancel()
		prepared, err = r.gateway.PrepareConnection(ctx)
		if err != nil {
			failure := &ExecutionError{Code: "connection_not_prepared"}
			if gatewayFailure, ok := err.(*modelgateway.OutcomeError); ok {
				failure.GatewayCode = gatewayFailure.Code
			}
			return out, failure
		}
		defer prepared.Close()
	}
	admissionStarted := time.Now()
	permit, err := r.store.PrepareModelDispatch(ctx, &l, reservation.ID)
	if err != nil {
		// A commit acknowledgment can be lost, or another executor can have
		// won. No network call and no candidate/pause write in this path.
		out.Status = OutcomeUnknown
		if r.markUnknown(l.OrganizationID, reservation.ID) {
			out.CostStatus = "unknown"
		}
		return out, runtimeError("dispatch_not_admitted", true)
	}
	dispatchDeadline := admissionStarted.Add(permit.ValidFor)
	if permit.OrganizationID != l.OrganizationID || permit.TaskID != l.TaskID || permit.OperationID != l.OperationID || permit.ReservationID != reservation.ID || permit.ActionID != reservation.ActionID || permit.IntentHash != intent || permit.FencingToken != l.Token || permit.IssuedAt.IsZero() || permit.ValidFor <= 0 || permit.ValidFor > 2*time.Second || permit.NotAfter.Sub(permit.IssuedAt) != permit.ValidFor || !time.Now().Before(dispatchDeadline) {
		out.Status = OutcomeUnknown
		if r.markUnknown(l.OrganizationID, reservation.ID) {
			out.CostStatus = "unknown"
		}
		return out, runtimeError("dispatch_permit_rejected", true)
	}

	// This is the only inference request in the lifecycle. Gateway has no automatic
	// redirects, retries, proxy discovery or provider fallback.
	request := modelgateway.Request{
		ID: chargeKey, Instructions: l.Agent.Instructions, Goal: domain.ExecutionInput(l), DispatchDeadline: dispatchDeadline,
		Tools: l.ModelTools, Messages: l.ModelMessages,
	}
	var result modelgateway.Result
	var executionErr error
	if prepared != nil {
		result, executionErr = r.gateway.ExecutePrepared(ctx, request, prepared)
	} else {
		result, executionErr = r.gateway.Execute(ctx, request)
	}
	out.Status = OutcomeUnknown
	if !r.markUnknown(l.OrganizationID, reservation.ID) {
		// A durable sent action still prevents redispatch if accounting cannot
		// be reached. Never publish a candidate on an unacknowledged transition.
		return out, runtimeError("accounting_record_failed", true)
	}
	out.CostStatus = "unknown"
	if executionErr != nil {
		// Even a gateway pre-send rejection occurs after the durable sent
		// marker. This profile has no final-zero-bill evidence authority.
		if !r.pause(l, reservation.ID, PauseOutcomeUnknown) {
			return out, runtimeError("pause_record_failed", true)
		}
		failure := &ExecutionError{Code: "provider_outcome_unknown", Unknown: true}
		if gatewayFailure, ok := executionErr.(*modelgateway.OutcomeError); ok {
			failure.GatewayCode, failure.ProviderStatus = gatewayFailure.Code, gatewayFailure.HTTPStatus
		}
		return out, failure
	}
	if !r.validResult(l, result) || result.InputTokens > r.price.MaxInputTokens {
		if !r.pause(l, reservation.ID, PauseOutputRejected) {
			return out, runtimeError("pause_record_failed", true)
		}
		return out, runtimeError("model_output_rejected", true)
	}
	estimate, err := r.price.Estimate(result.InputTokens, result.OutputTokens)
	if err != nil || estimate > r.price.MaxChargeMicroUSD {
		if !r.pause(l, reservation.ID, PauseOutputRejected) {
			return out, runtimeError("pause_record_failed", true)
		}
		return out, runtimeError("model_price_bound_exceeded", true)
	}
	candidate := domain.ModelCandidate{
		Text: result.Text, ProviderRequestID: result.ProviderRequestID, Model: r.price.Model,
		PriceVersion: r.price.Version, IntentDigest: intent,
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens, EstimatedMicroUSD: estimate,
		ToolCalls: result.ToolCalls,
	}
	persist, cancel := context.WithTimeout(context.Background(), r.persistenceTimeout)
	defer cancel()
	if l.ConfigurationVerificationID != "" {
		observer, ok := r.store.(ConfigurationProbeObserver)
		if !ok {
			return out, runtimeError("probe_receipt_store_unavailable", true)
		}
		// Even provider_request_id can echo the credential. Only a digest and
		// the already validated structural result cross the probe boundary.
		raw, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return out, runtimeError("probe_receipt_not_encoded", true)
		}
		hash := sha256.Sum256(raw)
		probe, recordErr := observer.RecordConfigurationModelProbeObservation(persist, &l, reservation.ID, domain.ConfigurationProbeObservation{
			OrganizationID: l.OrganizationID, VerificationID: l.ConfigurationVerificationID, WorkerID: l.OwnerID, FencingToken: l.Token,
			TaskID: l.TaskID, OperationID: l.OperationID, ReservationID: reservation.ID, IntentDigest: intent,
			Outcome: "response_received", ResponseDigest: hex.EncodeToString(hash[:]),
		})
		if recordErr != nil {
			return out, runtimeError("probe_receipt_not_committed", true)
		}
		if probe {
			candidate.Text, candidate.ProviderRequestID, candidate.ToolCalls = "Connection returned a structurally valid response. Response content was discarded.", "", nil
		}
	}
	artifact, err := r.store.CompleteModelCandidate(persist, &l, reservation.ID, candidate)
	if err != nil {
		// Cancellation, expired lease and revoked authority suppress publication;
		// incurred exposure remains recorded independently above.
		return out, runtimeError("candidate_not_committed", true)
	}
	if artifact.ID == "" || artifact.OrganizationID != l.OrganizationID || artifact.TaskID != l.TaskID {
		return out, runtimeError("invalid_store_artifact", true)
	}
	out.Status, out.ArtifactID = CandidateWaitingInput, artifact.ID
	return out, nil
}

func (r *Runtime) markUnknown(org, reservationID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), r.persistenceTimeout)
	defer cancel()
	_, err := r.store.MarkCostUnknown(ctx, org, reservationID, "model:accounting-pending", PauseAccountingPending)
	return err == nil
}

func (r *Runtime) pause(l domain.Lease, reservationID, reason string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), r.persistenceTimeout)
	defer cancel()
	return r.store.PauseModelAttempt(ctx, &l, reservationID, reason) == nil
}

func (r *Runtime) boundReservation(l domain.Lease, v domain.Reservation, key string) bool {
	if !validText(v.ID, 128) || !validText(v.ActionID, 128) || v.OrganizationID != l.OrganizationID ||
		v.TaskID != l.TaskID || v.OperationID != l.OperationID || v.ChargeKey != key ||
		v.Category != "model" || v.Provider != r.price.ProviderAccount || v.PriceVersion != r.price.Version ||
		v.AmountMicroUSD != r.price.MaxChargeMicroUSD {
		return false
	}
	switch v.Status {
	case "reserved", "unknown", "settled", "released":
		return true
	}
	return false
}

func (r *Runtime) intent(l domain.Lease) (string, string, error) {
	if l.RunInput != "" && (len(l.RunInput) > domain.MaxRunInputBytes || !json.Valid([]byte(l.RunInput))) {
		return "", "", runtimeError("unsupported_execution_request", false)
	}
	for _, id := range []string{l.OrganizationID, l.TaskID, l.RunID, l.OperationID, l.OwnerID} {
		if !validText(id, 100) {
			return "", "", runtimeError("invalid_execution_request", false)
		}
	}
	if l.Token <= 0 || l.Task.ID != l.TaskID || l.Task.OrganizationID != l.OrganizationID ||
		l.Agent.OrganizationID != l.OrganizationID || l.Task.AgentID != l.Agent.ID ||
		l.Task.AgentVersion < 1 || l.Task.AgentVersion != l.Agent.Version || !l.Agent.Published ||
		l.Task.Status != "running" || l.Agent.Model != r.price.Model || !validToolBindings(l) ||
		!validText(l.Task.OwnerID, 100) || !validText(l.Agent.ID, 100) ||
		!validText(l.Task.Goal, r.maxInputBytes) || !validText(l.Agent.Instructions, r.maxInputBytes) {
		return "", "", runtimeError("unsupported_execution_request", false)
	}
	key := "model:" + l.OperationID
	// The request ID sent to the gateway must remain printable ASCII. Internal
	// identifiers are opaque; neither the credential nor a worker lease enters it.
	for _, ch := range key {
		if ch < 0x21 || ch > 0x7e {
			return "", "", runtimeError("invalid_execution_request", false)
		}
	}
	// Admission and network dispatch use one encoder. Escaping, schema metadata
	// and all immutable conversation messages count toward the byte budget.
	if _, err := r.gateway.BuildRequest(modelgateway.Request{ID: key, Instructions: l.Agent.Instructions,
		Goal: domain.ExecutionInput(l), Tools: l.ModelTools, Messages: l.ModelMessages}); err != nil {
		return "", "", runtimeError("input_limit_exceeded", false)
	}
	pinned := struct {
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
	}{
		Version: "model-attempt-v1", OrganizationID: l.OrganizationID, TaskID: l.TaskID,
		RunID: l.RunID, OperationID: l.OperationID, AgentID: l.Agent.ID, TaskOwnerID: l.Task.OwnerID,
		AgentVersion: l.Agent.Version, Goal: l.Task.Goal, Instructions: l.Agent.Instructions,
		Endpoint: r.endpoint, TokenLimitField: r.tokenLimitField, MaxInputBytes: r.maxInputBytes,
		MaxOutputBytes: r.maxOutputBytes, MaxOutputTokens: r.maxOutputTokens,
		RequestTimeoutNanos: int64(r.requestTimeout), Price: r.price, RunInput: l.RunInput,
		WireProfile:              r.wireProfile,
		OpenRouterDataCollection: r.openRouterDataCollection,
		ModelTools:               l.ModelTools, ModelMessages: l.ModelMessages,
	}
	if len(l.ModelTools) != 0 || len(l.ModelMessages) != 0 {
		pinned.Version = "model-turn-v1"
	}
	encoded, err := json.Marshal(pinned)
	if err != nil {
		return "", "", runtimeError("invalid_execution_request", false)
	}
	digest := sha256.Sum256(encoded)
	return key, strings.ToLower(hex.EncodeToString(digest[:])), nil
}

func validToolBindings(l domain.Lease) bool {
	if len(l.Agent.Tools) != len(l.ModelTools) || len(l.Agent.ModelTools) != len(l.ModelTools) ||
		len(l.ModelTools) > domain.MaxModelTools || len(l.ModelTools) == 0 && len(l.ModelMessages) != 0 {
		return false
	}
	seen := make(map[string]bool, len(l.ModelTools))
	for i, definition := range l.ModelTools {
		if definition != l.Agent.ModelTools[i] || definition.ToolID != l.Agent.Tools[i] || seen[definition.ToolID] ||
			definition.Name != domain.ModelToolName(l.OrganizationID, definition.ToolID, definition.ToolVersion) {
			return false
		}
		seen[definition.ToolID] = true
	}
	return true
}

func (r *Runtime) validResult(l domain.Lease, result modelgateway.Result) bool {
	if len(result.ToolCalls) == 0 {
		return validText(result.Text, r.maxOutputBytes)
	}
	if len(result.ToolCalls) > domain.MaxModelToolCalls || len(result.Text) > r.maxOutputBytes || !utf8.ValidString(result.Text) || strings.ContainsRune(result.Text, '\x00') {
		return false
	}
	// A structured turn is bounded as a whole, not eight separately maximized
	// outputs. The representation is only an output bound; exact argument bytes
	// are retained in the candidate and in every subsequently approved intent.
	encoded, err := json.Marshal(struct {
		Text  string                 `json:"text"`
		Calls []domain.ModelToolCall `json:"tool_calls"`
	}{result.Text, result.ToolCalls})
	if err != nil || len(encoded) > r.maxOutputBytes {
		return false
	}
	bindings := make(map[string]domain.ModelToolDefinition, len(l.ModelTools))
	for _, definition := range l.ModelTools {
		bindings[definition.Name] = definition
	}
	for _, call := range result.ToolCalls {
		definition, ok := bindings[call.Name]
		if !ok {
			return false
		}
		digest := sha256.Sum256([]byte(definition.InputSchemaJSON))
		public := domain.ToolConnection{Tool: domain.ToolInfo{ID: definition.ToolID, Version: definition.ToolVersion,
			InputSchemaJSON: definition.InputSchemaJSON, InputSchemaDigest: hex.EncodeToString(digest[:]), MaxInputBytes: domain.MaxToolArgumentsBytes}}
		if exact, err := toolruntime.CanonicalArguments(public, call.ArgumentsJSON); err != nil || exact != call.ArgumentsJSON {
			return false
		}
	}
	return true
}

func cloneLease(l domain.Lease) domain.Lease {
	l.Agent.Tools = append([]string(nil), l.Agent.Tools...)
	l.Agent.ModelTools = append([]domain.ModelToolDefinition(nil), l.Agent.ModelTools...)
	l.ModelTools = append([]domain.ModelToolDefinition(nil), l.ModelTools...)
	l.ModelMessages = append([]domain.ModelMessage(nil), l.ModelMessages...)
	for i := range l.ModelMessages {
		l.ModelMessages[i].ToolCalls = append([]domain.ModelToolCall(nil), l.ModelMessages[i].ToolCalls...)
	}
	return l
}

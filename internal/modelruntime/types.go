// SPDX-License-Identifier: Elastic-2.0
// Package modelruntime coordinates one bounded, durable model attempt. It does
// not accept model output as a final bill or as proof that a task is complete.
package modelruntime

import (
	"context"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
)

// Store is a trusted kernel boundary, not an implementation for HTTP clients.
// Reserve and Prepare must durably recheck current authority and lease fencing.
// Prepare atomically changes a pending action to sent and never admits replay.
// Complete/Pause must also recheck current authority and fencing. Complete
// atomically records an unverified candidate, task/run waiting_input, and a
// blocked operation; it must never mark the task succeeded. MarkCostUnknown is
// accounting ingestion and must work independently of a subsequently lost lease.
type Store interface {
	ReserveCost(context.Context, *domain.Lease, domain.ReserveCostRequest) (domain.Reservation, error)
	PrepareModelDispatch(context.Context, *domain.Lease, string) (domain.DispatchPermit, error)
	MarkCostUnknown(context.Context, string, string, string, string) (domain.Reservation, error)
	CompleteModelCandidate(context.Context, *domain.Lease, string, domain.ModelCandidate) (domain.Artifact, error)
	PauseModelAttempt(context.Context, *domain.Lease, string, string) error
}

// ConfigurationProbeObserver is an optional trusted storage extension. It
// records only a safe structural receipt with the original dispatch identity;
// late facts do not require current publication authority. Returning true marks
// this Task as a connection probe, whose raw peer output must never persist.
type ConfigurationProbeObserver interface {
	RecordConfigurationModelProbeObservation(context.Context, *domain.Lease, string, domain.ConfigurationProbeObservation) (bool, error)
}

// PriceProfile is immutable, administrator-controlled configuration for a single
// provider account, model and price revision. MaxChargeMicroUSD is an explicit
// admission ceiling under BoundAssumptions, not a guaranteed provider invoice.
// MaxInputTokens is a contractual assumption: byte limits do not prove it.
// Rates produce an informational, rounded-up estimate, never CostEvidence.
type PriceProfile struct {
	AccountingMode           string `json:"accounting_mode,omitempty"`
	ProviderAccount          string
	Version                  string
	Model                    string
	BoundAssumptions         string
	MaxChargeMicroUSD        int64
	MaxInputTokens           int64
	InputMicroUSDPerMillion  int64
	OutputMicroUSDPerMillion int64
	FixedMicroUSD            int64
}

type Config struct {
	Gateway            modelgateway.Config
	Price              PriceProfile
	MaxOutputBytes     int
	PersistenceTimeout time.Duration
}

// Avoid accidental disclosure when a caller formats this configuration.
func (Config) String() string   { return "modelruntime.Config{redacted}" }
func (Config) GoString() string { return "modelruntime.Config{redacted}" }

type Outcome struct {
	Status        string
	ReservationID string
	ArtifactID    string
	CostStatus    string
}

const (
	CandidateWaitingInput = "candidate_waiting_input"
	AlreadyAttempted      = "already_attempted"
	OutcomeUnknown        = "outcome_unknown"
	NotDispatched         = "not_dispatched"

	PauseOutcomeUnknown    = "model_outcome_unknown"
	PauseOutputRejected    = "model_output_rejected"
	PauseAccountingPending = "model_accounting_pending"
)

// ExecutionError contains only a fixed internal code. Unknown means that the
// attempt may incur cost; it does not authorize retry, release, or settlement.
// Errors deliberately do not wrap provider or storage errors, which can contain
// credentials, customer text, endpoint addresses, or database details.
type ExecutionError struct {
	Code    string
	Unknown bool
}

func (e *ExecutionError) Error() string { return "model runtime: " + e.Code }

func runtimeError(code string, unknown bool) error {
	return &ExecutionError{Code: code, Unknown: unknown}
}

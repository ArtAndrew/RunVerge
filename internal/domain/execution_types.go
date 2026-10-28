// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"math"
	"time"
)

const MaxMoney int64 = 1_000_000_000_000
const MaxFactualMoney int64 = math.MaxInt64
const MaxTaskDepth = 16

type Run struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	TaskID            string    `json:"task_id"`
	PrincipalID       string    `json:"principal_id"`
	AgentID           string    `json:"agent_id"`
	AgentVersion      int       `json:"agent_version"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	Attempt           int       `json:"attempt,omitempty"`
	InputDigest       string    `json:"input_digest,omitempty"`
	Feedback          string    `json:"feedback,omitempty"`
	SourceCandidateID string    `json:"source_candidate_id,omitempty"`
}
type Operation struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id"`
	TaskID         string     `json:"task_id"`
	RunID          string     `json:"run_id"`
	Status         string     `json:"status"`
	LeaseOwner     string     `json:"lease_owner,omitempty"`
	LeaseUntil     *time.Time `json:"lease_until,omitempty"`
	FencingToken   int64      `json:"fencing_token"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	CreatedAt      time.Time  `json:"created_at"`
}
type Action struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	TaskID         string    `json:"task_id"`
	OperationID    string    `json:"operation_id"`
	IntentHash     string    `json:"intent_hash"`
	IdempotencyKey string    `json:"idempotency_key"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}
type Approval struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id"`
	TaskID         string     `json:"task_id"`
	ActionID       string     `json:"action_id"`
	IntentHash     string     `json:"intent_hash"`
	Status         string     `json:"status"`
	Version        int        `json:"version"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	ExecutionID    string     `json:"execution_id,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	DecisionMethod string     `json:"decision_method,omitempty"`
	Reason         string     `json:"reason,omitempty"`
}
type ApprovalDecision struct {
	ExpectedVersion int    `json:"expected_version"`
	IntentHash      string `json:"intent_hash"`
	Decision        string `json:"decision"`
}
type Reservation struct {
	ID                  string    `json:"id"`
	OrganizationID      string    `json:"organization_id"`
	TaskID              string    `json:"task_id"`
	OperationID         string    `json:"operation_id"`
	ActionID            string    `json:"action_id,omitempty"`
	RootID              string    `json:"root_id"`
	ScopeTaskIDs        []string  `json:"scope_task_ids"`
	ChargeKey           string    `json:"charge_key"`
	Category            string    `json:"category"`
	Provider            string    `json:"provider"`
	PriceVersion        string    `json:"price_version"`
	AmountMicroUSD      int64     `json:"amount_microusd,string"`
	ActualMicroUSD      *int64    `json:"actual_microusd,string,omitempty"`
	Status              string    `json:"status"`
	Revision            int       `json:"revision"`
	PendingQuarantineID string    `json:"pending_quarantine_id,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

// ReserveCostRequest is a trusted-runtime input, never a public billing endpoint.
// A charge key identifies one physical billable attempt, not a logical retry tree.
type ReserveCostRequest struct {
	ChargeKey      string `json:"charge_key"`
	Category       string `json:"category"`
	Provider       string `json:"provider"`
	PriceVersion   string `json:"price_version"`
	AmountMicroUSD int64  `json:"amount_microusd"`
	IntentHash     string `json:"intent_hash"`
}

// CostEvidence must come from authenticated, configured cost ingestion. Holding
// a task token does not authorize claiming that an external bill is zero.
type CostEvidence struct {
	Source         string `json:"source"`
	Reference      string `json:"reference"`
	PayloadDigest  string `json:"payload_digest"`
	ActualMicroUSD int64  `json:"actual_microusd,string"`
	Reason         string `json:"reason,omitempty"`
	Final          bool   `json:"final"`
	Currency       string `json:"currency"`
}
type CostAdjustment struct {
	ExpectedRevision int          `json:"expected_revision"`
	Evidence         CostEvidence `json:"evidence"`
}
type LedgerEntry struct {
	ID                    string    `json:"id"`
	OrganizationID        string    `json:"organization_id"`
	ReservationID         string    `json:"reservation_id"`
	TaskID                string    `json:"task_id"`
	RootID                string    `json:"root_id"`
	Kind                  string    `json:"kind"`
	DeltaReservedMicroUSD int64     `json:"delta_reserved_microusd,string"`
	DeltaSpentMicroUSD    int64     `json:"delta_spent_microusd,string"`
	Synthetic             bool      `json:"synthetic"`
	CreatedAt             time.Time `json:"created_at"`
}
type FinancialIncident struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	ReservationID  string    `json:"reservation_id"`
	TaskID         string    `json:"task_id"`
	Kind           string    `json:"kind"`
	AmountMicroUSD int64     `json:"amount_microusd,string"`
	CreatedAt      time.Time `json:"created_at"`
}
type CostReport struct {
	Reservations            []Reservation       `json:"reservations"`
	Entries                 []LedgerEntry       `json:"entries"`
	Incidents               []FinancialIncident `json:"incidents"`
	BudgetMicroUSD          int64               `json:"budget_microusd"`
	ReservedMicroUSD        int64               `json:"reserved_microusd,string"`
	SpentMicroUSD           int64               `json:"spent_microusd,string"`
	UnknownMicroUSD         int64               `json:"unknown_microusd,string"`
	UnknownReservationCount int64               `json:"unknown_reservation_count,string"`
	FinancialHold           bool                `json:"financial_hold"`
	NextReservationCursor   string              `json:"next_reservation_cursor,omitempty"`
	NextEntryCursor         string              `json:"next_entry_cursor,omitempty"`
	NextIncidentCursor      string              `json:"next_incident_cursor,omitempty"`
	Quarantines             []CostQuarantine    `json:"quarantines"`
	PendingQuarantineCount  int64               `json:"pending_quarantine_count"`
	Incomplete              bool                `json:"incomplete"`
	NextQuarantineCursor    string              `json:"next_quarantine_cursor,omitempty"`
}

// AccountingOutcome acknowledges a durable fact. Quarantined is an accepted
// accounting outcome, not permission to retry the external call or free a hold.
type AccountingOutcome struct {
	Status       string      `json:"status"`
	EvidenceID   string      `json:"evidence_id"`
	QuarantineID string      `json:"quarantine_id,omitempty"`
	Reservation  Reservation `json:"reservation"`
}

// CostQuarantine exposes pending liability without provider bodies or references.
// Unrestricted factual amounts use decimal strings on the wire, preserving
// every int64 value in JavaScript clients as well as Go and Python.
type CostQuarantine struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	ReservationID  string    `json:"reservation_id"`
	EvidenceID     string    `json:"evidence_id"`
	TaskID         string    `json:"task_id"`
	ActualMicroUSD int64     `json:"actual_microusd,string"`
	Status         string    `json:"status"`
	Reason         string    `json:"reason"`
	RequestedKind  string    `json:"requested_kind"`
	BaseRevision   int       `json:"base_revision"`
	Revision       int       `json:"revision"`
	ResolutionID   string    `json:"resolution_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}
type TeamMember struct {
	ID           string `json:"id"`
	AgentID      string `json:"agent_id"`
	AgentVersion int    `json:"agent_version"`
	Task         string `json:"task,omitempty"`
}
type TeamDefinition struct {
	Pattern     string       `json:"pattern"`
	Members     []TeamMember `json:"members"`
	JoinPolicy  string       `json:"join_policy"`
	MaxDepth    int          `json:"max_depth"`
	MaxChildren int          `json:"max_children"`
	MaxParallel int          `json:"max_parallel,omitempty"`
}
type AgentDefinition struct {
	Name           string          `json:"name"`
	Instructions   string          `json:"instructions"`
	Model          string          `json:"model"`
	Tools          []string        `json:"tools"`
	BudgetMicroUSD int64           `json:"budget_microusd"`
	Team           *TeamDefinition `json:"team,omitempty"`
}

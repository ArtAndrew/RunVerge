// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const ContractVersion = "0.14.0"
const FixtureModel = "fixture:echo"
const TeamModel = "team:coordinator"
const FixtureCost int64 = 1000

type Agent struct {
	ID             string                `json:"id"`
	OrganizationID string                `json:"organization_id"`
	Name           string                `json:"name"`
	Instructions   string                `json:"instructions"`
	Model          string                `json:"model"`
	Tools          []string              `json:"tools"`
	BudgetMicroUSD int64                 `json:"budget_microusd"`
	Version        int                   `json:"version"`
	Published      bool                  `json:"published"`
	CreatedAt      time.Time             `json:"created_at"`
	Team           *TeamDefinition       `json:"team,omitempty"`
	ModelTools     []ModelToolDefinition `json:"model_tools,omitempty"`
}
type CreateAgent struct {
	Name           string          `json:"name"`
	Instructions   string          `json:"instructions"`
	Model          string          `json:"model,omitempty"`
	Tools          []string        `json:"tools,omitempty"`
	BudgetMicroUSD *int64          `json:"budget_microusd,omitempty"`
	Team           *TeamDefinition `json:"team,omitempty"`
}
type CreateTask struct {
	Goal                    string `json:"goal"`
	AgentID                 string `json:"agent_id"`
	BudgetMicroUSD          int64  `json:"budget_microusd"`
	MaxRuns                 *int   `json:"max_runs,omitempty"`
	ExecutionTimeoutSeconds *int   `json:"execution_timeout_seconds,omitempty"`
}
type Task struct {
	ID                     string    `json:"id"`
	OrganizationID         string    `json:"organization_id"`
	OwnerID                string    `json:"owner_id"`
	AgentID                string    `json:"agent_id"`
	AgentVersion           int       `json:"agent_version"`
	Goal                   string    `json:"goal"`
	Status                 string    `json:"status"`
	BudgetMicroUSD         int64     `json:"budget_microusd"`
	ReservedMicroUSD       int64     `json:"reserved_microusd,string"`
	SpentMicroUSD          int64     `json:"spent_microusd,string"`
	Version                int       `json:"version"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
	WaitingReason          string    `json:"waiting_reason,omitempty"`
	Result                 string    `json:"result,omitempty"`
	RootID                 string    `json:"root_id"`
	ParentID               string    `json:"parent_id,omitempty"`
	Depth                  int       `json:"depth"`
	FinancialHold          bool      `json:"financial_hold"`
	ModelConnectionID      string    `json:"model_connection_id,omitempty"`
	ModelConnectionVersion int       `json:"model_connection_version,omitempty"`
	CurrentRunID           string    `json:"current_run_id,omitempty"`
	AttemptCount           int       `json:"attempt_count,omitempty"`
	MaxRuns                int       `json:"max_runs,omitempty"`
	ExecutionDeadline      time.Time `json:"execution_deadline,omitempty"`
	CandidateID            string    `json:"candidate_id,omitempty"`
	AcceptedDecisionID     string    `json:"accepted_decision_id,omitempty"`
}
type Event struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	TaskID         string         `json:"task_id"`
	Sequence       int64          `json:"sequence,string"`
	Type           string         `json:"type"`
	PayloadVersion int            `json:"payload_version"`
	Data           map[string]any `json:"data,omitempty"`
	LegacyDataJSON string         `json:"legacy_data_json,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}
type Artifact struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	TaskID         string         `json:"task_id"`
	Name           string         `json:"name"`
	MediaType      string         `json:"media_type"`
	Content        string         `json:"content"`
	Version        int            `json:"version"`
	CreatedAt      time.Time      `json:"created_at"`
	Provenance     map[string]any `json:"provenance"`
}
type Principal struct{ ID, OrganizationID, Role, TokenHash, CredentialKind, Kind string }
type Lease struct {
	OrganizationID, OperationID, TaskID, RunID, OwnerID string
	Token                                               int64
	Task                                                Task
	Agent                                               Agent
	ReservationID                                       string
	ConfigurationVerificationID                         string
	RunInput                                            string
	ModelTools                                          []ModelToolDefinition
	ModelMessages                                       []ModelMessage
}
type Error struct {
	Code      string
	Message   string
	Status    int
	Retryable bool
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func Err(code, message string, status int) error {
	return &Error{Code: code, Message: message, Status: status}
}

var NotFound = &Error{Code: "not_found", Message: "Объект не найден", Status: 404}
var Forbidden = &Error{Code: "forbidden", Message: "Недостаточно прав", Status: 403}
var Unauthenticated = &Error{Code: "unauthenticated", Message: "Требуется действующая учётная запись", Status: 401}

func NewID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("secure random: %w", err))
	}
	return prefix + "_" + hex.EncodeToString(b)
}
func Terminal(s string) bool { return s == "succeeded" || s == "failed" || s == "cancelled" }

func CanTransition(from, to string) bool { return ValidTransition("task", from, to) }

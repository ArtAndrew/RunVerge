// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

const APIContractHeader = "X-Platform-API-Version"
const EventCursorVersion = 1
const EventPayloadVersion = 1
const EventSnapshotLimit = 50
const MaxEventFrameBytes = 65536
const MaxEventPayloadBytes = 16384
const MaxEventBatchBytes = 32768
const MaxSnapshotEventBytes = 262144

// The cursor is an opaque, strictly decoded read coordinate bound to one
// organization and task. It is not a credential or delegated capability.
// Decimal strings preserve all PostgreSQL bigint positions in every SDK.
type EventReplayWindow struct {
	AfterSequence   int64 `json:"after_sequence,string"`
	ThroughSequence int64 `json:"through_sequence,string"`
	Version         int   `json:"version"`
}

// Task and cursor come from the same committed database snapshot. Recent
// events are a bounded preview, not a complete audit or an execution command.
type TaskEventSnapshot struct {
	OrganizationID   string            `json:"organization_id"`
	TaskID           string            `json:"task_id"`
	Task             Task              `json:"task"`
	Events           []Event           `json:"events"`
	Cursor           string            `json:"cursor"`
	ReplayCursor     string            `json:"replay_cursor"`
	Window           EventReplayWindow `json:"window"`
	HasEarlierEvents bool              `json:"has_earlier_events"`
	ObservedAt       time.Time         `json:"observed_at"`
}

// ResetRequired never silently advances a consumer past unavailable replay.
// It returns no events/cursor. The consumer explicitly obtains a new snapshot.
type TaskEventBatch struct {
	OrganizationID string            `json:"organization_id"`
	TaskID         string            `json:"task_id"`
	Events         []Event           `json:"events"`
	Cursor         string            `json:"cursor"`
	HasMore        bool              `json:"has_more"`
	ResetRequired  bool              `json:"reset_required"`
	ResetReason    string            `json:"reset_reason,omitempty"`
	Window         EventReplayWindow `json:"window"`
}

// Each SSE data frame is one complete bounded batch. Only a validated complete
// frame may advance its id/cursor. Heartbeat comments carry no cursor.
type TaskEventStreamFrame struct {
	FrameVersion int               `json:"frame_version"`
	Kind         string            `json:"kind"`
	Batch        *TaskEventBatch   `json:"batch,omitempty"`
	Error        *EventStreamError `json:"error,omitempty"`
}

type EventStreamError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type APICompatibility struct {
	APIVersion           string   `json:"api_version"`
	SupportedVersions    []string `json:"supported_versions"`
	VersionHeader        string   `json:"version_header"`
	HeaderRequired       bool     `json:"header_required"`
	EventPayloadVersions []int    `json:"event_payload_versions"`
	LegacyEventPolicy    string   `json:"legacy_event_policy"`
	CommandRetryPolicy   string   `json:"command_retry_policy"`
}

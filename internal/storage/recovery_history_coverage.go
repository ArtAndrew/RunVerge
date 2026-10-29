// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

var ErrRecoveryHistoryCoverage = errors.New("recovery history classification failed")
var ErrRecoveryHistoryCoverageLimit = errors.New("recovery history classification exceeds supported limits")

// This is a complete enumeration of the identified categories below, not a
// certificate that all other history is semantically validated. Dispositions
// describe requirements for a future importer/reader; no fence is installed by
// producing this report. The typed stage still rolls back and holds execution.
type RecoveryHistoryCoverageReport struct {
	ClassificationVersion int                         `json:"classification_version"`
	Scope                 string                      `json:"scope"`
	ReferenceFormat       string                      `json:"reference_format"`
	Records               []RecoveryHistoryGap        `json:"records"`
	Summaries             []RecoveryHistoryGapSummary `json:"summaries"`
	AffectedOrganizations []string                    `json:"affected_organizations"`
}

type RecoveryHistoryGap struct {
	OrganizationID      string `json:"organization_id"`
	Table               string `json:"table"`
	RecordSHA256        string `json:"record_sha256"`
	Reason              string `json:"reason"`
	RequiredDisposition string `json:"required_disposition"`
}

type RecoveryHistoryGapSummary struct {
	OrganizationID string `json:"organization_id"`
	Table          string `json:"table"`
	Reason         string `json:"reason"`
	Records        int64  `json:"records"`
}

type recoveryHistoryCoverageLimits struct{ records, bytes int }

const recoveryHistoryCoverageMaxRecords = 200000
const recoveryHistoryCoverageMaxBytes = 64 << 20

// Record references use SHA256(encoding/json of this exact v1 struct). Keys
// follow each table's declared primary-key order, excluding organization_id.
// No key, cached response, event payload, agent/tool name or document is exposed.
// These are PK references, not content authentication or encryption; guessable
// keys remain guessable. Keep the report within the recovery operator boundary.
func recoveryHistoryRecordSHA256(org, table string, keys []string) string {
	encoded, _ := json.Marshal(struct {
		Version        int      `json:"version"`
		OrganizationID string   `json:"organization_id"`
		Table          string   `json:"table"`
		Keys           []string `json:"keys"`
	}{1, org, table, keys})
	return Hash(encoded)
}

type recoveryHistoryCoverageRule struct {
	table, reason, disposition string
	keys                       []string
	where, reasonSQL           string
}

// Fixed SQL expressions only. No archive-supplied table/column/SQL is executed.
func recoveryHistoryCoverageRules() []recoveryHistoryCoverageRule {
	return []recoveryHistoryCoverageRule{
		// A legacy-format shape does not prove when or how it was admitted.
		{"agents", "unpinned_tool_metadata", "revalidate_before_new_use", []string{"id", "version::text"}, "model_tools='[]'::jsonb AND tools<>'[]'::jsonb", ""},
		{"events", "", "historical_only_reset_consumers", []string{"sequence::text"}, "", "CASE payload_version WHEN 0 THEN 'legacy_untyped_event' ELSE 'event_provenance_not_reconstructed' END"},
		{"outbox", "delivery_history_not_reconstructed", "historical_only_reset_consumers", []string{"sequence::text"}, "", ""},
		{"task_event_windows", "replay_boundary_not_reconstructed", "historical_only_reset_consumers", []string{"task_id"}, "", ""},
		{"event_replay_window_audit", "replay_audit_not_reconstructed", "historical_only_reset_consumers", []string{"id"}, "", ""},
		{"idempotency_keys", "cached_response_not_reconstructed", "never_replay_saved_response", []string{"principal_id", "scope", "key"}, "", ""},
		{"tasks", "task_transition_history_not_reconstructed", "historical_only_no_execution", []string{"id"}, "", ""},
		{"runs", "run_transition_history_not_reconstructed", "historical_only_no_execution", []string{"id"}, "", ""},
		{"operations", "operation_transition_history_not_reconstructed", "historical_only_no_execution", []string{"id"}, "", ""},
		{"team_executions", "team_transition_history_not_reconstructed", "historical_only_no_execution", []string{"run_id"}, "", ""},
		{"team_members", "team_member_transition_history_not_reconstructed", "historical_only_no_execution", []string{"parent_run_id", "member_id"}, "", ""},
	}
}

// SELECT-only and metadata-only: even oversized cached response/event contents
// are not selected. The caller must first pin the typed schema and check all
// declarative constraints, foreign keys and existing invariant validators.
func checkRecoveryHistoryCoverage(ctx context.Context, tx pgx.Tx, schema string) (RecoveryHistoryCoverageReport, error) {
	return checkRecoveryHistoryCoverageLimits(ctx, tx, schema, recoveryHistoryCoverageLimits{recoveryHistoryCoverageMaxRecords, recoveryHistoryCoverageMaxBytes})
}

func checkRecoveryHistoryCoverageLimits(ctx context.Context, tx pgx.Tx, schema string, limits recoveryHistoryCoverageLimits) (RecoveryHistoryCoverageReport, error) {
	empty := RecoveryHistoryCoverageReport{}
	if ctx == nil || tx == nil || !recoverySnapshotName.MatchString(schema) {
		return empty, ErrRecoveryHistoryCoverage
	}
	if limits.records < 1 || limits.records > recoveryHistoryCoverageMaxRecords || limits.bytes < 1 || limits.bytes > recoveryHistoryCoverageMaxBytes {
		return empty, ErrRecoveryHistoryCoverageLimit
	}
	report := RecoveryHistoryCoverageReport{ClassificationVersion: 1, Scope: "identified_unverified_history", ReferenceFormat: "history-record-sha256-v1", Records: []RecoveryHistoryGap{}, Summaries: []RecoveryHistoryGapSummary{}, AffectedOrganizations: []string{}}
	bytesUsed := 0
	// Reserve conservative space for record structs, summaries, map entries,
	// reference hashes and duplicated small strings as well as raw key material.
	for _, rule := range recoveryHistoryCoverageRules() {
		keys := make([]string, 0, len(rule.keys))
		for _, key := range rule.keys {
			keys = append(keys, "CASE WHEN pg_catalog.octet_length("+key+")<=65536 THEN "+key+" ELSE NULL END")
		}
		reason := rule.reasonSQL
		if reason == "" {
			reason = "'" + rule.reason + "'::text"
		}
		sql := "SELECT CASE WHEN pg_catalog.octet_length(organization_id)<=128 THEN organization_id ELSE NULL END,ARRAY[" + strings.Join(keys, ",") + "]," + reason + " FROM " + pgx.Identifier{schema, rule.table}.Sanitize()
		if rule.where != "" {
			sql += " WHERE " + rule.where
		}
		sql += " LIMIT " + strconv.Itoa(limits.records-len(report.Records)+1)
		rows, err := tx.Query(ctx, sql)
		if err != nil {
			return empty, recoverySnapshotError(ctx, ErrRecoveryHistoryCoverage)
		}
		for rows.Next() {
			var org, reason string
			var keys []string
			if rows.Scan(&org, &keys, &reason) != nil {
				rows.Close()
				return empty, recoverySnapshotError(ctx, ErrRecoveryHistoryCoverageLimit)
			}
			bytesUsed += 1024 + len(org)
			for _, key := range keys {
				bytesUsed += len(key)
			}
			if len(report.Records) >= limits.records || bytesUsed > limits.bytes {
				rows.Close()
				return empty, ErrRecoveryHistoryCoverageLimit
			}
			report.Records = append(report.Records, RecoveryHistoryGap{OrganizationID: org, Table: rule.table, RecordSHA256: recoveryHistoryRecordSHA256(org, rule.table, keys), Reason: reason, RequiredDisposition: rule.disposition})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return empty, recoverySnapshotError(ctx, ErrRecoveryHistoryCoverage)
		}
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	sort.Slice(report.Records, func(i, j int) bool {
		a, b := report.Records[i], report.Records[j]
		if a.OrganizationID != b.OrganizationID {
			return a.OrganizationID < b.OrganizationID
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.RecordSHA256 < b.RecordSHA256
	})
	for _, record := range report.Records {
		if len(report.AffectedOrganizations) == 0 || report.AffectedOrganizations[len(report.AffectedOrganizations)-1] != record.OrganizationID {
			report.AffectedOrganizations = append(report.AffectedOrganizations, record.OrganizationID)
		}
		n := len(report.Summaries)
		if n == 0 || report.Summaries[n-1].OrganizationID != record.OrganizationID || report.Summaries[n-1].Table != record.Table || report.Summaries[n-1].Reason != record.Reason {
			report.Summaries = append(report.Summaries, RecoveryHistoryGapSummary{OrganizationID: record.OrganizationID, Table: record.Table, Reason: record.Reason})
			n++
		}
		report.Summaries[n-1].Records++
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return report, nil
}

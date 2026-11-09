// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/jackc/pgx/v5"
)

// This is an inventory of recorded admissions, not a proof of complete sender
// coverage, network delivery, effects or billing. The two views are immutable
// source snapshots; current financial positions remain in the parent review.
type RecoveryExecutionWindow struct {
	Version             int                             `json:"version"`
	Algorithm           string                          `json:"algorithm"`
	Families            []RecoveryExecutionWindowFamily `json:"families"`
	Records             []RecoveryExecutionWindowRecord `json:"records"`
	CoverageProven      bool                            `json:"coverage_proven"`
	SourceStopRechecked bool                            `json:"source_stop_rechecked"`
	NewWorkAllowed      bool                            `json:"new_work_allowed"`
}
type RecoveryExecutionWindowFamily struct {
	Family                  string `json:"family"`
	OriginalRecords         int    `json:"original_records"`
	CapturedRecords         int    `json:"captured_records"`
	OriginalDispatchRecords int    `json:"original_dispatch_records"`
	CapturedDispatchRecords int    `json:"captured_dispatch_records"`
}
type RecoveryExecutionWindowRecord struct {
	Family         string                           `json:"family"`
	OrganizationID string                           `json:"organization_id"`
	RecordID       string                           `json:"record_id"`
	Original       *RecoveryExecutionWindowPosition `json:"original"`
	Captured       *RecoveryExecutionWindowPosition `json:"captured"`
	Comparison     string                           `json:"comparison"`
}
type RecoveryExecutionWindowPosition struct {
	Kind             string   `json:"kind"`
	Status           string   `json:"status"`
	TaskID           string   `json:"task_id"`
	OperationID      string   `json:"operation_id"`
	RelatedIDs       []string `json:"related_ids"`
	IdentitySHA256   string   `json:"identity_sha256"`
	RowSHA256        string   `json:"row_sha256"`
	DispatchRecorded bool     `json:"dispatch_recorded"`
	DispatchSHA256   string   `json:"dispatch_sha256"`
}

type recoveryWindowKey struct{ family, org, id string }
type recoveryWindowSnapshot map[recoveryWindowKey]RecoveryExecutionWindowPosition

func recoveryWindowFamilies() []string {
	return []string{"action", "configuration_verification", "reservation_without_action", "tool_execution", "webhook_attempt"}
}

// Caller has validated and staged the complete typed snapshot under the target
// gate. SQL is fixed, and no inner join can hide an action without a reservation.
func readRecoveryExecutionWindow(ctx context.Context, tx pgx.Tx, schema string, work *recoveryReviewWork) (recoveryWindowSnapshot, error) {
	if ctx == nil || tx == nil || work == nil || !recoverySnapshotName.MatchString(schema) {
		return nil, ErrRecoveryAccounting
	}
	q := func(table string) string { return pgx.Identifier{schema, table}.Sanitize() }
	hash := func(expr string) string {
		return "pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to((" + expr + ")::text,'UTF8')),'hex')"
	}
	reservationIdentity := "to_jsonb(r)-ARRAY['status','actual_microusd','revision','pending_quarantine_id']"
	// Bound the aggregate before collecting identities. Group once, rather than
	// scanning all reservations for every action; the FK has no action index.
	var reservationCount int
	if tx.QueryRow(ctx, "SELECT count(*) FROM (SELECT 1 FROM "+q("reservations")+" LIMIT 10001) bounded").Scan(&reservationCount) != nil || reservationCount > recoveryPlanMaxItems {
		return nil, ErrRecoveryAccounting
	}
	actionRefs := "SELECT r.organization_id,r.action_id,jsonb_agg(r.id ORDER BY r.id COLLATE \"C\") AS ids,jsonb_agg(" + reservationIdentity + " ORDER BY r.id COLLATE \"C\") AS identities,CASE WHEN count(DISTINCT r.category)=1 THEN min(r.category) ELSE 'mixed_or_unbound' END AS kind FROM " + q("reservations") + " r WHERE r.action_id IS NOT NULL GROUP BY r.organization_id,r.action_id"

	queries := map[string]string{
		"action":                     "SELECT a.organization_id,a.id,COALESCE(z.kind,'mixed_or_unbound'),a.status,a.task_id,a.operation_id,COALESCE(z.ids,'[]'::jsonb)," + hash("jsonb_build_array(to_jsonb(a)-ARRAY['status','dispatch_issued_at','dispatch_not_after','dispatch_fencing_token'],COALESCE(z.identities,'[]'::jsonb))") + "," + hash("to_jsonb(a)") + ",a.dispatch_issued_at IS NOT NULL,CASE WHEN a.dispatch_issued_at IS NULL THEN '' ELSE " + hash("jsonb_build_array(a.dispatch_issued_at,a.dispatch_not_after,a.dispatch_fencing_token)") + " END FROM " + q("actions") + " a LEFT JOIN (" + actionRefs + ") z ON z.organization_id=a.organization_id AND z.action_id=a.id",
		"reservation_without_action": "SELECT r.organization_id,r.id,r.category,r.status,r.task_id,r.operation_id,'[]'::jsonb," + hash(reservationIdentity) + "," + hash("to_jsonb(r)") + ",false,'' FROM " + q("reservations") + " r WHERE r.action_id IS NULL",
		"tool_execution":             "SELECT e.organization_id,e.id,'tool',e.status,e.task_id,e.operation_id,jsonb_build_array(e.action_id,e.reservation_id)," + hash("to_jsonb(e)-ARRAY['status','version','reason','dispatch_requested_by','dispatcher_authority_epoch','dispatcher_policy_epoch','result_receipt_id','attempts','lease_owner','lease_until','fencing_token','dispatch_issued_at','dispatch_not_after','dispatch_fencing_token']") + "," + hash("to_jsonb(e)") + ",e.dispatch_issued_at IS NOT NULL,CASE WHEN e.dispatch_issued_at IS NULL THEN '' ELSE " + hash("jsonb_build_array(e.dispatch_issued_at,e.dispatch_not_after,e.dispatch_fencing_token)") + " END FROM " + q("tool_executions") + " e",
		"configuration_verification": "SELECT v.organization_id,v.id,v.kind,v.status,COALESCE(v.task_id,''),v.operation_id,jsonb_build_array(v.plan_id,v.target_id)," + hash("to_jsonb(v)-ARRAY['status','version','worker_id','lease_until','fencing_token','dispatch_started_at','dispatched_worker_id','dispatched_fencing_token','outcome','evidence_digest','reason_code','updated_at']") + "," + hash("to_jsonb(v)") + ",v.dispatch_started_at IS NOT NULL,CASE WHEN v.dispatch_started_at IS NULL THEN '' ELSE " + hash("jsonb_build_array(v.dispatch_started_at,v.dispatched_worker_id,v.dispatched_fencing_token)") + " END FROM " + q("configuration_verifications") + " v",
		"webhook_attempt":            "SELECT a.organization_id,a.id,'webhook',a.status,a.task_id,'',jsonb_build_array(a.delivery_id)," + hash("jsonb_build_array(a.organization_id,a.id,a.task_id,a.delivery_id,a.number,a.created_at,d.subscription_id,d.destination_id,d.destination_version,d.profile_digest,d.event_id,d.event_sequence,d.envelope_digest,d.max_attempts,d.created_at,d.expires_at)") + "," + hash("to_jsonb(a)") + ",a.sent_at IS NOT NULL,CASE WHEN a.sent_at IS NULL THEN '' ELSE " + hash("jsonb_build_array(a.sent_at,a.signature_timestamp,a.worker_id,a.lease_until,a.fencing_token,a.recoveries)") + " END FROM " + q("webhook_attempts") + " a LEFT JOIN " + q("webhook_deliveries") + " d ON d.organization_id=a.organization_id AND d.id=a.delivery_id",
	}
	result := recoveryWindowSnapshot{}
	for _, family := range recoveryWindowFamilies() {
		// The fixed outer order is byte-stable across database collations.
		rows, err := tx.Query(ctx, "SELECT * FROM ("+queries[family]+") entries ORDER BY entries.organization_id COLLATE \"C\",entries.id COLLATE \"C\" LIMIT 10001")
		if err != nil {
			return nil, ErrRecoveryAccounting
		}
		for rows.Next() {
			var org, id string
			var p RecoveryExecutionWindowPosition
			var related []byte
			if rows.Scan(&org, &id, &p.Kind, &p.Status, &p.TaskID, &p.OperationID, &related, &p.IdentitySHA256, &p.RowSHA256, &p.DispatchRecorded, &p.DispatchSHA256) != nil || len(result) >= recoveryPlanMaxItems || len(related) > 256<<10 || json.Unmarshal(related, &p.RelatedIDs) != nil || len(p.RelatedIDs) > recoveryPlanMaxItems || p.RelatedIDs == nil {
				rows.Close()
				return nil, ErrRecoveryAccounting
			}
			if len(org) == 0 || len(org) > 128 || len(id) == 0 || len(id) > 128 || len(p.TaskID) > 128 || len(p.OperationID) > 128 || len(p.Kind) > 64 || len(p.Status) > 64 || len(p.IdentitySHA256) != 64 || len(p.RowSHA256) != 64 || (p.DispatchRecorded && len(p.DispatchSHA256) != 64) || (!p.DispatchRecorded && p.DispatchSHA256 != "") {
				rows.Close()
				return nil, ErrRecoveryAccounting
			}
			for _, ref := range p.RelatedIDs {
				if len(ref) == 0 || len(ref) > 128 {
					rows.Close()
					return nil, ErrRecoveryAccounting
				}
			}
			key := recoveryWindowKey{family, org, id}
			if _, exists := result[key]; exists {
				rows.Close()
				return nil, ErrRecoveryAccounting
			}
			if work.add(org, id, p.Kind, p.Status, p.TaskID, p.OperationID, string(related), p.IdentitySHA256, p.RowSHA256, p.DispatchSHA256) != nil {
				rows.Close()
				return nil, ErrRecoveryAccounting
			}
			result[key] = p
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, ErrRecoveryAccounting
		}
	}
	return result, nil
}

func compareRecoveryExecutionWindow(original, captured recoveryWindowSnapshot) RecoveryExecutionWindow {
	r := RecoveryExecutionWindow{Version: 1, Algorithm: "source-dispatch-inventory-v1", Families: []RecoveryExecutionWindowFamily{}, Records: []RecoveryExecutionWindowRecord{}}
	counts := map[string]*RecoveryExecutionWindowFamily{}
	for _, family := range recoveryWindowFamilies() {
		counts[family] = &RecoveryExecutionWindowFamily{Family: family}
	}
	records := map[recoveryWindowKey]*RecoveryExecutionWindowRecord{}
	for index, snapshot := range []recoveryWindowSnapshot{original, captured} {
		for key, p := range snapshot {
			entry := records[key]
			if entry == nil {
				entry = &RecoveryExecutionWindowRecord{Family: key.family, OrganizationID: key.org, RecordID: key.id}
				records[key] = entry
			}
			position := p
			position.RelatedIDs = append([]string{}, p.RelatedIDs...)
			c := counts[key.family]
			if index == 0 {
				entry.Original = &position
				c.OriginalRecords++
				if p.DispatchRecorded {
					c.OriginalDispatchRecords++
				}
			} else {
				entry.Captured = &position
				c.CapturedRecords++
				if p.DispatchRecorded {
					c.CapturedDispatchRecords++
				}
			}
		}
	}
	for _, e := range records {
		a, b := e.Original, e.Captured
		switch {
		case a == nil:
			e.Comparison = "captured_only"
		case b == nil:
			e.Comparison = "missing_from_capture"
		case a.IdentitySHA256 != b.IdentitySHA256:
			e.Comparison = "identity_changed"
		case a.DispatchRecorded && (!b.DispatchRecorded || a.DispatchSHA256 != b.DispatchSHA256):
			e.Comparison = "dispatch_record_changed"
		case !a.DispatchRecorded && b.DispatchRecorded:
			e.Comparison = "dispatch_recorded_after_backup"
		case a.DispatchRecorded:
			e.Comparison = "dispatch_recorded_in_both"
		default:
			e.Comparison = "no_dispatch_record"
		}
		r.Records = append(r.Records, *e)
	}
	sort.Slice(r.Records, func(i, j int) bool {
		a, b := r.Records[i], r.Records[j]
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		if a.OrganizationID != b.OrganizationID {
			return a.OrganizationID < b.OrganizationID
		}
		return a.RecordID < b.RecordID
	})
	for _, family := range recoveryWindowFamilies() {
		r.Families = append(r.Families, *counts[family])
	}
	return r
}

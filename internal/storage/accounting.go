// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// QuarantinedError is only a compatibility signal for the old internal API.
// The fact is durably accepted: callers must not retry the network or release
// its reservation. New code uses AccountCost's explicit acknowledged outcome.
type QuarantinedError struct{ Outcome domain.AccountingOutcome }

func (e *QuarantinedError) Error() string { return "accounting fact accepted into quarantine" }

func validEvidence(e domain.CostEvidence) bool {
	return e.Final && e.Currency == "USD" && validText(e.Source, 128) && validText(e.Reference, 256) && validDigest(e.PayloadDigest) && e.ActualMicroUSD >= 0 && utf8.ValidString(e.Reason) && len(e.Reason) <= 256 && !strings.ContainsRune(e.Reason, '\x00')
}

// This semantic digest deliberately preserves the 0.2 representation. Changing
// the public money encoding must not invalidate already accepted facts/replays.
func evidenceRequestHash(e domain.CostEvidence, expected *int) string {
	type values struct {
		Source         string `json:"source"`
		Reference      string `json:"reference"`
		PayloadDigest  string `json:"payload_digest"`
		ActualMicroUSD int64  `json:"actual_microusd"`
		Reason         string `json:"reason,omitempty"`
		Final          bool   `json:"final"`
		Currency       string `json:"currency"`
	}
	return digestValue(struct {
		Evidence values
		Expected *int
	}{values{e.Source, e.Reference, e.PayloadDigest, e.ActualMicroUSD, e.Reason, e.Final, e.Currency}, expected})
}

func preflightCostDelta(s costScopes, reserveDelta, spentDelta int64) error {
	for _, pair := range [][2]int64{{s.reserved, reserveDelta}, {s.spent, spentDelta}} {
		if _, e := checkedCounter(pair[0], pair[1]); e != nil {
			return e
		}
	}
	for _, t := range s.tasks {
		if _, e := checkedCounter(t.ReservedMicroUSD, reserveDelta); e != nil {
			return e
		}
		if _, e := checkedCounter(t.SpentMicroUSD, spentDelta); e != nil {
			return e
		}
	}
	return nil
}

func holdCostScopes(ctx context.Context, tx pgx.Tx, s costScopes) error {
	if _, e := tx.Exec(ctx, "UPDATE organizations SET financial_hold=true WHERE id=$1", s.org); e != nil {
		return e
	}
	for _, t := range s.tasks {
		if _, e := tx.Exec(ctx, "UPDATE tasks SET financial_hold=true,version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", s.org, t.ID); e != nil {
			return e
		}
	}
	return nil
}

func incident(ctx context.Context, tx pgx.Tx, r domain.Reservation, kind string, amount int64) error {
	_, e := tx.Exec(ctx, "INSERT INTO financial_incidents(organization_id,id,reservation_id,task_id,kind,amount_microusd) VALUES($1,$2,$3,$4,$5,$6)", r.OrganizationID, domain.NewID("inc"), r.ID, r.TaskID, kind, amount)
	return e
}

func (s *Store) SettleCost(ctx context.Context, org, id string, evidence domain.CostEvidence) (domain.Reservation, error) {
	out, e := s.AccountCost(ctx, org, id, evidence)
	if e == nil && out.Status == "quarantined" {
		e = &QuarantinedError{out}
	}
	return out.Reservation, e
}
func (s *Store) AdjustCost(ctx context.Context, org, id string, in domain.CostAdjustment) (domain.Reservation, error) {
	out, e := s.AccountAdjustment(ctx, org, id, in)
	if e == nil && out.Status == "quarantined" {
		e = &QuarantinedError{out}
	}
	return out.Reservation, e
}

// AccountCost accepts trusted final provider facts independently of current task
// authority, cancellation or lease ownership. Users cannot invoke this via API.
func (s *Store) AccountCost(ctx context.Context, org, id string, evidence domain.CostEvidence) (domain.AccountingOutcome, error) {
	return s.accountCost(ctx, org, id, evidence, nil)
}
func (s *Store) AccountAdjustment(ctx context.Context, org, id string, in domain.CostAdjustment) (domain.AccountingOutcome, error) {
	if in.ExpectedRevision < 1 || strings.TrimSpace(in.Evidence.Reason) == "" {
		return domain.AccountingOutcome{}, domain.Err("invalid_request", "Корректировка требует версии и причины", 400)
	}
	return s.accountCost(ctx, org, id, in.Evidence, &in.ExpectedRevision)
}

func (s *Store) accountCost(ctx context.Context, org, id string, evidence domain.CostEvidence, expected *int) (domain.AccountingOutcome, error) {
	var empty domain.AccountingOutcome
	if !validEvidence(evidence) {
		return empty, domain.Err("invalid_request", "Некорректное свидетельство стоимости", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback(ctx)
	r, scopes, e := lockReservation(ctx, tx, org, id)
	if e != nil {
		return empty, e
	}
	if evidence.Source != r.Provider {
		return empty, domain.Forbidden
	}
	if r.ActionID == "" {
		return empty, domain.Err("conflict", "Синтетический резерв не принимает внешнее начисление", 409)
	}
	hash := evidenceRequestHash(evidence, expected)
	var evidenceID, previousReservation, previousHash string
	e = tx.QueryRow(ctx, "SELECT id,reservation_id,request_hash FROM cost_evidence WHERE organization_id=$1 AND source=$2 AND reference=$3", org, evidence.Source, evidence.Reference).Scan(&evidenceID, &previousReservation, &previousHash)
	if e == nil {
		if previousReservation != id || previousHash != hash {
			return empty, domain.Err("conflict", "Свидетельство стоимости уже принято с другими данными", 409)
		}
		out, e := accountingOutcomeTx(ctx, tx, r, evidenceID)
		if e != nil {
			return empty, e
		}
		if e = tx.Commit(ctx); e != nil {
			return empty, e
		}
		return out, nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return empty, e
	}
	if r.PendingQuarantineID != "" {
		return empty, domain.Err("conflict", "Сначала требуется разрешение карантина стоимости", 409)
	}
	if expected == nil && r.Status == "settled" {
		return empty, domain.Err("conflict", "Для изменения расхода нужна корректировка", 409)
	}
	if expected != nil && (r.Status != "settled" || r.Revision != *expected) {
		return empty, domain.Err("conflict", "Версия расхода изменилась", 409)
	}
	var actionState string
	if e = tx.QueryRow(ctx, "SELECT status FROM actions WHERE organization_id=$1 AND id=$2 FOR UPDATE", org, r.ActionID).Scan(&actionState); e != nil {
		return empty, e
	}
	evidenceID = domain.NewID("evi")
	if _, e = tx.Exec(ctx, "INSERT INTO cost_evidence(organization_id,id,source,reference,reservation_id,payload_digest,actual_microusd,reason,request_hash,final,currency) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", org, evidenceID, evidence.Source, evidence.Reference, id, evidence.PayloadDigest, evidence.ActualMicroUSD, evidence.Reason, hash, evidence.Final, evidence.Currency); e != nil {
		return empty, e
	}
	kind := "settle"
	if expected != nil {
		kind = "adjustment"
	}
	reserveDelta, spentDelta := costDeltas(r, evidence.ActualMicroUSD)
	if preflightCostDelta(scopes, reserveDelta, spentDelta) != nil {
		qid := domain.NewID("quar")
		if _, e = tx.Exec(ctx, "INSERT INTO cost_quarantines(organization_id,id,evidence_id,reservation_id,base_revision,revision,base_status,base_actual_microusd,requested_kind,expected_revision,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'projection_overflow')", org, qid, evidenceID, id, r.Revision, r.Revision+1, r.Status, r.ActualMicroUSD, kind, expected); e != nil {
			return empty, e
		}
		r, e = scanReservation(tx.QueryRow(ctx, "UPDATE reservations SET status=CASE WHEN status='reserved' THEN 'unknown' ELSE status END,revision=revision+1,pending_quarantine_id=$3 WHERE organization_id=$1 AND id=$2 RETURNING "+reservationColumns, org, id, qid))
		if e != nil {
			return empty, e
		}
		if e = holdCostScopes(ctx, tx, scopes); e != nil {
			return empty, e
		}
		if _, e = tx.Exec(ctx, "UPDATE actions SET status='unknown' WHERE organization_id=$1 AND id=$2 AND status='sent'", org, r.ActionID); e != nil {
			return empty, e
		}
		if e = incident(ctx, tx, r, "projection_overflow", evidence.ActualMicroUSD); e != nil {
			return empty, e
		}
		if e = recordLedger(ctx, tx, r, "quarantine:"+qid, hash, "quarantine", 0, 0); e != nil {
			return empty, e
		}
		if e = emit(ctx, tx, org, r.TaskID, "cost.quarantined", map[string]any{"reservation_id": id, "evidence_id": evidenceID, "quarantine_id": qid, "actual_microusd": strconv.FormatInt(evidence.ActualMicroUSD, 10), "reason": "projection_overflow"}); e != nil {
			return empty, e
		}
		out := domain.AccountingOutcome{Status: "quarantined", EvidenceID: evidenceID, QuarantineID: qid, Reservation: r}
		if e = tx.Commit(ctx); e != nil {
			return empty, e
		}
		return out, nil
	}
	r, e = bookCostEvidence(ctx, tx, r, &scopes, evidence, actionState, kind+":"+evidence.Source+":"+evidence.Reference, hash, kind)
	if e != nil {
		return empty, e
	}
	out := domain.AccountingOutcome{Status: "settled", EvidenceID: evidenceID, Reservation: r}
	if e = tx.Commit(ctx); e != nil {
		return empty, e
	}
	return out, nil
}

func costDeltas(r domain.Reservation, actual int64) (int64, int64) {
	var prior, reserveDelta int64
	if r.ActualMicroUSD != nil {
		prior = *r.ActualMicroUSD
	}
	if r.Status == "reserved" || r.Status == "unknown" {
		reserveDelta = -r.AmountMicroUSD
	}
	return reserveDelta, actual - prior
}

func accountingOutcomeTx(ctx context.Context, tx pgx.Tx, r domain.Reservation, evidenceID string) (domain.AccountingOutcome, error) {
	out := domain.AccountingOutcome{Status: "settled", EvidenceID: evidenceID, Reservation: r}
	var resolved bool
	e := tx.QueryRow(ctx, "SELECT q.id,EXISTS(SELECT 1 FROM cost_quarantine_resolutions x WHERE x.organization_id=q.organization_id AND x.quarantine_id=q.id) FROM cost_quarantines q WHERE q.organization_id=$1 AND q.evidence_id=$2", r.OrganizationID, evidenceID).Scan(&out.QuarantineID, &resolved)
	if errors.Is(e, pgx.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return domain.AccountingOutcome{}, e
	}
	if !resolved {
		out.Status = "quarantined"
	}
	return out, nil
}

func bookCostEvidence(ctx context.Context, tx pgx.Tx, r domain.Reservation, scopes *costScopes, evidence domain.CostEvidence, actionState, key, hash, kind string) (domain.Reservation, error) {
	reserveDelta, spentDelta := costDeltas(r, evidence.ActualMicroUSD)
	incidentKind := ""
	if r.Status == "released" && evidence.ActualMicroUSD > 0 {
		incidentKind = "late_charge_after_release"
	} else if actionState == "pending" && evidence.ActualMicroUSD > 0 {
		incidentKind = "charge_without_dispatch"
	} else if evidence.ActualMicroUSD > r.AmountMicroUSD {
		incidentKind = "price_bound_exceeded"
	}
	if e := applyCostDelta(ctx, tx, scopes, reserveDelta, spentDelta, incidentKind != ""); e != nil {
		return domain.Reservation{}, e
	}
	if incidentKind != "" {
		if e := incident(ctx, tx, r, incidentKind, evidence.ActualMicroUSD); e != nil {
			return domain.Reservation{}, e
		}
	}
	updated, e := scanReservation(tx.QueryRow(ctx, "UPDATE reservations SET status='settled',actual_microusd=$3,revision=revision+1,pending_quarantine_id=NULL WHERE organization_id=$1 AND id=$2 RETURNING "+reservationColumns, r.OrganizationID, r.ID, evidence.ActualMicroUSD))
	if e != nil {
		return domain.Reservation{}, e
	}
	if _, e = tx.Exec(ctx, "UPDATE actions SET status='confirmed' WHERE organization_id=$1 AND id=$2", r.OrganizationID, r.ActionID); e != nil {
		return domain.Reservation{}, e
	}
	if e = recordLedger(ctx, tx, updated, key, hash, kind, reserveDelta, spentDelta); e != nil {
		return domain.Reservation{}, e
	}
	if e = emit(ctx, tx, r.OrganizationID, r.TaskID, "cost."+kind, map[string]any{"reservation_id": r.ID, "actual_microusd": strconv.FormatInt(evidence.ActualMicroUSD, 10), "incident": incidentKind != ""}); e != nil {
		return domain.Reservation{}, e
	}
	return updated, nil
}

// ResolveCostQuarantine uses only the persisted fact and base snapshot. A retry
// while capacity is still insufficient is a read-only acknowledged quarantine.
// Resolution never clears financial holds or grants execution authority.
func (s *Store) ResolveCostQuarantine(ctx context.Context, org, qid string, expectedRevision int) (domain.AccountingOutcome, error) {
	var empty domain.AccountingOutcome
	if !validText(qid, 128) || expectedRevision < 1 {
		return empty, domain.Err("invalid_request", "Нужны карантин и его версия", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback(ctx)
	var reservationID string
	if e = tx.QueryRow(ctx, "SELECT reservation_id FROM cost_quarantines WHERE organization_id=$1 AND id=$2", org, qid).Scan(&reservationID); e != nil {
		return empty, rowExists(e)
	}
	r, scopes, e := lockReservation(ctx, tx, org, reservationID)
	if e != nil {
		return empty, e
	}
	var actionState string
	if e = tx.QueryRow(ctx, "SELECT status FROM actions WHERE organization_id=$1 AND id=$2 FOR UPDATE", org, r.ActionID).Scan(&actionState); e != nil {
		return empty, e
	}
	var evidence domain.CostEvidence
	var evidenceID, hash, baseStatus, kind string
	var revision int
	var baseActual *int64
	e = tx.QueryRow(ctx, `SELECT q.evidence_id,q.revision,q.base_status,q.base_actual_microusd,q.requested_kind,e.request_hash,e.source,e.reference,e.payload_digest,e.actual_microusd,e.reason,e.final,e.currency
 FROM cost_quarantines q JOIN cost_evidence e ON e.organization_id=q.organization_id AND e.id=q.evidence_id AND e.reservation_id=q.reservation_id
 WHERE q.organization_id=$1 AND q.id=$2 FOR UPDATE OF q`, org, qid).Scan(&evidenceID, &revision, &baseStatus, &baseActual, &kind, &hash, &evidence.Source, &evidence.Reference, &evidence.PayloadDigest, &evidence.ActualMicroUSD, &evidence.Reason, &evidence.Final, &evidence.Currency)
	if e != nil {
		return empty, rowExists(e)
	}
	if revision != expectedRevision {
		return empty, domain.Err("conflict", "Версия карантина изменилась", 409)
	}
	out, e := accountingOutcomeTx(ctx, tx, r, evidenceID)
	if e != nil {
		return empty, e
	}
	if out.Status == "settled" {
		if e = tx.Commit(ctx); e != nil {
			return empty, e
		}
		return out, nil
	}
	wantStatus := baseStatus
	if wantStatus == "reserved" {
		wantStatus = "unknown"
	}
	if r.PendingQuarantineID != qid || r.Revision != revision || r.Status != wantStatus || !sameAmount(r.ActualMicroUSD, baseActual) {
		return empty, domain.Err("conflict", "Состояние расхода изменилось после карантина", 409)
	}
	reserveDelta, spentDelta := costDeltas(r, evidence.ActualMicroUSD)
	if preflightCostDelta(scopes, reserveDelta, spentDelta) != nil {
		if e = tx.Commit(ctx); e != nil {
			return empty, e
		}
		return out, nil
	}
	r, e = bookCostEvidence(ctx, tx, r, &scopes, evidence, actionState, "resolve:"+qid, hash, "quarantine_resolution")
	if e != nil {
		return empty, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO cost_quarantine_resolutions(organization_id,id,quarantine_id,reservation_id,evidence_id,resulting_revision) VALUES($1,$2,$3,$4,$5,$6)", org, domain.NewID("qres"), qid, r.ID, evidenceID, r.Revision); e != nil {
		return empty, e
	}
	out = domain.AccountingOutcome{Status: "settled", EvidenceID: evidenceID, QuarantineID: qid, Reservation: r}
	if e = tx.Commit(ctx); e != nil {
		return empty, e
	}
	return out, nil
}

func sameAmount(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

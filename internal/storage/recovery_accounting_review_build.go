// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"
)

func (s *Store) buildRecoveryAccountingReviewTx(ctx context.Context, tx pgx.Tx, in recoveryAccountingInputs, views [3]recoveryReviewSnapshot, work *recoveryReviewWork, p *RecoveryAccountingReview) error {
	block := func(code string, key recoveryCostKey) {
		p.Blockers = append(p.Blockers, RecoveryFinancialBlocker{Code: code, OrganizationID: key.org, ReservationID: key.id})
	}
	orgs := map[string]bool{}
	keys := map[recoveryCostKey]bool{}
	for _, v := range views {
		for id := range v.organizations {
			orgs[id] = true
		}
		for key := range v.histories {
			keys[key] = true
		}
	}
	if len(orgs) > recoveryPlanMaxItems || len(keys) > recoveryPlanMaxItems {
		return ErrRecoveryAccounting
	}
	ids := make([]string, 0, len(orgs))
	for id := range orgs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p.Organizations = append(p.Organizations, RecoveryAccountingReviewOrganization{OrganizationID: id, Original: views[0].organizations[id], Captured: views[1].organizations[id], Target: views[2].organizations[id]})
		if views[0].organizations[id] == nil {
			block("late_only_organization", recoveryCostKey{org: id})
		} else if views[1].organizations[id] == nil {
			block("source_organization_missing", recoveryCostKey{org: id})
		}
	}
	ordered := make([]recoveryCostKey, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].org != ordered[j].org {
			return ordered[i].org < ordered[j].org
		}
		return ordered[i].id < ordered[j].id
	})
	rows, err := tx.Query(ctx, "SELECT id,ordinal,organization_id,reservation_id,request_sha256,plan_sha256 FROM "+pgx.Identifier{s.runtimeSchema, "recovery_accounting_batches"}.Sanitize()+" ORDER BY ordinal LIMIT 10001")
	if err != nil {
		return err
	}
	receiptIndex := map[string]int{}
	for rows.Next() {
		r := RecoveryAccountingReviewReceipt{Attribution: "unattributed"}
		if rows.Scan(&r.OperationID, &r.Ordinal, &r.OrganizationID, &r.ReservationID, &r.RequestSHA256, &r.PlanSHA256) != nil || len(p.Receipts) >= recoveryPlanMaxItems || r.Ordinal != int64(len(p.Receipts)+1) {
			rows.Close()
			return ErrRecoveryAccounting
		}
		receiptIndex[r.OperationID] = len(p.Receipts)
		p.Receipts = append(p.Receipts, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if int64(len(p.Receipts)) != p.HeadOrdinal || (len(p.Receipts) > 0 && p.Receipts[len(p.Receipts)-1].OperationID != p.HeadID) {
		return ErrRecoveryAccounting
	}
	claim := func(key recoveryCostKey, m recoveryAccountingSourceMapping, kind string) error {
		index, ok := receiptIndex[m.operationID]
		if !ok {
			return ErrRecoveryAccounting
		}
		r := &p.Receipts[index]
		if r.Attribution != "unattributed" || r.OrganizationID != key.org || r.ReservationID != key.id || r.RequestSHA256 != m.requestSHA256 || r.PlanSHA256 != m.planSHA256 {
			return ErrRecoveryAccounting
		}
		r.Attribution = kind
		return nil
	}
	for _, key := range ordered {
		old, late, current := views[0].histories[key], views[1].histories[key], views[2].histories[key]
		row := RecoveryAccountingReviewReservation{OrganizationID: key.org, ReservationID: key.id}
		if old != nil {
			row.Original = &old.position
		}
		if late != nil {
			row.Captured = &late.position
		}
		if current != nil {
			row.Target = &current.position
		}
		p.Reservations = append(p.Reservations, row)
		if current != nil {
			if current.history.status == "unknown" {
				block("target_unknown_outcome", key)
			}
			if current.history.pending != "" {
				block("target_pending_quarantine", key)
			}
		}
		supported := old != nil && late != nil && current != nil && old.supported && late.supported && current.supported
		if old == nil {
			block("late_only_reservation", key)
		} else if late == nil {
			block("source_scope_missing", key)
		} else if !supported {
			block("unsupported_financial_history", key)
		}
		if old != nil && late != nil {
			if old.position.ImmutableSHA256 != late.position.ImmutableSHA256 {
				return ErrRecoveryAccounting
			}
			for id, hash := range old.immutableRows {
				if late.immutableRows[id] != hash {
					return ErrRecoveryAccounting
				}
			}
		}
		var transitions []recoveryFinancialSourceTransition
		if supported {
			var valid bool
			transitions, valid = recoveryCostContinuationTransitions(ctx, old.history, late.history, &work.cost)
			if !valid || work.cost.hashes > recoveryCostMaxHashWork {
				return ErrRecoveryAccounting
			}
		}
		mappings := map[string]recoveryAccountingSourceMapping{}
		targetBySource := map[string]*recoveryCostEvidence{}
		seenTarget := map[string]bool{}
		firstMissing := ""
		if old != nil && current != nil {
			for id := range old.history.evidence {
				e := current.history.evidence[id]
				if e == nil || old.evidenceRows[id] != current.evidenceRows[id] {
					return ErrRecoveryAccounting
				}
				targetBySource[id] = e
				seenTarget[e.id] = true
			}
		}
		if supported {
			byReference := map[[2]string]*recoveryCostEvidence{}
			for _, e := range current.history.evidence {
				byReference[[2]string{e.value.Source, e.value.Reference}] = e
			}
			for _, step := range transitions {
				if step.kind != "fact" {
					continue
				}
				e := byReference[[2]string{step.evidence.value.Source, step.evidence.value.Reference}]
				if e == nil {
					if firstMissing == "" {
						firstMissing = step.evidence.id
					}
					continue
				}
				if firstMissing != "" {
					return ErrRecoveryAccounting
				}
				source := recoveryReviewSource(old, late, transitions, step.evidence.id)
				m, e2 := s.recoveryAccountingMappingTx(ctx, tx, in, source, step, e)
				if e2 != nil {
					return e2
				}
				if err = claim(key, m, "source_fact"); err != nil {
					return err
				}
				mappings[step.evidence.id] = m
				targetBySource[step.evidence.id] = e
				seenTarget[e.id] = true
			}
		}
		sourceView := late
		if sourceView == nil {
			sourceView = old
		}
		if sourceView != nil {
			for id, e := range sourceView.history.evidence {
				f := RecoveryAccountingReviewFact{OrganizationID: key.org, ReservationID: key.id, SourceEvidenceID: id, SourceRowSHA256: sourceView.evidenceRows[id], Origin: "captured", Kind: e.kind, ActualMicroUSD: e.value.ActualMicroUSD, Recording: "not_recorded", TargetQuarantineState: "none", NextStep: "unsupported_history"}
				if old != nil && old.history.evidence[id] != nil {
					f.Origin = "original"
				}
				if e.q != nil {
					f.SourceQuarantineID = e.q.id
				}
				if old == nil {
					f.NextStep = "unmapped_scope"
				}
				target := targetBySource[id]
				if target != nil {
					f.TargetEvidenceID = target.id
					f.Recording = "imported"
					f.NextStep = "none"
					if !supported {
						f.NextStep = "unsupported_history"
					}
					if m, ok := mappings[id]; ok {
						f.Recording = "recorded"
						f.FactOperationID = m.operationID
					}
					if target.q != nil {
						z := target.q
						f.TargetQuarantineID = z.id
						f.TargetQuarantineState = "pending"
						if z.resolution == nil {
							if current.history.pending != z.id {
								return ErrRecoveryAccounting
							}
							if supported {
								f.NextStep = "prepare_resolution_plan"
							}
							if recoverySemanticCatalogSHA256 != recoveryAccountingSourceV2CatalogSHA256 {
								f.NextStep = "unsupported_catalog"
							}
						} else {
							f.TargetQuarantineState = "resolved"
							f.ResolutionOrigin = "unattributed"
							originalEvidence := old.history.evidence[id]
							if originalEvidence != nil && originalEvidence.q != nil && originalEvidence.q.resolution != nil {
								f.ResolutionOrigin = "original"
							} else if supported {
								source := recoveryReviewSource(old, late, transitions, id)
								provenance := recoveryAccountingResolutionProvenance{evidenceID: target.id, evidenceSHA: current.evidenceRows[target.id], quarantineSHA: current.quarantineRows[z.id], origin: "original-snapshot"}
								if m, ok := mappings[id]; ok {
									provenance.origin, provenance.factOperation, provenance.factPlan = "captured-fact", m.operationID, m.planSHA256
								} else if originalEvidence == nil || originalEvidence.q == nil || originalEvidence.q.id != z.id || source.originalEvidenceSHA256 != provenance.evidenceSHA || source.originalQuarantineSHA256 != provenance.quarantineSHA {
									return ErrRecoveryAccounting
								}
								request := RecoveryAccountingSelection{Mode: "resolve", OrganizationID: key.org, ReservationID: key.id, SourceEvidenceID: id, QuarantineID: z.id}
								m, e2 := s.recoveryReviewResolutionMappingTx(ctx, tx, in, source, provenance, request, z)
								if e2 != nil {
									return e2
								}
								if err = claim(key, m, "source_resolution"); err != nil {
									return err
								}
								f.ResolutionOrigin, f.ResolutionOperationID = "recovery", m.operationID
							}
						}
					}
				} else if supported {
					f.NextStep = "await_predecessor"
					if id == firstMissing {
						f.NextStep = "prepare_fact_plan"
						if current.history.pending != "" {
							f.NextStep = "resolve_pending_quarantine"
						} else if (e.kind == "adjustment" && current.history.status != "settled") || (e.kind != "adjustment" && current.history.status == "settled") {
							f.NextStep = "unsupported_target_state"
						}
						if recoverySemanticCatalogSHA256 != recoveryAccountingSourceV2CatalogSHA256 {
							f.NextStep = "unsupported_catalog"
						}
					}
				}
				p.Facts = append(p.Facts, f)
			}
		}
		if current != nil {
			for id, e := range current.history.evidence {
				if seenTarget[id] {
					continue
				}
				// Unsupported histories remain visible, but do not manufacture an
				// authenticated source-to-target mapping merely because amounts match.
				f := RecoveryAccountingReviewFact{OrganizationID: key.org, ReservationID: key.id, Origin: "target_unattributed", Kind: e.kind, ActualMicroUSD: e.value.ActualMicroUSD, Recording: "unattributed", TargetEvidenceID: id, TargetQuarantineState: "none", NextStep: "unsupported_history"}
				if e.q != nil {
					f.TargetQuarantineID = e.q.id
					f.TargetQuarantineState = "pending"
					if e.q.resolution != nil {
						f.TargetQuarantineState = "resolved"
					}
				}
				p.Facts = append(p.Facts, f)
				block("unattributed_target_fact", key)
			}
		}
		if len(p.Facts) > recoveryPlanMaxItems || len(p.Blockers) > recoveryPlanMaxItems || ctx.Err() != nil {
			return ErrRecoveryAccounting
		}
	}
	unattributed := false
	for _, r := range p.Receipts {
		if r.Attribution == "unattributed" {
			unattributed = true
			block("unattributed_accounting_operation", recoveryCostKey{r.OrganizationID, r.ReservationID})
		}
	}
	if unattributed {
		for i := range p.Facts {
			if p.Facts[i].NextStep == "prepare_fact_plan" || p.Facts[i].NextStep == "prepare_resolution_plan" {
				p.Facts[i].NextStep = "review_unattributed_operations"
			}
		}
	}
	if len(p.Blockers) > recoveryPlanMaxItems {
		return ErrRecoveryAccounting
	}
	return nil
}

// Independently reconstruct the immutable resolution commitment for the review.
// Its caller has checked the entire source prefix and original/target row hashes.
func (s *Store) recoveryReviewResolutionMappingTx(ctx context.Context, tx pgx.Tx, in recoveryAccountingInputs, source recoveryAccountingSource, provenance recoveryAccountingResolutionProvenance, request RecoveryAccountingSelection, z *recoveryCostQuarantine) (m recoveryAccountingSourceMapping, err error) {
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	rows, err := tx.Query(ctx, `SELECT b.id,COALESCE(b.previous_id,''),b.request_sha256,b.plan_sha256,b.expected_revision,b.mutation_profile,
 encode(sha256(convert_to(r.before_row::text,'UTF8')),'hex'),(r.before_row->>'revision')::bigint,r.before_row->>'pending_quarantine_id',
 b.restore_id=$1 AND b.target_installation_id=$2 AND b.evidence_batch_id=$3 AND b.organization_id=$4 AND b.reservation_id=$5
 FROM `+q("recovery_accounting_batches")+` b JOIN `+q("recovery_accounting_mutations")+` z ON z.batch_id=b.id AND z.table_name='cost_quarantine_resolutions' AND z.operation='INSERT'
 JOIN `+q("recovery_accounting_mutations")+` r ON r.batch_id=b.id AND r.table_name='reservations' AND r.operation='UPDATE'
 WHERE z.after_row->>'organization_id'=$4 AND z.after_row->>'reservation_id'=$5 AND z.after_row->>'quarantine_id'=$6 AND z.after_row->>'evidence_id'=$7 LIMIT 2`, in.held.RestoreID, in.held.InstallationID, in.verified.Link().OperationID, request.OrganizationID, request.ReservationID, z.id, provenance.evidenceID)
	if err != nil {
		return m, ErrRecoveryAccounting
	}
	defer rows.Close()
	var beforeSHA, pending, profile string
	var revision int64
	var bound bool
	if !rows.Next() || rows.Scan(&m.operationID, &m.previousID, &m.requestSHA256, &m.planSHA256, &m.revision, &profile, &beforeSHA, &revision, &pending, &bound) != nil || !bound || rows.Next() || rows.Err() != nil || revision != int64(z.revision) || m.revision != revision || pending != z.id || m.requestSHA256 != recoveryAccountingResolutionHash(request.OrganizationID, request.ReservationID, z.id, z.revision) || m.planSHA256 != recoveryAccountingResolutionPlanHash(in, source, provenance, request, m.operationID, beforeSHA, m.previousID, m.requestSHA256, revision, profile) {
		return recoveryAccountingSourceMapping{}, ErrRecoveryAccounting
	}
	return m, nil
}

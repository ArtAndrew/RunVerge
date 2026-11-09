// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"sort"

	"agentplatform.local/platform/internal/recoveryarchive"
)

type RecoveryPeerWindowReview struct {
	Scope             recoveryarchive.RecoveryPeerScope           `json:"scope"`
	JournalSHA256     string                                      `json:"journal_sha256"`
	SignerSHA256      string                                      `json:"signer_sha256"`
	EgressProofSHA256 string                                      `json:"egress_proof_sha256"`
	CoveredThroughUTC string                                      `json:"covered_through_utc"`
	CoverageProven    bool                                        `json:"coverage_proven"`
	Organizations     []RecoveryPeerOrganization                  `json:"organizations"`
	Blockers          []RecoveryPeerWindowBlocker                 `json:"blockers"`
	Unmatched         []recoveryarchive.RecoveryPeerJournalRecord `json:"unmatched"`
}

type RecoveryPeerOrganization struct {
	OrganizationID   string `json:"organization_id"`
	CoverageProven   bool   `json:"coverage_proven"`
	FinancialHold    bool   `json:"financial_hold"`
	BoundLiabilities int    `json:"bound_liabilities"`
}

type RecoveryPeerWindowBlocker struct {
	OrganizationID string `json:"organization_id,omitempty"`
	Family         string `json:"family"`
	RecordID       string `json:"record_id"`
	Code           string `json:"code"`
}

// Reconcile against the CURRENT, verified accounting view from the same
// target transaction. Neither peer dispositions nor missing local markers
// are financial facts. Unmatched evidence holds its known organization or
// the entire restore when its scope cannot be attributed.
func reviewRecoveryPeerWindow(peer *recoveryarchive.VerifiedRecoveryPeerJournal, window RecoveryExecutionWindow, view recoveryReviewSnapshot) RecoveryPeerWindowReview {
	j := peer.Journal()
	result := RecoveryPeerWindowReview{
		Scope: j.Scope, JournalSHA256: peer.SHA256(), SignerSHA256: peer.SignerSHA256(), EgressProofSHA256: j.EgressProofSHA256,
		CoveredThroughUTC: j.CoveredThroughUTC, Organizations: []RecoveryPeerOrganization{},
		Blockers: []RecoveryPeerWindowBlocker{}, Unmatched: []recoveryarchive.RecoveryPeerJournalRecord{},
	}
	observed := make(map[recoveryWindowKey]recoveryarchive.RecoveryPeerJournalRecord, len(j.Records))
	for _, r := range j.Records {
		observed[recoveryWindowKey{r.Family, r.OrganizationID, r.RecordID}] = r
	}
	modelActions := map[recoveryCostKey][]recoveryWindowKey{}
	for _, r := range window.Records {
		if r.Family == "action" && r.Captured != nil && r.Captured.Kind == "model" && r.Captured.DispatchRecorded {
			task := recoveryCostKey{r.OrganizationID, r.Captured.TaskID}
			modelActions[task] = append(modelActions[task], recoveryWindowKey{r.Family, r.OrganizationID, r.RecordID})
		}
	}
	blocked := map[string]bool{}
	block := func(k recoveryWindowKey, code string) {
		org := k.org
		if view.organizations[org] == nil {
			org = ""
		}
		blocked[org] = true
		result.Blockers = append(result.Blockers, RecoveryPeerWindowBlocker{org, k.family, k.id, code})
	}
	used := map[recoveryWindowKey]bool{}
	liabilities := map[recoveryCostKey]bool{}
	for _, r := range window.Records {
		k := recoveryWindowKey{r.Family, r.OrganizationID, r.RecordID}
		if r.Comparison == "missing_from_capture" || r.Comparison == "identity_changed" || r.Comparison == "dispatch_record_changed" {
			block(k, "local_history_changed")
			continue
		}
		c := r.Captured
		if c == nil || view.organizations[k.org] == nil {
			block(k, "local_scope_unverified")
			continue
		}
		if !c.DispatchRecorded {
			if recoveryPeerMissingDispatch(k.family, *c) {
				block(k, "local_dispatch_identity_unverified")
			}
			continue
		}
		fact, exists := observed[k]
		switch {
		case !exists:
			block(k, "peer_disposition_missing")
		case fact.DispatchSHA256 != c.DispatchSHA256:
			block(k, "peer_dispatch_identity_mismatch")
		default:
			used[k] = true
			if fact.Disposition == "unknown" {
				block(k, "external_effect_unresolved")
			}
			if k.family == "tool_execution" && !recoveryPeerToolEffectAgrees(observed, fact, *c) {
				block(k, "peer_tool_action_disagreement")
			}
			if k.family == "configuration_verification" && c.Kind == "model" {
				actions := modelActions[recoveryCostKey{k.org, c.TaskID}]
				if len(actions) != 1 || !recoveryPeerSameEffect(observed[actions[0]], fact) {
					block(k, "peer_model_probe_disagreement")
				}
			}
		}
		if k.family == "action" {
			cost, open, valid := recoveryPeerActionCost(k, *c, view)
			if !valid {
				block(k, "dispatch_liability_unverified")
			} else if open {
				liabilities[cost] = true
			}
		}
	}
	for _, fact := range j.Records {
		k := recoveryWindowKey{fact.Family, fact.OrganizationID, fact.RecordID}
		if !used[k] {
			result.Unmatched = append(result.Unmatched, fact)
			block(k, "unmatched_external_evidence")
		}
	}
	counts := map[string]int{}
	for k := range liabilities {
		counts[k.org]++
	}
	for org, account := range view.organizations {
		result.Organizations = append(result.Organizations, RecoveryPeerOrganization{
			OrganizationID: org, CoverageProven: !blocked[""] && !blocked[org], FinancialHold: account.FinancialHold, BoundLiabilities: counts[org],
		})
	}
	sort.Slice(result.Organizations, func(i, j int) bool {
		return result.Organizations[i].OrganizationID < result.Organizations[j].OrganizationID
	})
	result.CoverageProven = len(result.Blockers) == 0
	return result
}

func recoveryPeerMissingDispatch(family string, p RecoveryExecutionWindowPosition) bool {
	switch family {
	case "action":
		return p.Status == "sent" || p.Status == "confirmed" || p.Status == "unknown"
	case "tool_execution":
		return p.Status == "sent" || p.Status == "succeeded" || p.Status == "unknown"
	case "configuration_verification":
		return p.Status == "sent" || p.Status == "response_received" || p.Status == "unknown"
	case "webhook_attempt":
		return p.Status == "sent" || p.Status == "acknowledged" || p.Status == "not_acknowledged" || p.Status == "unknown"
	case "reservation_without_action":
		return p.Kind != "fixture_legacy"
	default:
		return true
	}
}

func recoveryPeerToolEffectAgrees(peer map[recoveryWindowKey]recoveryarchive.RecoveryPeerJournalRecord, fact recoveryarchive.RecoveryPeerJournalRecord, p RecoveryExecutionWindowPosition) bool {
	if len(p.RelatedIDs) != 2 {
		return false
	}
	action, ok := peer[recoveryWindowKey{"action", fact.OrganizationID, p.RelatedIDs[0]}]
	return ok && action.DispatchSHA256 == p.DispatchSHA256 && recoveryPeerSameEffect(action, fact)
}

func recoveryPeerSameEffect(a, b recoveryarchive.RecoveryPeerJournalRecord) bool {
	return a.Disposition == b.Disposition && a.EvidenceSHA256 == b.EvidenceSHA256
}

func recoveryPeerActionCost(key recoveryWindowKey, p RecoveryExecutionWindowPosition, view recoveryReviewSnapshot) (recoveryCostKey, bool, bool) {
	if len(p.RelatedIDs) != 1 {
		return recoveryCostKey{}, false, false
	}
	k := recoveryCostKey{key.org, p.RelatedIDs[0]}
	h := view.histories[k]
	if h == nil || h.history == nil || !h.supported || h.history.ledger["dispatch"] == nil || h.position.Category != p.Kind ||
		h.position.TaskID != p.TaskID || h.position.OperationID != p.OperationID {
		return k, false, false
	}
	switch h.position.Status {
	case "reserved", "unknown":
		return k, true, true
	case "settled":
		return k, false, true
	default:
		return k, false, false
	}
}

// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryUsageBinding = errors.New("provider usage request cannot be bound to verified recovery history")
var recoveryUsageGenerationID = regexp.MustCompile(`^gen-[0-9A-Za-z-]+$`)

// Select only local history. A generation ID, credential, amount or provider
// account supplied by the operator cannot replace authenticated source rows.
type RecoveryUsageSelection struct {
	OrganizationID string `json:"organization_id"`
	ReservationID  string `json:"reservation_id"`
}

// This plan proves a source-record binding, not that its provider ID is genuine,
// belongs to the current credential, or has a final cost. It authorizes no I/O.
type RecoveryUsageReadbackPlan struct {
	Version                 int                    `json:"version"`
	Algorithm               string                 `json:"algorithm"`
	PlanSHA256              string                 `json:"plan_sha256"`
	SourceID                string                 `json:"source_id"`
	TargetID                string                 `json:"target_id"`
	RestoreID               string                 `json:"restore_id"`
	SelectedReceiptSHA256   string                 `json:"selected_receipt_sha256"`
	CapturedReceiptSHA256   string                 `json:"captured_receipt_sha256"`
	SelectedSnapshotSHA256  string                 `json:"selected_snapshot_sha256"`
	CapturedSnapshotSHA256  string                 `json:"captured_snapshot_sha256"`
	OriginalReceiptSHA256   string                 `json:"original_receipt_sha256,omitempty"`
	OriginalSnapshotSHA256  string                 `json:"original_snapshot_sha256,omitempty"`
	ColdActivationID        string                 `json:"cold_activation_id,omitempty"`
	CaptureLinkSHA256       string                 `json:"capture_link_sha256"`
	TargetCatalogSHA256     string                 `json:"target_catalog_sha256"`
	HeadID                  string                 `json:"head_id"`
	Selection               RecoveryUsageSelection `json:"selection"`
	TaskID                  string                 `json:"task_id"`
	RunID                   string                 `json:"run_id"`
	OperationID             string                 `json:"operation_id"`
	ActionID                string                 `json:"action_id"`
	IntentSHA256            string                 `json:"intent_sha256"`
	ReservationIdentitySHA  string                 `json:"reservation_identity_sha256"`
	ActionIdentitySHA       string                 `json:"action_identity_sha256"`
	DispatchReceiptSHA256   string                 `json:"dispatch_receipt_sha256"`
	ModelTurnSHA256         string                 `json:"model_turn_sha256"`
	ConnectionIdentitySHA   string                 `json:"connection_identity_sha256"`
	ModelConnectionID       string                 `json:"model_connection_id"`
	ModelConnectionVersion  int                    `json:"model_connection_version"`
	ArchivedProviderAccount string                 `json:"archived_provider_account"`
	Model                   string                 `json:"model"`
	CandidateSHA256         string                 `json:"candidate_sha256"`
	ArtifactSHA256          string                 `json:"artifact_sha256"`
	CandidateOrigin         string                 `json:"candidate_origin"`
	State                   string                 `json:"state"`
	GenerationID            string                 `json:"generation_id,omitempty"`
	ExecutionHeld           bool                   `json:"execution_held"`
	RollbackConfirmed       bool                   `json:"rollback_confirmed"`
}

type recoveryUsageAnchor struct {
	task, run, operation, action, intent              string
	reservationSHA, actionSHA, turnSHA, connectionSHA string
	dispatchSHA                                       string
	connection                                        domain.ModelConnection
	candidateSHA, artifactSHA, generation             string
}

func (s *Store) PlanRecoveryUsageReadback(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection) (result RecoveryUsageReadbackPlan, resultErr error) {
	if !limits.valid() || !validText(selection.OrganizationID, 128) || !validText(selection.ReservationID, 128) {
		return result, ErrRecoveryUsageBinding
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	evidence, err := s.bindRecoveryUsageEvidence(ctx, selected, late, verified, spec, limits)
	if err != nil {
		return result, err
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, ErrRecoveryUsageBinding
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, ErrRecoveryUsageBinding
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if tx.Rollback(cleanup) != nil {
			_ = conn.Conn().Close(cleanup)
			result, resultErr = RecoveryUsageReadbackPlan{}, ErrRecoveryStageCleanup
			return
		}
		if resultErr != nil {
			result = RecoveryUsageReadbackPlan{}
			return
		}
		result.RollbackConfirmed = true
		result.PlanSHA256 = digestValue(result)
	}()
	return s.planRecoveryUsageReadbackTx(ctx, tx, selected, late, verified, spec, limits, selection, evidence)
}

// The caller owns the transaction and its outcome. This builder does not report
// rollback or seal a display digest; the public planning wrapper does that only
// after successful rollback. Durable admission separately reconstructs the
// acquired plan's commitment and never reports its write transaction as rolled back.
func (s *Store) planRecoveryUsageReadbackTx(ctx context.Context, tx pgx.Tx, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection, evidence recoveryUsageEvidence) (result RecoveryUsageReadbackPlan, resultErr error) {
	baseArchive, baseline := selected, evidence.original
	if evidence.cold {
		baseArchive, baseline = late, evidence.captured
	}
	held, catalog, err := s.evidenceTargetTx(ctx, tx, spec, baseline.SHA256)
	if err != nil {
		return result, err
	}
	var cold recoveryColdUsageAnchor
	if evidence.cold {
		cold, err = s.recoveryColdUsageAnchorTx(ctx, tx, spec, verified)
	} else {
		_, err = s.inspectRecoveryEvidenceTx(ctx, tx, spec, verified)
	}
	if err != nil {
		return result, err
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		return result, err
	}
	base, err := recoverySourceInventory(inventory, baseline.InventorySHA256)
	if err != nil {
		return result, err
	}
	head, err := s.verifyRecoveryAccountingBaselineTx(ctx, tx, baseArchive, baseline, limits, held, catalog, base)
	if err != nil {
		return result, err
	}
	var anchors [2]recoveryUsageAnchor
	originalPresent := true
	// Read captured first so the original snapshot can detect an ID reused by
	// another lineage even when our candidate did not exist at the original cut.
	for i, archive := range []*recoveryarchive.VerifiedArchive{late, selected} {
		snapshot, index := evidence.captured, 1
		if i == 1 {
			snapshot, index = evidence.original, 0
		}
		inv, err := recoverySourceInventory(inventory, snapshot.InventorySHA256)
		if err != nil {
			return result, err
		}
		staged, err := s.stageRecoveryTypedDataTx(ctx, tx, archive, snapshot, limits, held, catalog, inv)
		if err != nil {
			return result, err
		}
		if index == 0 && evidence.cold {
			err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{staged.schema, "reservations"}.Sanitize()+
				" WHERE organization_id=$1 AND id=$2)", selection.OrganizationID, selection.ReservationID).Scan(&originalPresent)
			if err != nil {
				return result, ErrRecoveryUsageBinding
			}
		}
		if index == 0 && !originalPresent {
			// This operation may have begun after the earlier cut. Its exact
			// captured reservation is authoritative, but an earlier reuse of
			// the same account/generation still makes attribution ambiguous.
			if b := anchors[1]; b.generation != "" {
				err = recoveryUsageUniqueTx(ctx, tx, staged.schema, selection, b.connection.ProviderAccount, b.generation)
			}
		} else {
			anchors[index], err = readRecoveryUsageAnchorTx(ctx, tx, staged.schema, selection, anchors[1].generation)
		}
		if err != nil {
			return result, err
		}
		if err = dropRecoveryAccountingStageTx(ctx, tx, staged, snapshot); err != nil {
			return result, err
		}
	}
	a, b := anchors[0], anchors[1]
	if (originalPresent && (a.reservationSHA != b.reservationSHA || a.actionSHA != b.actionSHA || a.turnSHA != b.turnSHA || a.connectionSHA != b.connectionSHA ||
		(a.dispatchSHA != "" && a.dispatchSHA != b.dispatchSHA) || (a.candidateSHA != "" && (a.candidateSHA != b.candidateSHA || a.artifactSHA != b.artifactSHA)))) || ctx.Err() != nil {
		return result, ErrRecoveryUsageBinding
	}
	l := verified.Link()
	result = RecoveryUsageReadbackPlan{Version: 1, Algorithm: "archived-model-usage-binding-v1", SourceID: held.SourceID, TargetID: held.InstallationID, RestoreID: held.RestoreID,
		SelectedReceiptSHA256: l.SelectedReceiptSHA256, CapturedReceiptSHA256: l.EvidenceReceiptSHA256, SelectedSnapshotSHA256: evidence.original.SHA256, CapturedSnapshotSHA256: evidence.captured.SHA256,
		CaptureLinkSHA256: verified.SHA256(), TargetCatalogSHA256: recoverySemanticCatalogSHA256, HeadID: head.ID, Selection: selection,
		TaskID: b.task, RunID: b.run, OperationID: b.operation, ActionID: b.action, IntentSHA256: b.intent,
		ReservationIdentitySHA: b.reservationSHA, ActionIdentitySHA: b.actionSHA, DispatchReceiptSHA256: b.dispatchSHA, ModelTurnSHA256: b.turnSHA, ConnectionIdentitySHA: b.connectionSHA,
		ModelConnectionID: b.connection.ID, ModelConnectionVersion: b.connection.Version, ArchivedProviderAccount: b.connection.ProviderAccount, Model: b.connection.Model,
		CandidateSHA256: b.candidateSHA, ArtifactSHA256: b.artifactSHA, CandidateOrigin: "none", State: "request_identity_unavailable", ExecutionHeld: true}
	if evidence.cold {
		result.OriginalReceiptSHA256, result.OriginalSnapshotSHA256 = l.SelectedReceiptSHA256, evidence.original.SHA256
		result.SelectedReceiptSHA256, result.SelectedSnapshotSHA256 = l.EvidenceReceiptSHA256, evidence.captured.SHA256
		result.ColdActivationID = cold.ActivationID
	}
	if b.candidateSHA != "" {
		result.CandidateOrigin = "captured"
		if a.candidateSHA != "" {
			result.CandidateOrigin = "original"
		}
	}
	if b.connection.WireProfile != domain.ModelWireOpenRouterTextV1 || b.connection.Endpoint != modelgateway.OpenRouterEndpoint {
		result.State = "unsupported_provider_profile"
	} else if b.generation != "" {
		if len(b.generation) <= 128 && recoveryUsageGenerationID.MatchString(b.generation) {
			result.State, result.GenerationID = "request_identity_available", b.generation
		} else {
			result.State = "unsupported_request_identity"
		}
	}
	return result, nil
}

func readRecoveryUsageAnchorTx(ctx context.Context, tx pgx.Tx, schema string, selection RecoveryUsageSelection, capturedGeneration string) (out recoveryUsageAnchor, resultErr error) {
	q := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	hash := func(expr string) string { return "encode(sha256(convert_to((" + expr + ")::text,'UTF8')),'hex')" }
	var profile []byte
	var connectionID, provider, price, model, credential, connectionProvider, connectionPrice string
	var connectionVersion int
	err := tx.QueryRow(ctx, `SELECT r.task_id,o.run_id,r.operation_id,r.action_id,a.intent_hash,t.model_connection_id,t.model_connection_version,c.profile,r.provider,r.price_version,c.model,c.credential_environment,c.provider_account,c.price_version,`+
		hash("to_jsonb(r)-ARRAY['status','actual_microusd','revision','pending_quarantine_id']")+`,`+
		hash("to_jsonb(a)-ARRAY['status','dispatch_issued_at','dispatch_not_after','dispatch_fencing_token']")+`,`+hash("to_jsonb(m)")+`,`+hash("to_jsonb(c)-'active'")+`,COALESCE(`+hash("CASE WHEN a.dispatch_issued_at IS NOT NULL THEN jsonb_build_array(a.dispatch_issued_at,a.dispatch_not_after,a.dispatch_fencing_token) END")+`,'')
 FROM `+q("reservations")+` r JOIN `+q("tasks")+` t ON t.organization_id=r.organization_id AND t.id=r.task_id
 JOIN `+q("operations")+` o ON o.organization_id=r.organization_id AND o.id=r.operation_id AND o.task_id=r.task_id
 JOIN `+q("actions")+` a ON a.organization_id=r.organization_id AND a.id=r.action_id AND a.operation_id=r.operation_id AND a.task_id=r.task_id
 JOIN `+q("model_turns")+` m ON m.organization_id=r.organization_id AND m.operation_id=r.operation_id AND m.task_id=r.task_id AND m.run_id=o.run_id
 JOIN `+q("model_connections")+` c ON c.organization_id=t.organization_id AND c.id=t.model_connection_id AND c.version=t.model_connection_version
 WHERE r.organization_id=$1 AND r.id=$2 AND r.category='model'`, selection.OrganizationID, selection.ReservationID).Scan(&out.task, &out.run, &out.operation, &out.action, &out.intent, &connectionID, &connectionVersion, &profile, &provider, &price, &model, &credential, &connectionProvider, &connectionPrice, &out.reservationSHA, &out.actionSHA, &out.turnSHA, &out.connectionSHA, &out.dispatchSHA)
	if err != nil || decodeRecoveryHistoryJSON(profile, &out.connection, 262144) != nil || out.connection.ID != connectionID || out.connection.Version != connectionVersion || out.connection.OrganizationID != selection.OrganizationID || out.connection.ProviderAccount != provider || out.connection.PriceVersion != price || out.connection.Model != model || out.connection.CredentialEnvironment != credential || connectionProvider != provider || connectionPrice != price {
		return out, ErrRecoveryUsageBinding
	}
	var artifact, candidate, provenance json.RawMessage
	var contentDigest, mediaType, source string
	var artifactVersion, proposalCount int
	err = tx.QueryRow(ctx, `SELECT to_jsonb(c),to_jsonb(a),a.provenance,c.content_digest,a.media_type,a.version,tc.source,
 (SELECT count(*) FROM `+q("model_tool_proposals")+` p WHERE p.organization_id=c.organization_id AND p.operation_id=c.operation_id)
 FROM `+q("model_candidates")+` c JOIN `+q("artifacts")+` a ON a.organization_id=c.organization_id AND a.id=c.artifact_id AND a.task_id=c.task_id
 JOIN `+q("task_candidates")+` tc ON tc.organization_id=c.organization_id AND tc.operation_id=c.operation_id AND tc.task_id=c.task_id AND tc.artifact_id=c.artifact_id AND tc.artifact_version=a.version AND tc.content_digest=c.content_digest
 WHERE c.organization_id=$1 AND c.reservation_id=$2 AND c.task_id=$3 AND c.operation_id=$4`, selection.OrganizationID, selection.ReservationID, out.task, out.operation).Scan(&candidate, &artifact, &provenance, &contentDigest, &mediaType, &artifactVersion, &source, &proposalCount)
	if errors.Is(err, pgx.ErrNoRows) {
		if capturedGeneration != "" && recoveryUsageUniqueTx(ctx, tx, schema, selection, provider, capturedGeneration) != nil {
			return out, ErrRecoveryUsageBinding
		}
		return out, nil
	}
	if err != nil || out.dispatchSHA == "" || len(candidate) > 65536 || len(artifact) > 1<<20 {
		return out, ErrRecoveryUsageBinding
	}
	var p map[string]json.RawMessage
	if decodeRecoveryHistoryJSON(provenance, &p, 262144) != nil {
		return out, ErrRecoveryUsageBinding
	}
	expected := map[string]any{"adapter": "model", "operation_id": out.operation, "reservation_id": selection.ReservationID, "intent_hash": out.intent, "content_digest": contentDigest, "model": out.connection.Model, "price_version": out.connection.PriceVersion, "model_connection_id": connectionID, "model_connection_version": connectionVersion}
	if out.connection.WireProfile == domain.ModelWireOpenRouterTextV1 {
		if source != "model" || mediaType != "text/plain" || artifactVersion != 1 || proposalCount != 0 {
			return out, ErrRecoveryUsageBinding
		}
		expected["proposal_count"] = 0
	}
	for key, value := range expected {
		encoded, _ := json.Marshal(value)
		var compact json.RawMessage
		// Decode and re-encode scalar values to preserve numeric versus string type.
		var actual any
		if decodeRecoveryHistoryJSON(p[key], &actual, 4096) != nil {
			return out, ErrRecoveryUsageBinding
		}
		compact, _ = json.Marshal(actual)
		if string(compact) != string(encoded) {
			return out, ErrRecoveryUsageBinding
		}
	}
	if len(p["provider_request_id"]) == 0 || string(p["provider_request_id"]) == "null" || json.Unmarshal(p["provider_request_id"], &out.generation) != nil {
		return out, ErrRecoveryUsageBinding
	}
	// Hash complete immutable rows, including source timestamps and provenance.
	out.candidateSHA, out.artifactSHA = Hash(candidate), Hash(artifact)
	checkGeneration := out.generation
	if capturedGeneration != "" {
		checkGeneration = capturedGeneration
	}
	if checkGeneration != "" && recoveryUsageUniqueTx(ctx, tx, schema, selection, provider, checkGeneration) != nil {
		return out, ErrRecoveryUsageBinding
	}
	return out, nil
}

func recoveryUsageUniqueTx(ctx context.Context, tx pgx.Tx, schema string, selection RecoveryUsageSelection, provider, generation string) error {
	q := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	var unique bool
	if tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM `+q("model_candidates")+` x
 JOIN `+q("artifacts")+` a ON a.organization_id=x.organization_id AND a.id=x.artifact_id
 JOIN `+q("tasks")+` t ON t.organization_id=x.organization_id AND t.id=x.task_id
 JOIN `+q("model_connections")+` c ON c.organization_id=t.organization_id AND c.id=t.model_connection_id AND c.version=t.model_connection_version
 WHERE c.provider_account=$1 AND a.provenance->>'provider_request_id'=$2 AND (x.organization_id<>$3 OR x.reservation_id<>$4))`, provider, generation, selection.OrganizationID, selection.ReservationID).Scan(&unique) != nil || !unique {
		return ErrRecoveryUsageBinding
	}
	return nil
}

// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/modelruntime"
	"github.com/jackc/pgx/v5"
)

// Keep the outcome hash and response audit coherent. Rejection must come from
// the protocol shape/binding, not merely from a stale evidence digest.
func recoveryConfigurationOutcomeFault(t *testing.T, tx pgx.Tx, change func(map[string]any)) {
	t.Helper()
	var raw []byte
	if err := tx.QueryRow(context.Background(), "SELECT outcome FROM pg_temp.configuration_verifications WHERE status='response_received'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	change(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	digest := digestValue(value)
	if _, err = tx.Exec(context.Background(), "UPDATE pg_temp.configuration_verifications SET outcome=$1,evidence_digest=$2 WHERE status='response_received'", encoded, digest); err != nil {
		t.Fatal("outcome mutation violated prerequisite row constraints", err)
	}
	if _, err = tx.Exec(context.Background(), "UPDATE pg_temp.configuration_audit SET digest=$1 WHERE action='response_received'", digest); err != nil {
		t.Fatal(err)
	}
}

func recoveryConfigurationOutcomeCase(t *testing.T, tx pgx.Tx, mutate func()) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), "SAVEPOINT response_fault"); err != nil {
		t.Fatal(err)
	}
	defer tx.Exec(context.Background(), "ROLLBACK TO SAVEPOINT response_fault")
	mutate()
	if !errors.Is(validateRecoveryConfigurationHistory(context.Background(), tx, "pg_temp"), ErrRecoveryConfigurationHistory) {
		t.Fatal("forged observed outcome accepted")
	}
}

func recoveryConfigurationObservedStage(t *testing.T, source *Store) {
	t.Helper()
	var snapshot bytes.Buffer
	if _, err := source.ExportRecoverySnapshot(context.Background(), &snapshot, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("observed-history export failed", err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, snapshot.Bytes())
	report, err := target.CheckRecoverySnapshotData(context.Background(), recoveryStageAuthorArchive(t, snapshot.Bytes()), spec, DefaultRecoverySnapshotLimits())
	if err != nil || !report.RollbackConfirmed || report.FullSemanticValidation {
		t.Fatal("observed-history full stage failed", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
}

func TestRecoveryConfigurationHistoryAuthorObservedMCP(t *testing.T) {
	recoveryInvariantOwned(t)
	f := configurationAuthorSetup(t)
	ctx := context.Background()
	actor, _ := f.account(t, "historical MCP owner")
	grant := f.grant(t, f.p, actor.ID, nil, 3)
	plan := f.plan(t, actor, grant.ID)
	plan = f.apply(t, actor, plan, f.validate(t, actor, plan))
	verification := f.verify(t, actor, plan, "connect", 0)
	lease, _, err := f.s.ClaimConfigurationVerification(ctx, "recovery_mcp_worker", time.Minute)
	if err != nil || lease == nil {
		t.Fatal("claim failed", err)
	}
	if _, err = f.s.PrepareConfigurationVerificationDispatch(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err = f.s.PauseConfigurationVerification(ctx, lease, "acknowledgement_unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RevokeConfigurationGrant(ctx, f.p, grant.ID, domain.NewID("key"), domain.RevokeConfigurationGrant{ExpectedVersion: grant.Version, Reason: "Revoke before late factual observation"}); err != nil {
		t.Fatal(err)
	}
	observed, err := f.s.RecordConfigurationVerificationObservation(ctx, domain.ConfigurationProbeObservation{OrganizationID: verification.OrganizationID, VerificationID: verification.ID, WorkerID: lease.WorkerID, FencingToken: lease.Token, Outcome: "response_received", ProtocolVersion: mcpbridge.ProtocolVersion, ToolCount: 0, CatalogDigest: Hash([]byte("synthetic empty catalogue"))})
	if err != nil || observed.Status != "response_received" || observed.ToolCount != 0 {
		t.Fatal("late zero-tool observation failed", err)
	}
	tx := recoveryInvariantStage(t, f.s)
	if err = validateRecoveryConfigurationHistory(ctx, tx, "pg_temp"); err != nil {
		t.Fatal("valid observed MCP history rejected", err)
	}
	t.Run("coherent_missing_zero_count", func(t *testing.T) {
		recoveryConfigurationOutcomeCase(t, tx, func() { recoveryConfigurationOutcomeFault(t, tx, func(v map[string]any) { delete(v, "tool_count") }) })
	})
	t.Run("coherent_null_count", func(t *testing.T) {
		recoveryConfigurationOutcomeCase(t, tx, func() { recoveryConfigurationOutcomeFault(t, tx, func(v map[string]any) { v["tool_count"] = nil }) })
	})
	t.Run("wrong_response_worker", func(t *testing.T) {
		recoveryConfigurationOutcomeCase(t, tx, func() {
			if _, err := tx.Exec(ctx, "UPDATE pg_temp.configuration_audit SET actor_id='different_worker' WHERE action='response_received'"); err != nil {
				t.Fatal(err)
			}
		})
	})
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	recoveryConfigurationObservedStage(t, f.s)
	again, _, err := f.s.ClaimConfigurationVerification(ctx, "recovery_mcp_must_not_resend", time.Minute)
	if err != nil || again != nil {
		t.Fatal("observed revoked verification became dispatchable", err)
	}
}

func TestRecoveryConfigurationHistoryAuthorObservedModel(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentConfigurationModelSetup(t, nil)
	outcome, err := f.runtime(t).Execute(context.Background(), f.lease)
	if err != nil || outcome.Status != modelruntime.CandidateWaitingInput {
		t.Fatal("synthetic response failed", err)
	}
	observed, err := f.f.s.ConfigurationVerification(context.Background(), f.actor, f.verification.ID)
	if err != nil || observed.Status != "response_received" || observed.CostStatus != "unknown" {
		t.Fatal("probe response lost accounting uncertainty", err)
	}
	tx := recoveryInvariantStage(t, f.f.s)
	if err = validateRecoveryConfigurationHistory(context.Background(), tx, "pg_temp"); err != nil {
		t.Fatal("valid observed model history rejected", err)
	}
	var amount int64
	if err = tx.QueryRow(context.Background(), "SELECT amount_microusd FROM pg_temp.reservations WHERE status='unknown'").Scan(&amount); err != nil || amount != 17 {
		t.Fatal("unknown reservation was not retained", err)
	}
	t.Run("coherent_MCP_metadata_on_model", func(t *testing.T) {
		recoveryConfigurationOutcomeCase(t, tx, func() { recoveryConfigurationOutcomeFault(t, tx, func(v map[string]any) { v["tool_count"] = 0 }) })
	})
	if err = tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	recoveryConfigurationObservedStage(t, f.f.s)
	if f.calls.Load() != 1 {
		t.Fatal("recovery validation made another model call")
	}
	var retained int64
	if err = f.f.s.Pool.QueryRow(context.Background(), "SELECT amount_microusd FROM reservations WHERE status='unknown'").Scan(&retained); err != nil || retained != 17 {
		t.Fatal("source liability changed", err)
	}
}

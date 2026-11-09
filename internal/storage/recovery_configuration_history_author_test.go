// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRecoveryConfigurationHistoryAuthorLifecycle(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentConfigurationSchemaFixture(t)
	actor, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	grant := independentConfigurationGrant(t, f, f.owner, actor.ID, nil, 3, 3, 0)
	plan, _ := independentConfigurationPlan(t, f, actor, grant.ID)
	validation := independentConfigurationValidate(t, f, actor, plan)
	plan = independentConfigurationApply(t, f, actor, plan, validation)
	_, _ = independentConfigurationStart(t, f, actor, plan, "connect", domain.NewID("key"), 0)
	rawDenied, err := f.s.ValidateConfigurationPlan(context.Background(), actor, plan.ID, domain.NewID("key"), domain.ValidateConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest})
	denied := independentConfigurationDecode[domain.ConfigurationValidation](t, rawDenied, err)
	if denied.Valid {
		t.Fatal("already-applied historical validation unexpectedly allowed")
	}
	if _, err := f.s.RevokeConfigurationGrant(context.Background(), f.owner, grant.ID, domain.NewID("key"), domain.RevokeConfigurationGrant{ExpectedVersion: grant.Version, Reason: "Retain revoked historical authority without reviving it"}); err != nil {
		t.Fatal("revocation fixture failed", err)
	}
	tx := recoveryInvariantStage(t, f.s)
	if err := validateRecoveryConfigurationHistory(context.Background(), tx, "pg_temp"); err != nil {
		t.Fatal("valid configuration history rejected", err)
	}
	cases := map[string]string{
		"grant_original_actions":     `UPDATE pg_temp.configuration_grants SET body=jsonb_set(body,'{actions}','["plan"]')`,
		"plan_original_reason":       `UPDATE pg_temp.configuration_plans SET body=jsonb_set(body,'{reason}','"Changed plan reason"')`,
		"approval_original_reason":   `UPDATE pg_temp.configuration_approvals SET body=jsonb_set(body,'{reason}','"Changed approval reason"')`,
		"verification_unknown_field": `UPDATE pg_temp.configuration_verifications SET body=jsonb_set(body,'{reason}','"Changed verification reason"')`,
		"creation_audit_digest":      `UPDATE pg_temp.configuration_audit SET digest=repeat('a',64) WHERE action='plan.created'`,
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tx.Exec(context.Background(), "SAVEPOINT configuration_history_fault"); err != nil {
				t.Fatal(err)
			}
			defer tx.Exec(context.Background(), "ROLLBACK TO SAVEPOINT configuration_history_fault")
			if _, err := tx.Exec(context.Background(), query); err != nil {
				t.Fatal("forgery violated prerequisite row constraints", err)
			}
			if !errors.Is(validateRecoveryConfigurationHistory(context.Background(), tx, "pg_temp"), ErrRecoveryConfigurationHistory) {
				t.Fatal("forged history accepted")
			}
		})
	}
	t.Run("coherent_expired_approval", func(t *testing.T) {
		if _, err := tx.Exec(context.Background(), "SAVEPOINT approval_lifetime_fault"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(context.Background(), "ROLLBACK TO SAVEPOINT approval_lifetime_fault")
		var raw []byte
		if err := tx.QueryRow(context.Background(), "SELECT body FROM pg_temp.configuration_approvals").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var approval domain.ConfigurationApproval
		if err := json.Unmarshal(raw, &approval); err != nil {
			t.Fatal(err)
		}
		approval.CreatedAt = plan.ExpiresAt
		body, err := json.Marshal(approval)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(context.Background(), "UPDATE pg_temp.configuration_approvals SET body=$1 WHERE id=$2", body, approval.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(context.Background(), "UPDATE pg_temp.configuration_audit SET digest=$1 WHERE object_id=$2 AND action='plan.approved'", digestValue(approval), approval.ID); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(validateRecoveryConfigurationHistory(context.Background(), tx, "pg_temp"), ErrRecoveryConfigurationHistory) {
			t.Fatal("impossible expired approval accepted with matching audit hash")
		}
	})
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	var original bytes.Buffer
	if _, err := f.s.ExportRecoverySnapshot(context.Background(), &original, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("export failed", err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, original.Bytes())
	report, err := target.CheckRecoverySnapshotData(context.Background(), recoveryStageAuthorArchive(t, original.Bytes()), spec, DefaultRecoverySnapshotLimits())
	if err != nil || !report.RollbackConfirmed || report.FullSemanticValidation {
		t.Fatal("valid configuration archive rejected", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	changed := recoveryStageAuthorRewrite(t, original.Bytes(), func(stage *recoveryStageAuthorFixture) {
		column := recoveryAuthorColumn(t, stage.header, "configuration_audit", "digest")
		action := recoveryAuthorColumn(t, stage.header, "configuration_audit", "action")
		changed := false
		for _, row := range stage.rows["configuration_audit"] {
			if string(row[action]) == "plan.created" {
				row[column] = []byte(Hash([]byte("forged original plan audit")))
				changed = true
			}
		}
		if !changed {
			t.Fatal("plan audit fixture missing")
		}
	})
	if _, err := target.CheckRecoverySnapshotData(context.Background(), recoveryStageAuthorArchive(t, changed), spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryConfigurationHistory) {
		t.Fatal("authenticated forged audit accepted", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
}

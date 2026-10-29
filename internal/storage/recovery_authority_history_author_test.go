// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

func TestRecoveryAuthorityHistoryAuthorConfigurationEpochs(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentConfigurationSchemaFixture(t)
	actor, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	grant := independentConfigurationGrant(t, f, f.owner, actor.ID, nil, 3, 3, 0)
	plan, _ := independentConfigurationPlan(t, f, actor, grant.ID)
	validation := independentConfigurationValidate(t, f, actor, plan)
	plan = independentConfigurationApply(t, f, actor, plan, validation)
	_, _ = independentConfigurationStart(t, f, actor, plan, "connect", domain.NewID("key"), 0)
	ctx := context.Background()
	tx := recoveryInvariantStage(t, f.s)
	recoveryInvariantWantValid(t, tx)
	for _, column := range []struct{ table, column string }{
		{"configuration_grants", "issuer_authority_epoch"}, {"configuration_grants", "issuer_policy_epoch"},
		{"configuration_grants", "subject_authority_epoch"}, {"configuration_grants", "subject_policy_epoch"},
		{"configuration_plans", "actor_authority_epoch"}, {"configuration_plans", "actor_policy_epoch"},
		{"configuration_approvals", "authority_epoch"}, {"configuration_approvals", "policy_epoch"},
		{"configuration_verifications", "actor_authority_epoch"}, {"configuration_verifications", "actor_policy_epoch"},
	} {
		t.Run(column.table+"_"+column.column, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT future_epoch"); err != nil {
				t.Fatal(err)
			}
			defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT future_epoch")
			tag, err := tx.Exec(ctx, "UPDATE pg_temp."+column.table+" SET "+column.column+"=999")
			if err != nil || tag.RowsAffected() == 0 {
				t.Fatal("future epoch fixture absent or invalid", err)
			}
			// Body/audit hashes still agree; epoch columns are independent evidence.
			if err := validateRecoveryConfigurationHistory(ctx, tx, "pg_temp"); err != nil {
				t.Fatal("fault changed original body history", err)
			}
			if !errors.Is(validateRecoveryInvariants(ctx, tx, "pg_temp"), ErrRecoveryInvariant) {
				t.Fatal("future configuration authority accepted")
			}
		})
	}
	t.Run("older_epochs_after_revocation", func(t *testing.T) {
		if _, err := tx.Exec(ctx, "SAVEPOINT old_epoch"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT old_epoch")
		if _, err := tx.Exec(ctx, "UPDATE pg_temp.memberships SET security_epoch=security_epoch+10,active=false WHERE principal_id=$1", actor.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "UPDATE pg_temp.organization_login_policies SET human_authority_epoch=human_authority_epoch+10,service_authority_epoch=service_authority_epoch+10"); err != nil {
			t.Fatal(err)
		}
		recoveryInvariantWantValid(t, tx)
		if err := validateRecoveryConfigurationHistory(ctx, tx, "pg_temp"); err != nil {
			t.Fatal("old historical authority reinterpreted as live", err)
		}
	})
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if _, err := f.s.ExportRecoverySnapshot(ctx, &snapshot, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, snapshot.Bytes())
	report, err := target.CheckRecoverySnapshotData(ctx, recoveryStageAuthorArchive(t, snapshot.Bytes()), spec, DefaultRecoverySnapshotLimits())
	if err != nil || !report.RollbackConfirmed || report.FullSemanticValidation {
		t.Fatal("valid authority archive failed", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	changed := recoveryStageAuthorRewrite(t, snapshot.Bytes(), func(f *recoveryStageAuthorFixture) {
		column := recoveryAuthorColumn(t, f.header, "configuration_approvals", "authority_epoch")
		if len(f.rows["configuration_approvals"]) == 0 {
			t.Fatal("approval fixture absent")
		}
		for _, row := range f.rows["configuration_approvals"] {
			value := make([]byte, 8)
			binary.BigEndian.PutUint64(value, 999)
			row[column] = value
		}
	})
	if _, err = target.CheckRecoverySnapshotData(ctx, recoveryStageAuthorArchive(t, changed), spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryInvariant) {
		t.Fatal("authenticated future epoch accepted", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
}

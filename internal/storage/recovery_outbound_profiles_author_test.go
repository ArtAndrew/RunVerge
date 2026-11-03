// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestRecoveryOutboundProfilesAuthorImportedReferencesStayInert(t *testing.T) {
	f := recoveryProfilesSetup(t)
	ctx := context.Background()
	org := f.p.OrganizationID
	refs := []RecoveryOutboundProfile{
		{org, "model", f.model.ID, 1},
		{org, "tool", f.tool.Tool.ID, 1}, // Deliberately keyless MCP tool.
		{org, "model", "configured_model", 1},
		{org, "mcp", "configured_mcp", 1},
		{org, "destination", "destination_model", 1},
		{org, "destination", "destination_mcp", 1},
		{org, "credential_purpose", "purpose_model", 1},
		{org, "credential_purpose", "purpose_mcp", 1},
		{org, "webhook", f.webhook.profile.Destination.ID, 1},
	}
	for _, ref := range refs {
		if err := f.s.CheckRecoveryOutboundProfiles(ctx, ref); err != nil {
			t.Fatal("ordinary installed profile refused", ref.Kind, err)
		}
	}
	var raw bytes.Buffer
	if _, err := f.s.ExportRecoverySnapshot(ctx, &raw, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("profile source export failed", err)
	}
	var header recoverySnapshotHeader
	if err := json.Unmarshal(bytes.SplitN(raw.Bytes(), []byte("\n"), 2)[0], &header); err != nil {
		t.Fatal("source inventory unavailable", err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, raw.Bytes())
	if _, err := target.ImportRecoverySnapshot(ctx, recoveryStageAuthorArchive(t, raw.Bytes()), spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("actual authenticated profile import failed", err)
	}
	if err := target.CheckRecoveryOutboundProfiles(ctx, refs[0]); !errors.Is(err, RecoveryHeld) {
		t.Fatal("held target admitted outbound use", err)
	}
	// Trusted negative-test fault ONLY: unmask the independent history fence.
	// This is not a product resume or credential-recovery operation.
	control := &independentImportedFixture{target: target, spec: spec, held: held}
	independentImportFaultControl(t, control, false)
	t.Cleanup(func() { independentImportFaultControl(t, control, true) })
	if err := target.CheckRuntimeAdmission(ctx); err != nil {
		t.Fatal("global gate still masks profile probes", err)
	}
	before := independentImportRows(t, target, header.Inventory, true)
	for _, ref := range refs {
		if err := target.CheckRecoveryOutboundProfiles(ctx, ref); !errors.Is(err, domain.Forbidden) {
			t.Fatal("imported profile lacked independent denial", ref.Kind, err)
		}
	}
	tx, err := target.beginRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = loadToolConnectionTx(ctx, tx, org, f.tool.Tool.ID, 1, false); err != nil {
		t.Fatal("historical tool read lost", err)
	}
	if _, err = loadConfigurationDestinationTx(ctx, tx, org, "destination_mcp", 1, false); err != nil {
		t.Fatal("historical destination read lost", err)
	}
	if _, err = loadCredentialPurposeProfileTx(ctx, tx, org, "purpose_mcp", 1, false); err != nil {
		t.Fatal("historical purpose read lost", err)
	}
	if _, _, err = loadWebhookProfileTx(ctx, tx, org, f.webhook.profile.Destination.ID, 1); err != nil {
		t.Fatal("historical webhook read lost", err)
	}
	if _, err = loadToolConnectionTx(ctx, tx, org, f.tool.Tool.ID, 1, true); !errors.Is(err, domain.Forbidden) {
		t.Fatal("active tool path accepted imported profile", err)
	}
	if _, err = loadConfigurationDestinationTx(ctx, tx, org, "destination_mcp", 1, true); !errors.Is(err, domain.Forbidden) {
		t.Fatal("active destination path accepted imported profile", err)
	}
	if _, err = loadCredentialPurposeProfileTx(ctx, tx, org, "purpose_mcp", 1, true); !errors.Is(err, domain.Forbidden) {
		t.Fatal("active purpose path accepted imported profile", err)
	}
	_ = tx.Rollback(ctx)

	// Explicit new host-installed profiles in the SAME organization remain
	// usable. Use a different model name: reauthorizing the historical model
	// name is a separate, not-yet-implemented recovery operation.
	freshModel := f.model
	freshModel.ID, freshModel.Model = "fresh_model", "fresh-model"
	if _, err = target.InstallModelConnection(ctx, freshModel); err != nil {
		t.Fatal("fresh explicit model install failed", err)
	}
	freshTool := f.tool
	freshTool.Tool.ID = "fresh_tool"
	if _, err = target.InstallToolConnection(ctx, freshTool); err != nil {
		t.Fatal("fresh explicit keyless tool install failed", err)
	}
	freshWebhook := f.webhook.profile
	freshWebhook.Destination.ID = "fresh_webhook"
	if _, err = target.InstallWebhookDestination(ctx, freshWebhook); err != nil {
		t.Fatal("fresh explicit webhook install failed", err)
	}
	if err = target.CheckRecoveryOutboundProfiles(ctx,
		RecoveryOutboundProfile{org, "model", freshModel.ID, 1},
		RecoveryOutboundProfile{org, "tool", freshTool.Tool.ID, 1},
		RecoveryOutboundProfile{org, "webhook", freshWebhook.Destination.ID, 1}); err != nil {
		t.Fatal("fresh profiles incorrectly inherited history denial", err)
	}
	// A fresh resource row cannot launder a historical destination/purpose.
	// New inactive versions preserve the exact destination FK and leave every
	// historical version untouched. The transaction is rolled back afterward.
	tx, err = target.beginRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	dest, err := loadConfigurationDestinationTx(ctx, tx, org, "destination_mcp", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = installMCPConnectionTx(ctx, tx, org, "configured_mcp", 2, *dest.MCP, false); err != nil {
		t.Fatal("fresh MCP fault control failed", err)
	}
	configured, err := scanModelConnection(tx.QueryRow(ctx, "SELECT profile,active,created_at FROM model_connections WHERE organization_id=$1 AND id='configured_model' AND version=1", org))
	if err != nil {
		t.Fatal(err)
	}
	configured.Version, configured.Active = 2, false
	if _, err = installModelConnectionTx(ctx, tx, configured); err != nil {
		t.Fatal("fresh model version control failed", err)
	}
	for _, kind := range []string{"model", "mcp"} {
		if err = checkRecoveryOutboundProfileTx(ctx, tx, org, kind, "configured_"+kind, 2); err != nil {
			t.Fatal("fresh version must not be historical", kind, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO configuration_resource_bindings(organization_id,kind,target_id,resource_version,destination_id,destination_version,credential_purpose_id,credential_purpose_version) VALUES($1,$2,$3,2,$4,1,$5,1)`, org, kind, "configured_"+kind, "destination_"+kind, "purpose_"+kind); err != nil {
			t.Fatal("bound history fault setup failed", err)
		}
		if err = checkConfigurationResourceAdmissionTx(ctx, tx, org, kind, "configured_"+kind, 2); !errors.Is(err, domain.Forbidden) {
			t.Fatal("fresh resource inherited imported configuration authority", kind, err)
		}
	}
	_ = tx.Rollback(ctx)
	// Reinstalling identical legacy model bytes is not reauthorization.
	if _, err = target.InstallModelConnection(ctx, f.model); err != nil {
		t.Fatal("immutable model reinstall unexpectedly changed behavior", err)
	}
	if err = target.CheckRecoveryOutboundProfiles(ctx, refs[0]); !errors.Is(err, domain.Forbidden) {
		t.Fatal("idempotent install reauthorized imported model", err)
	}
	independentImportAssertRows(t, before, independentImportRows(t, target, header.Inventory, true))
}

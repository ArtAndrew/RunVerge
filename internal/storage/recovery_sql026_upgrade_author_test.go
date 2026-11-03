// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

func TestRecoverySQL026AuthorUsageUpgrade(t *testing.T) {
	recoveryEvidenceLegacyProducer(t, "RUNVERGE_RECOVERY_SQL026_BINARY", recoverySQL026Schema, recoverySQL026Catalog, "usage26")
}

type recoverySQL026UsageUpgradeFixture struct {
	generation         []byte
	generationIdentity recoveryLineageGenerationStart

	selection   RecoveryUsageSelection
	complete    func() domain.Artifact
	calls       *atomic.Int32
	beforeCalls int32
	recorded    RecoveryUsageRecording
	rows        string
}

func recoverySQL026UsageSource(t *testing.T, source *Store) *recoverySQL026UsageUpgradeFixture {
	t.Helper()
	calls := recoveryUsageOwnedHTTPS(t)
	p, _ := identity(t, source, 100000)
	c, err := source.InstallModelConnection(context.Background(), domain.ModelConnection{ID: "usage-router", OrganizationID: p.OrganizationID, Version: 1, WireProfile: domain.ModelWireOpenRouterTextV1, Model: "example/usage-model", ProviderAccount: "synthetic-provider-account", PriceVersion: "synthetic-v1", Endpoint: modelgateway.OpenRouterEndpoint, CredentialEnvironment: "PLATFORM_MODEL_KEY_USAGE_FIXTURE", MaxChargeMicroUSD: 37, MaxInputTokens: 32, InputMicroUSDPerMillion: 1000000, MaxOutputTokens: 16, MaxInputBytes: 4096, MaxOutputBytes: 4096, TimeoutMilliseconds: 2000, TokenLimitField: "max_tokens", BoundAssumptions: "Synthetic history only; no model request", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	r, complete := recoveryUsageAuthorAttempt(t, source, p, c, "gen-usage-selected")
	return &recoverySQL026UsageUpgradeFixture{selection: RecoveryUsageSelection{OrganizationID: p.OrganizationID, ReservationID: r.ID}, complete: complete, calls: calls}
}

func recoverySQL026UsageRows(t *testing.T, s *Store) string {
	t.Helper()
	var rows string
	if err := s.Pool.QueryRow(context.Background(), "SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM recovery_usage_observations r").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func (f *recoverySQL026UsageUpgradeFixture) applyOld(t *testing.T, old func(string, ...string) []byte, sourceIntent, targetIntent, dir string, s *Store) {
	t.Helper()
	old("unused", "recovery", "evidence-ingest", "--source-intent-file", sourceIntent, "--target-config", targetIntent, "--confirm-ingest-evidence")
	intent := filepath.Join(dir, "usage26.json")
	var plan RecoveryUsageReadbackPlan
	if err := json.Unmarshal(old("unused", "recovery", "usage-plan", "--source-intent-file", sourceIntent, "--target-config", targetIntent, "--organization-id", f.selection.OrganizationID, "--reservation-id", f.selection.ReservationID, "--out", intent), &plan); err != nil || plan.TargetCatalogSHA256 != recoverySQL026Catalog || plan.State != "request_identity_available" {
		t.Fatal("old usage plan", err)
	}
	key := filepath.Join(dir, "synthetic-usage-key")
	if err := os.WriteFile(key, []byte(recoveryUsagePeerKey), 0600); err != nil {
		t.Fatal(err)
	}
	authority := filepath.Join(dir, "usage26-authority")
	old("unused", "recovery", "usage-access-init", "--intent-file", intent, "--authority-dir", authority, "--key-file", key, "--credential-reference", "owned-upgrade-key", "--operator-reference", "owned:upgrade", "--expires-at", time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "--confirm-current-account")
	old("unused", "recovery", "usage-acquire", "--intent-file", intent, "--authority-dir", authority, "--confirm-read-provider-usage")
	if err := json.Unmarshal(old("unused", "recovery", "usage-inspect", "--intent-file", intent), &f.recorded); err != nil || f.recorded.State != "recorded" || f.recorded.ObservationState != "observed_unverified" {
		t.Fatal("old usage recording", err)
	}
	f.captureGeneration(t, s)
	f.rows = recoverySQL026UsageRows(t, s)
	f.beforeCalls = f.calls.Load()
	if f.beforeCalls != 1 {
		t.Fatal("expected exactly one owned usage request", f.beforeCalls)
	}
}

func (f *recoverySQL026UsageUpgradeFixture) checkUpgraded(t *testing.T, s *Store, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec) {
	t.Helper()
	f.stageGeneration(t, s)
	inspected, err := s.InspectRecoveryUsage(context.Background(), selected, late, verified, spec, DefaultRecoverySnapshotLimits(), f.selection, f.recorded.OperationID, f.recorded.AcquisitionSHA256)
	if err != nil {
		t.Fatal("SQL026 observation unreadable after SQL027 upgrade", err)
	}
	// JSON returns UTC for Z; pgx binary timestamps use time.Local. Compare
	// instants, then normalize only the location for exact remaining fields.
	if !inspected.RecordedAt.Equal(f.recorded.RecordedAt) {
		t.Fatal("upgrade changed observation timestamp")
	}
	expected := f.recorded
	inspected.RecordedAt, expected.RecordedAt = inspected.RecordedAt.UTC(), expected.RecordedAt.UTC()
	if !reflect.DeepEqual(inspected, expected) {
		t.Fatal("upgrade changed observation fields")
	}
	if recoverySQL026UsageRows(t, s) != f.rows || f.calls.Load() != f.beforeCalls {
		t.Fatal("upgrade inspection changed history or contacted provider")
	}
}

// Capture actual SQL026 rows before migration, using the frozen layout. This
// test-only writer does not expose a legacy catalog bypass in production.
func (f *recoverySQL026UsageUpgradeFixture) captureGeneration(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	catalog, err := readRecoverySemanticCatalogProfile(ctx, tx, s.runtimeSchema, recoverySQL026Inventory, recoverySQL026Schema)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(catalog)
	if err != nil || Hash(raw) != recoverySQL026Catalog {
		t.Fatal("old actual catalog mismatch", err)
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := recoveryLineageInventoryProfile(inventory, "sql026-current-generation-v1")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := readRecoveryImportReceipt(ctx, tx, s.runtimeSchema)
	if err != nil {
		t.Fatal(err)
	}
	f.generationIdentity = recoveryLineageGenerationStart{Kind: "generation", Profile: "sql026-current-generation-v1", InstallationID: receipt.TargetInstallationID, RestoreID: receipt.RestoreID, ImportedSourceID: receipt.SourceID, ImportedSnapshotSHA256: receipt.SnapshotSHA256, InventorySHA256: recoverySQL026Inventory, CatalogSHA256: recoverySQL026Catalog}
	limits := DefaultRecoverySnapshotLimits()
	limits.MaxRowBytes = recoveryLineageMetadataRowBytes
	counts, err := recoverySnapshotPreflight(ctx, tx, s.runtimeSchema, projected, limits)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writer := &recoverySnapshotWriter{ctx: ctx, out: &out, hash: sha256.New(), limit: limits.MaxWireBytes}
	report := RecoverySnapshotReport{}
	if err = writer.record(f.generationIdentity); err != nil {
		t.Fatal(err)
	}
	if err = writeRecoverySnapshotTables(ctx, tx, s.runtimeSchema, projected, counts, writer, limits, &report); err != nil {
		t.Fatal(err)
	}
	if err = writer.record(recoveryLineageGenerationEnd{Kind: "generation_end", Tables: len(report.Tables), Rows: report.Rows, Bytes: report.Bytes}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	f.generation = out.Bytes()
}

func (f *recoverySQL026UsageUpgradeFixture) stageGeneration(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stageRecoveryLineageGeneration(ctx, tx, s.runtimeSchema, bytes.NewReader(f.generation), inventory, f.generationIdentity, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("SQL026 historical generation rejected on SQL027", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverySQL026AuthorAccountingUpgrade(t *testing.T) {
	for _, mode := range []string{"old_resolution", "new_resolution"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEvidenceLegacyProducer(t, "RUNVERGE_RECOVERY_SQL026_BINARY", recoverySQL026Schema, recoverySQL026Catalog, mode)
		})
	}
}

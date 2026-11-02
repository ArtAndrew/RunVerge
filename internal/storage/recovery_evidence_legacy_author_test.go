// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/recoveryarchive"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecoveryEvidenceAuthorActualSQL023ProducerAndHeldUpgrade(t *testing.T) {
	recoveryEvidenceLegacyProducer(t, "RUNVERGE_RECOVERY_SQL023_BINARY", recoverySQL023Schema, recoverySQL023Catalog)
}
func TestRecoveryEvidenceAuthorActualSQL024ProducerAndHeldUpgrade(t *testing.T) {
	recoveryEvidenceLegacyProducer(t, "RUNVERGE_RECOVERY_SQL024_BINARY", recoverySQL024Schema, recoverySQL024Catalog)
}
func recoveryEvidenceLegacyProducer(t *testing.T, binaryEnvironment, sourceSchema, sourceCatalog string) {
	t.Helper()
	recoveryInvariantOwned(t)
	binary := os.Getenv(binaryEnvironment)
	if binary == "" {
		t.Skip("exact previously verified old-schema binary required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	stage, keys := filepath.Join(d, "stage"), filepath.Join(d, "keys")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryarchive.GenerateHostKeys(keys); err != nil {
		t.Fatal(err)
	}
	old := func(dsn string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			if key != "DATABASE_URL" && !strings.HasPrefix(key, "PG") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "DATABASE_URL="+dsn)
		var out, privateErr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &privateErr
		if err := cmd.Run(); err != nil {
			t.Fatalf("old-schema fixture command %s failed: %v", args[0], err)
		}
		return out.Bytes()
	}
	private := func(name string, v any) string {
		t.Helper()
		p := filepath.Join(d, name)
		raw, _ := json.Marshal(v)
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base, err := Open(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	legacyStore := func() (*Store, string) {
		t.Helper()
		name := domain.NewID("test")
		if _, e := base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); e != nil {
			t.Fatal(e)
		}
		u, _ := url.Parse(os.Getenv("TEST_DATABASE_URL"))
		q := u.Query()
		q.Set("search_path", name)
		u.RawQuery = q.Encode()
		dsn := u.String()
		old(dsn, "migrate")
		s, e := Open(ctx, dsn)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			s.Close()
			cleanup, e := Open(context.Background(), os.Getenv("TEST_DATABASE_URL"))
			if e != nil {
				t.Error(e)
				return
			}
			defer cleanup.Close()
			if _, e = cleanup.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()+" CASCADE"); e != nil {
				t.Error(e)
			}
		})
		return s, dsn
	}
	source, sourceDSN := legacyStore()
	p, _, content := recoveryAuthorPopulate(t, source)
	sourceConfig := private("source.json", map[string]string{"database_url": sourceDSN})
	backup := func(name string) (*recoveryarchive.VerifiedArchive, recoveryarchive.HostBackupReceipt) {
		t.Helper()
		dir := filepath.Join(d, name)
		raw := old("unused", "recovery", "backup-create", "--database-config", sourceConfig, "--keys-file", keys, "--staging-parent", stage, "--out-dir", dir)
		var receipt recoveryarchive.HostBackupReceipt
		if json.Unmarshal(raw, &receipt) != nil || receipt.Manifest.Metadata.SchemaSHA256 != sourceSchema {
			t.Fatal("old producer schema differs")
		}
		a, e := recoveryarchive.VerifyHostBackup(ctx, dir, keys, receipt.ReceiptSHA256, stage)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if e := a.Close(); e != nil {
				t.Error(e)
			}
		})
		return a, receipt
	}
	selected, receipt := backup("selected")
	// The new source has a whole late-only organization with results/unknown costs.
	recoveryAuthorPopulate(t, source)
	late, _ := backup("late")
	verified := evidenceAuthorPair(t, selected, late)
	legacyTarget, targetDSN := legacyStore()
	var targetID string
	if legacyTarget.Pool.QueryRow(ctx, "SELECT installation_id FROM runtime_recovery_control WHERE singleton").Scan(&targetID) != nil {
		t.Fatal("old target identity")
	}
	targetDatabase := private("target-db.json", map[string]string{"database_url": targetDSN})
	targetIntent := filepath.Join(d, "target.json")
	selection := []string{"--backup-dir", filepath.Join(d, "selected"), "--keys-file", keys, "--staging-parent", stage, "--expected-receipt-sha256", receipt.ReceiptSHA256}
	args := append([]string{"recovery", "target-prepare", "--database-config", targetDatabase, "--intent-out", targetIntent, "--expected-installation-id", targetID, "--confirm-new-target"}, selection...)
	old("unused", args...)
	old("unused", "recovery", "archive-import", "--target-config", targetIntent, "--backup-dir", filepath.Join(d, "selected"), "--keys-file", keys, "--staging-parent", stage)
	var config struct {
		TargetID  string `json:"target_installation_id"`
		RestoreID string `json:"restore_id"`
	}
	raw, e := os.ReadFile(targetIntent)
	if e != nil || json.Unmarshal(raw, &config) != nil {
		t.Fatal("old target intent")
	}
	m := receipt.Manifest.Metadata
	spec := RecoveryTargetSpec{TargetInstallationID: config.TargetID, RestoreID: config.RestoreID, BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}
	var before string
	if legacyTarget.Pool.QueryRow(ctx, "SELECT row_to_json(r)::text FROM recovery_import_receipts r").Scan(&before) != nil {
		t.Fatal("old receipt")
	}
	if err = legacyTarget.Migrate(ctx); err != nil {
		t.Fatal("forward held upgrade", err)
	}
	var after string
	if legacyTarget.Pool.QueryRow(ctx, "SELECT row_to_json(r)::text FROM recovery_import_receipts r").Scan(&after) != nil || before != after {
		t.Fatal("upgrade rewrote old receipt")
	}
	inspection, err := legacyTarget.InspectRecoveryImport(ctx, spec, receipt.Manifest.PlaintextSHA256)
	if err != nil || inspection.Receipt == nil || inspection.Receipt.CatalogSHA256 != sourceCatalog {
		t.Fatal("old receipt unreadable", err)
	}
	if _, err = legacyTarget.IngestRecoveryEvidence(ctx, selected, late, verified, spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("old evidence ingestion", err)
	}
	if _, err = legacyTarget.InspectRecoveryEvidence(ctx, spec, verified); err != nil {
		t.Fatal(err)
	}
	plan, err := legacyTarget.PlanRecoveryReconciliation(ctx, selected, late, verified, spec, DefaultRecoverySnapshotLimits())
	if err != nil || plan.SourceSchemaSHA256 != sourceSchema || plan.EvidenceSchemaSHA256 != sourceSchema || !plan.RollbackConfirmed {
		t.Fatal("old-schema reconciliation plan", err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binding := recoveryaccess.Binding{TargetID: spec.TargetInstallationID, RestoreID: spec.RestoreID, SnapshotSHA256: receipt.Manifest.PlaintextSHA256}
	authority, err := recoveryaccess.New(recoveryaccess.Policy{Binding: binding, Epoch: 1, Issuers: map[string]ed25519.PublicKey{"current": pub}, Scopes: []recoveryaccess.Scope{{Subject: "reviewer", OrganizationID: p.OrganizationID}}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewRecoveryReader(legacyTarget, authority)
	if err != nil {
		t.Fatal(err)
	}
	token, err := recoveryaccess.Sign(recoveryaccess.Claims{Version: 1, Binding: binding, Issuer: "current", Subject: "reviewer", OrganizationID: p.OrganizationID, Epoch: 1, IssuedAt: time.Now().Unix() - 1, ExpiresAt: time.Now().Unix() + 600}, key)
	if err != nil {
		t.Fatal(err)
	}
	var candidate string
	if legacyTarget.Pool.QueryRow(ctx, "SELECT id FROM task_candidates WHERE organization_id=$1", p.OrganizationID).Scan(&candidate) != nil {
		t.Fatal("old candidate")
	}
	result, err := reader.Result(ctx, token, p.OrganizationID, candidate)
	if err != nil || result.Content != content || !result.Notice.ExecutionHeld {
		t.Fatal("old result/fresh reader", err)
	}
	// Also import the real old archive into a fresh current-schema target.
	if _, err = selected.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	oldWire, err := io.ReadAll(selected)
	if err != nil {
		t.Fatal(err)
	}
	fresh, freshSpec, _ := recoveryStageAuthorTarget(t, oldWire)
	newReceipt, err := fresh.ImportRecoverySnapshot(ctx, selected, freshSpec, DefaultRecoverySnapshotLimits())
	if err != nil || newReceipt.SchemaSHA256 != sourceSchema || newReceipt.CatalogSHA256 != recoverySemanticCatalogSHA256 || !reflect.DeepEqual(newReceipt.Tables, inspection.Receipt.Tables) {
		t.Fatal("old-schema to fresh current-schema", err)
	}
}

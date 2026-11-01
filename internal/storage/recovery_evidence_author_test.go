// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"bytes"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type evidenceAuthorFixture struct {
	target         *Store
	spec           RecoveryTargetSpec
	selected, late *recoveryarchive.VerifiedArchive
	verified       *recoveryarchive.VerifiedCaptureLink
	before         RecoverySnapshotReport
	lateBytes      []byte
	oldOrg, newOrg string
}

func evidenceAuthorPair(t *testing.T, selected, late *recoveryarchive.VerifiedArchive) *recoveryarchive.VerifiedCaptureLink {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	keys, path := filepath.Join(d, "keys"), filepath.Join(d, "link")
	if _, err := recoveryarchive.GenerateHostKeys(keys); err != nil {
		t.Fatal(err)
	}
	a, b := selected.Manifest(), late.Manifest()
	l := recoveryarchive.CaptureLink{Version: 1, OperationID: strings.TrimPrefix(domain.NewID("capture"), "capture_"), SourceID: a.Metadata.SourceID, SelectedReceiptSHA256: recoveryManifestReceipt(selected), SelectedSnapshotSHA256: a.PlaintextSHA256, SelectedCutUTC: a.Metadata.CutUTC, BindingSHA256: strings.Repeat("a", 64), StoppedExecutorsSHA256: strings.Repeat("b", 64), EvidenceReceiptSHA256: recoveryManifestReceipt(late), EvidenceSnapshotSHA256: b.PlaintextSHA256, EvidenceCutUTC: b.Metadata.CutUTC, CapturedAt: time.Now().UTC()}
	if err := recoveryarchive.WriteHostCaptureLink(path, keys, l); err != nil {
		t.Fatal(err)
	}
	v, err := recoveryarchive.VerifyHostCaptureLink(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func evidenceAuthorSetup(t *testing.T) *evidenceAuthorFixture {
	t.Helper()
	ctx := context.Background()
	source, _ := recoveryAuthorStore(t)
	p, _, _ := recoveryAuthorPopulate(t, source)
	unused := domain.NewID("principal")
	if _, err := source.Pool.Exec(ctx, "INSERT INTO principals(id,name,active) VALUES($1,'unused synthetic principal',false)", unused); err != nil {
		t.Fatal(err)
	}
	var original, late bytes.Buffer
	before, err := source.ExportRecoverySnapshot(ctx, &original, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	newPrincipal, _, _ := recoveryAuthorPopulate(t, source)
	if _, err = source.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE principal_id=$1", p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Pool.Exec(ctx, "DELETE FROM principals WHERE id=$1", unused); err != nil {
		t.Fatal(err)
	}
	if _, err = source.ExportRecoverySnapshot(ctx, &late, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	a, b := recoveryStageAuthorArchive(t, original.Bytes()), recoveryStageAuthorArchive(t, late.Bytes())
	target, spec, _ := recoveryStageAuthorTarget(t, original.Bytes())
	if _, err = target.ImportRecoverySnapshot(ctx, a, spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	return &evidenceAuthorFixture{target: target, spec: spec, selected: a, late: b, verified: evidenceAuthorPair(t, a, b), before: before, lateBytes: late.Bytes(), oldOrg: p.OrganizationID, newOrg: newPrincipal.OrganizationID}
}

func TestRecoveryEvidenceAuthorPreservesLateFactsWithoutExecution(t *testing.T) {
	f := evidenceAuthorSetup(t)
	ctx := context.Background()
	report, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("evidence ingest", err)
	}
	if report.Added == 0 || report.Changed == 0 || report.MissingFromLaterSnapshot != 1 || report.UnmappedScope == 0 || report.ReconciliationState != "pending" || !report.ExecutionHeld {
		t.Fatal("incomplete classifications")
	}
	inspected, err := f.target.InspectRecoveryEvidence(ctx, f.spec, f.verified)
	if err != nil || !reflect.DeepEqual(report, inspected) {
		t.Fatal("fresh evidence inspection", err)
	}
	if _, err = f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err == nil {
		t.Fatal("accepted batch replayed")
	}
	if !errors.Is(f.target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("evidence enabled execution")
	}
	var newOrg, revoked int
	var unknown int
	if f.target.Pool.QueryRow(ctx, "SELECT count(*) FROM organizations WHERE id=$1", f.newOrg).Scan(&newOrg) != nil || newOrg != 0 {
		t.Fatal("late-only org became business authority")
	}
	if f.target.Pool.QueryRow(ctx, "SELECT count(*) FROM access_tokens WHERE revoked").Scan(&revoked) != nil || revoked != 0 {
		t.Fatal("original access history replaced")
	}
	if f.target.Pool.QueryRow(ctx, "SELECT count(*) FROM reservations WHERE status='unknown'").Scan(&unknown) != nil || unknown != 2 {
		t.Fatal("original uncertainty changed")
	}
	rows, err := f.target.Pool.Query(ctx, "SELECT payload FROM recovery_evidence_chunks ORDER BY ordinal")
	if err != nil {
		t.Fatal(err)
	}
	var restored bytes.Buffer
	for rows.Next() {
		var raw []byte
		if rows.Scan(&raw) != nil {
			t.Fatal("chunk read")
		}
		restored.Write(raw)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !bytes.Equal(restored.Bytes(), f.lateBytes) {
		t.Fatal("late wire bytes changed")
	}
	tx, err := f.target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	inv, err := recoverySnapshotInventory(ctx, tx, f.target.runtimeSchema, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = recoveryStageCheckDigests(ctx, tx, f.target.runtimeSchema, inv, f.before, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("original business rows changed", err)
	}
}

func TestRecoveryEvidenceAuthorRejectsWrongPairAndModifiedBaseline(t *testing.T) {
	for _, fault := range []string{"pair", "baseline"} {
		t.Run(fault, func(t *testing.T) {
			f := evidenceAuthorSetup(t)
			ctx := context.Background()
			if fault == "baseline" {
				// Owned negative fixture: alter content, then restore the exact
				// trusted triggers before calling the production validator.
				tx, e := f.target.Pool.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = tx.Exec(ctx, "ALTER TABLE organizations DISABLE TRIGGER USER"); e != nil {
					t.Fatal(e)
				}
				if _, e = tx.Exec(ctx, "UPDATE organizations SET budget_microusd=budget_microusd+1 WHERE id=$1", f.oldOrg); e != nil {
					t.Fatal(e)
				}
				if _, e = tx.Exec(ctx, "ALTER TABLE organizations ENABLE TRIGGER USER"); e != nil {
					t.Fatal(e)
				}
				if e = tx.Commit(ctx); e != nil {
					t.Fatal(e)
				}
			}
			late := f.late
			if fault == "pair" {
				late = f.selected
			}
			if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err == nil {
				t.Fatal("invalid evidence accepted")
			}
			var n int
			if f.target.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM recovery_evidence_batches)+(SELECT count(*) FROM recovery_evidence_chunks)+(SELECT count(*) FROM recovery_evidence_changes)").Scan(&n) != nil || n != 0 {
				t.Fatal("partial evidence persisted")
			}
		})
	}
}

func TestRecoveryEvidenceAuthorRealCommitLoss(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := evidenceAuthorSetup(t)
			victim, proxy := independentImportCommitProxy(t, f.target.Pool.Config().ConnString())
			plan := proxy.arm(t, "insert into "+pgx.Identifier{victim.runtimeSchema, "recovery_evidence_batches"}.Sanitize(), mode)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r, err := victim.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits())
			if !errors.Is(err, ErrRecoveryEvidenceUnknown) || !reflect.DeepEqual(r, RecoveryEvidenceReport{}) {
				t.Fatal("uncertain commit result", err)
			}
			proxy.wait(t, plan)
			observer, err := Open(context.Background(), f.target.Pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			var count int
			if observer.Pool.QueryRow(context.Background(), "SELECT count(*) FROM recovery_evidence_batches").Scan(&count) != nil {
				t.Fatal("batch inspection")
			}
			if mode == "before_commit" && count != 0 {
				t.Fatal("precommit cut persisted batch")
			}
			if mode == "after_commit" {
				if count != 1 {
					t.Fatal("committed batch absent")
				}
				if _, err = observer.InspectRecoveryEvidence(context.Background(), f.spec, f.verified); err != nil {
					t.Fatal("committed evidence unreadable", err)
				}
			}
			if !errors.Is(observer.CheckRuntimeAdmission(context.Background()), RecoveryHeld) {
				t.Fatal("commit loss enabled runtime")
			}
		})
	}
}

func TestRecoveryEvidenceAuthorFiniteSlotsAfterEarlyConstraintCheck(t *testing.T) {
	f := evidenceAuthorSetup(t)
	ctx := context.Background()
	for _, changed := range []int{0, 1} {
		t.Run(strconv.Itoa(changed), func(t *testing.T) {
			tx, err := f.target.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			link := f.verified.Bytes()
			h := strings.Repeat("a", 64)
			batch := strings.Repeat("c", 32)
			_, err = tx.Exec(ctx, `INSERT INTO recovery_evidence_batches(id,restore_id,target_installation_id,source_id,selected_snapshot_sha256,selected_receipt_sha256,evidence_receipt_sha256,evidence_snapshot_sha256,evidence_schema_sha256,evidence_cut_at,capture_link,capture_link_sha256,target_catalog_sha256,payload_bytes,chunk_count,changed_rows,report) VALUES($1,$2,$3,$4,$5,$6,$6,$6,$6,clock_timestamp()+interval '1 minute',$7,$8,$6,1,1,$9,'{}')`, batch, f.spec.RestoreID, f.spec.TargetInstallationID, f.spec.SourceID, f.before.SHA256, h, link, Hash(link), changed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, "INSERT INTO recovery_evidence_chunks VALUES($1,0,$2,$3)", batch, []byte{1}, Hash([]byte{1})); err != nil {
				t.Fatal(err)
			}
			if changed == 1 {
				if _, err = tx.Exec(ctx, `INSERT INTO recovery_evidence_changes(batch_id,ordinal,table_name,key_parts,key_sha256,classification,after_sha256) VALUES($1,0,'tasks','["x"]',encode(sha256(convert_to('["x"]','UTF8')),'hex'),'added',$2)`, batch, h); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
				t.Fatal("early completeness", err)
			}
			for _, kind := range []string{"chunk", "change"} {
				if _, err = tx.Exec(ctx, "SAVEPOINT extra"); err != nil {
					t.Fatal(err)
				}
				if kind == "chunk" {
					_, err = tx.Exec(ctx, "INSERT INTO recovery_evidence_chunks VALUES($1,1,$2,$3)", batch, []byte{2}, Hash([]byte{2}))
				} else {
					_, err = tx.Exec(ctx, `INSERT INTO recovery_evidence_changes(batch_id,ordinal,table_name,key_parts,key_sha256,classification,after_sha256) VALUES($1,$2,'tasks','["y"]',encode(sha256(convert_to('["y"]','UTF8')),'hex'),'added',$3)`, batch, changed, h)
				}
				if err == nil {
					t.Fatal("append after completeness accepted", kind)
				}
				if _, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT extra"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

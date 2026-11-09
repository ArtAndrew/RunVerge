// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRecoveryImportAuthorDisconnectAfterSequenceRestart(t *testing.T) {
	recoveryInvariantOwned(t)
	source, _ := testStore(t)
	recoveryAuthorPopulate(t, source)
	ctx := context.Background()
	var exported bytes.Buffer
	if _, err := source.ExportRecoverySnapshot(ctx, &exported, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	archive := recoveryStageAuthorArchive(t, exported.Bytes())
	target, spec, held := recoveryStageAuthorTarget(t, exported.Bytes())
	snapshot, err := recoveryBoundArchive(ctx, archive, spec, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	type sequenceState struct {
		Last   int64
		Called bool
	}
	observe := func() map[string]sequenceState {
		t.Helper()
		result := map[string]sequenceState{}
		for _, seq := range snapshot.Sequences {
			var value sequenceState
			if err := target.Pool.QueryRow(ctx, "SELECT last_value,is_called FROM "+pgx.Identifier{target.runtimeSchema, seq.Name}.Sanitize()).Scan(&value.Last, &value.Called); err != nil {
				t.Fatal(err)
			}
			result[seq.Name] = value
		}
		return result
	}
	before := observe()
	inspection, err := target.InspectRecoveryImport(ctx, spec, snapshot.SHA256)
	if err != nil || inspection.State != "empty_held" || inspection.Receipt != nil {
		t.Fatal("fresh import inspection failed", err)
	}
	connection, err := target.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := connection.Begin(ctx)
	if err != nil {
		connection.Release()
		t.Fatal(err)
	}
	staged, err := target.stageRecoverySnapshotTx(ctx, tx, archive, spec, snapshot, DefaultRecoverySnapshotLimits())
	if err != nil {
		tx.Rollback(ctx)
		connection.Release()
		t.Fatal(err)
	}
	if err = target.importStagedRecoveryTx(ctx, tx, staged, DefaultRecoverySnapshotLimits()); err != nil {
		tx.Rollback(ctx)
		connection.Release()
		t.Fatal(err)
	}
	// The transaction contains every row, marker and receipt; sequences have
	// already been restarted. Disconnect without COMMIT, including after the
	// trusted schema guards have been restored. This is an actual backend session
	// termination, not an injected return value or a promised future rollback.
	closeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	err = connection.Conn().Close(closeCtx)
	stop()
	connection.Release()
	if err != nil {
		t.Fatal("uncommitted connection close failed")
	}
	inspection, err = target.InspectRecoveryImport(ctx, spec, snapshot.SHA256)
	if err != nil || inspection.State != "empty_held" || inspection.Receipt != nil {
		t.Fatal("disconnected import was not fully rolled back", err)
	}
	if !reflect.DeepEqual(observe(), before) {
		t.Fatal("sequence restart escaped data transaction rollback")
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	// A separate explicit attempt follows confirmed inspection, never a blind
	// automatic retry of a commit with an unknown outcome.
	receipt, err := target.ImportRecoverySnapshot(ctx, archive, spec, DefaultRecoverySnapshotLimits())
	if err != nil || receipt.SnapshotSHA256 != snapshot.SHA256 {
		t.Fatal("explicit inspected attempt failed", err)
	}
	inspection, err = target.InspectRecoveryImport(ctx, spec, snapshot.SHA256)
	if err != nil || inspection.State != "committed_held" || inspection.Receipt == nil {
		t.Fatal("acknowledged import lost durable receipt", err)
	}
	if !errors.Is(target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("import enabled ordinary execution")
	}
}

func TestRecoveryImportAuthorMetadataWriteGrantsRejected(t *testing.T) {
	recoveryInvariantOwned(t)
	target, _ := testStore(t)
	ctx := context.Background()
	for name, grant := range map[string]string{
		"receipt_table":  "GRANT INSERT ON recovery_import_receipts TO PUBLIC",
		"marker_table":   "GRANT INSERT ON recovery_historical_rows TO PUBLIC",
		"receipt_column": "GRANT INSERT(target_installation_id) ON recovery_import_receipts TO PUBLIC",
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := target.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, grant); err != nil {
				t.Fatal("owned metadata ACL fault setup failed", err)
			}
			if _, err := validateRecoverySemanticCatalog(ctx, tx, target.runtimeSchema); !errors.Is(err, ErrRecoverySemanticCatalog) {
				t.Fatal("non-owner metadata write grant accepted", err)
			}
		})
	}
	if _, err := target.InspectRecoveryTarget(ctx); err != nil {
		t.Fatal("ACL fault rollback did not restore trusted profile", err)
	}
}

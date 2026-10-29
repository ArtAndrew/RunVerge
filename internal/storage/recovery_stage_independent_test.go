// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Independent stage fault choices/assertions. Shared author helpers supply only
// isolated schema/synthetic data, field extraction and signed envelope assembly.
// This reviewer authored recovery_binary.go separately; these probes are not an
// independent review of that leaf and do not establish productive restoration.
func stageIndependentSource(t *testing.T) (*Store, []byte) {
	t.Helper()
	if os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" || os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("explicit newly owned recovery PostgreSQL cluster required")
	}
	s, _ := recoveryAuthorStore(t)
	recoveryAuthorPopulate(t, s)
	return s, stageIndependentExport(t, s)
}

func stageIndependentExport(t *testing.T, s *Store) []byte {
	t.Helper()
	var out bytes.Buffer
	if _, err := s.ExportRecoverySnapshot(context.Background(), &out, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("independent source export failed", err)
	}
	return out.Bytes()
}

type stageIndependentImage struct {
	Control RecoveryTargetState
	Rows    map[string]int64
	Seq     map[string]string
}

func stageIndependentCapture(t *testing.T, s *Store) stageIndependentImage {
	t.Helper()
	ctx := context.Background()
	// Test-owner observation of our known ordinary control table. Some cases
	// deliberately tamper OTHER catalogue objects, so production Inspect must
	// refuse that catalogue before reading even metadata. Never expose this
	// raw observation as a product unsupported-schema inspection fallback.
	control, err := readRecoveryState(ctx, s.runtimeSchema, s.Pool)
	if err != nil {
		t.Fatal("target control observation failed", err)
	}
	image := stageIndependentImage{Control: control, Rows: map[string]int64{}, Seq: map[string]string{}}
	rows, err := s.Pool.Query(ctx, `SELECT c.relname,c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=$1 AND c.relkind IN('r','S') ORDER BY c.relname COLLATE "C"`, s.runtimeSchema)
	if err != nil {
		t.Fatal(err)
	}
	type relation struct{ name, kind string }
	var relations []relation
	for rows.Next() {
		var v relation
		if err := rows.Scan(&v.name, &v.kind); err != nil {
			t.Fatal(err)
		}
		relations = append(relations, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, v := range relations {
		name := pgx.Identifier{s.runtimeSchema, v.name}.Sanitize()
		if v.kind == "S" {
			var state string
			if err := s.Pool.QueryRow(ctx, "SELECT last_value::text||':'||is_called::text FROM "+name).Scan(&state); err != nil {
				t.Fatal(err)
			}
			image.Seq[v.name] = state
		} else if v.name != "schema_migrations" && v.name != "runtime_recovery_control" && v.name != "recovery_import_receipts" && v.name != "recovery_historical_rows" {
			var count int64
			if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM "+name).Scan(&count); err != nil || count != 0 {
				t.Fatal("held target contains business rows", v.name, err)
			}
			image.Rows[v.name] = count
		}
	}
	if len(image.Rows) != 68 || len(image.Seq) != 2 || image.Control.State != "held" {
		t.Fatal("unexpected target observation coverage", len(image.Rows), len(image.Seq))
	}
	return image
}

func stageIndependentUnchanged(t *testing.T, s *Store, before stageIndependentImage) {
	t.Helper()
	after := stageIndependentCapture(t, s)
	first, _ := json.Marshal(before)
	second, _ := json.Marshal(after)
	if !bytes.Equal(first, second) {
		t.Fatal("validation changed held identity, business rows or sequence state")
	}
	if err := s.CheckRuntimeAdmission(context.Background()); !errors.Is(err, RecoveryHeld) {
		t.Fatal("validation enabled held runtime", err)
	}
}

func TestRecoveryStageIndependentPreexistingTempAndArrayDeclaration(t *testing.T) {
	_, raw := stageIndependentSource(t)
	target, spec, _ := recoveryStageAuthorTarget(t, raw)
	ctx := context.Background()
	// A separate one-connection pool makes the exact backend/temp ownership
	// deterministic; no assumptions about scheduling or pool LIFO are needed.
	config := target.Pool.Config()
	config.MinConns, config.MaxConns = 1, 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("single owned validation backend unavailable")
	}
	single := &Store{Pool: pool, runtimeSchema: target.runtimeSchema}
	t.Cleanup(single.Close)
	before := stageIndependentCapture(t, single)
	qualified := pgx.Identifier{target.runtimeSchema, "reservations"}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE TEMP TABLE array_declaration_probe (LIKE "+qualified+" INCLUDING CONSTRAINTS INCLUDING INDEXES EXCLUDING DEFAULTS EXCLUDING GENERATED EXCLUDING IDENTITY)"); err != nil {
		t.Fatal(err)
	}
	var sourceDims, copiedDims int16
	var sourceOID, copiedOID uint32
	if err := pool.QueryRow(ctx, `SELECT s.attndims,t.attndims,s.atttypid,t.atttypid
 FROM pg_catalog.pg_attribute s JOIN pg_catalog.pg_attribute t ON t.attname=s.attname
 WHERE s.attrelid=$1::regclass AND t.attrelid='pg_temp.array_declaration_probe'::regclass AND s.attname='scope_task_ids'`, qualified).Scan(&sourceDims, &copiedDims, &sourceOID, &copiedOID); err != nil || sourceDims != 1 || copiedDims != 0 || sourceOID != 1009 || copiedOID != 1009 {
		t.Fatal("PG18 LIKE declaration observation differs", sourceDims, copiedDims, sourceOID, copiedOID, err)
	}
	if _, err := pool.Exec(ctx, "CREATE TEMP TABLE unrelated_temp_sentinel(value text); INSERT INTO unrelated_temp_sentinel VALUES('preserve')"); err != nil {
		t.Fatal(err)
	}
	verified := recoveryStageAuthorArchive(t, raw)
	if report, err := single.CheckRecoverySnapshotData(ctx, verified, spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryStageTarget) || report.RollbackConfirmed {
		t.Fatal("validation adopted preexisting temporary objects", err)
	}
	var sentinel string
	if err := pool.QueryRow(ctx, "SELECT value FROM pg_temp.unrelated_temp_sentinel").Scan(&sentinel); err != nil || sentinel != "preserve" {
		t.Fatal("validation deleted or rewrote another temporary object", err)
	}
	stageIndependentUnchanged(t, single, before)
}

func TestRecoveryStageIndependentCatalogTamperBeforeCanaryInvocation(t *testing.T) {
	_, raw := stageIndependentSource(t)
	target, spec, _ := recoveryStageAuthorTarget(t, raw)
	ctx := context.Background()
	fn := pgx.Identifier{target.runtimeSchema, "stage_import_canary"}.Sanitize()
	seq := pgx.Identifier{target.runtimeSchema, "events_sequence_seq"}.Sanitize()
	// The function is intentionally side-effecting: even a rolled-back call
	// would advance the owned target sequence. Tampered catalogue must be refused
	// before LIKE/COPY could evaluate this new CHECK against plaintext rows.
	if _, err := target.Pool.Exec(ctx, "CREATE FUNCTION "+fn+"(text) RETURNS boolean LANGUAGE plpgsql AS $$BEGIN PERFORM pg_catalog.nextval('"+seq+"'::pg_catalog.regclass); RETURN true; END$$"); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Pool.Exec(ctx, "ALTER TABLE "+pgx.Identifier{target.runtimeSchema, "organizations"}.Sanitize()+" ADD CONSTRAINT independent_canary CHECK ("+fn+"(name)) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	before := stageIndependentCapture(t, target)
	verified := recoveryStageAuthorArchive(t, raw)
	if report, err := target.CheckRecoverySnapshotData(ctx, verified, spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoverySemanticCatalog) || report.RollbackConfirmed {
		t.Fatal("tampered semantic catalogue reached row execution", err)
	}
	stageIndependentUnchanged(t, target, before)
}

func TestRecoveryStageIndependentPartialUniqueIndex(t *testing.T) {
	_, raw := stageIndependentSource(t)
	changed := recoveryStageAuthorRewrite(t, raw, func(f *recoveryStageAuthorFixture) {
		if len(f.rows["model_connections"]) != 1 {
			t.Fatal("fixture must contain one active model")
		}
		original := f.rows["model_connections"][0]
		duplicate := make([][]byte, len(original))
		for i, value := range original {
			duplicate[i] = bytes.Clone(value)
		}
		duplicate[recoveryAuthorColumn(t, f.header, "model_connections", "id")] = []byte("zz_independent_duplicate_model")
		f.rows["model_connections"] = append(f.rows["model_connections"], duplicate)
	})
	target, spec, _ := recoveryStageAuthorTarget(t, changed)
	before := stageIndependentCapture(t, target)
	verified := recoveryStageAuthorArchive(t, changed)
	if report, err := target.CheckRecoverySnapshotData(context.Background(), verified, spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryStageData) || report.RollbackConfirmed {
		t.Fatal("non-PK partial unique conflict was admitted", err)
	}
	stageIndependentUnchanged(t, target, before)
}

func TestRecoveryStageIndependentActualGeneratedValueMismatch(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" || os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("explicit newly owned recovery PostgreSQL cluster required")
	}
	f := independentToolSetup(t, false)
	f.prepare(t) // Stored explicit intent only; no tool/model request or dispatch.
	raw := stageIndependentExport(t, f.s)
	target, spec, _ := recoveryStageAuthorTarget(t, raw)
	before := stageIndependentCapture(t, target)
	verified := recoveryStageAuthorArchive(t, raw)
	if report, err := target.CheckRecoverySnapshotData(context.Background(), verified, spec, DefaultRecoverySnapshotLimits()); err != nil || !report.RollbackConfirmed || report.GeneratedChecked != 2 {
		t.Fatal("genuine prepared-tool baseline must validate first", err)
	}
	changed := recoveryStageAuthorRewrite(t, raw, func(v *recoveryStageAuthorFixture) {
		if len(v.rows["tool_executions"]) != 1 {
			t.Fatal("fixture needs one real generated-field row")
		}
		column := recoveryAuthorColumn(t, v.header, "tool_executions", "model_proposal_id")
		if v.rows["tool_executions"][0][column] != nil {
			t.Fatal("human-prepared baseline should have no model proposal")
		}
		v.rows["tool_executions"][0][column] = []byte(domain.NewID("proposal"))
	})
	corrupt := recoveryStageAuthorArchive(t, changed)
	if report, err := target.CheckRecoverySnapshotData(context.Background(), corrupt, spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryStageGenerated) || report.RollbackConfirmed {
		t.Fatal("archived generated value was trusted instead of recomputed", err)
	}
	stageIndependentUnchanged(t, target, before)
}

func TestRecoveryStageIndependentCancellationAfterActualGateWait(t *testing.T) {
	_, raw := stageIndependentSource(t)
	target, spec, _ := recoveryStageAuthorTarget(t, raw)
	before := stageIndependentCapture(t, target)
	verified := recoveryStageAuthorArchive(t, raw)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	blocker, err := target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext($1))", target.runtimeSchema); err != nil {
		t.Fatal(err)
	}
	validation, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := target.CheckRecoverySnapshotData(validation, verified, spec, DefaultRecoverySnapshotLimits())
		done <- err
	}()
	independentRecoveryWaitAdvisory(t, target, ctx, 0)
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrRecoveryStageCleanup) {
			t.Fatal("cancelled waiter did not fail closed", err)
		}
	case <-ctx.Done():
		t.Fatal("validation did not return after cancellation")
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	stageIndependentUnchanged(t, target, before)
	// The borrowed archive must be detached after the cancelled call returns.
	if _, err := verified.Seek(0, io.SeekStart); err != nil {
		t.Fatal("cancelled validation retained archive ownership", err)
	}
}

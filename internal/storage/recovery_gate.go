// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RecoveryHeld is deliberately not retryable. An operator must resolve recovery;
// normal admission cannot clear the hold or make an old queue safe to execute.
var RecoveryHeld = &domain.Error{Code: "unavailable", Message: "Recovery target is held; ordinary access and execution are disabled", Status: 503}

func recoveryGateError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && (pgerr.Code == "55000" || pgerr.Code == "42883" || pgerr.Code == "42P01") {
		return RecoveryHeld
	}
	return err
}

// beginRuntime is the first lock in normal transactions, before IAM/tree/money.
// Migration, snapshot export and fresh-target preparation use explicit separate
// host paths; they are not a context flag that callers can use as a bypass.
func (s *Store) beginRuntime(ctx context.Context) (pgx.Tx, error) {
	return s.beginRuntimeTx(ctx, pgx.TxOptions{})
}

func (s *Store) beginRuntimeTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := s.Pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "SELECT "+pgx.Identifier{s.runtimeSchema, "runtime_recovery_admit"}.Sanitize()+"($1)", options.AccessMode == pgx.ReadOnly); err != nil {
		_ = tx.Rollback(ctx)
		return nil, recoveryGateError(err)
	}
	return tx, nil
}

// CheckRuntimeAdmission is a read-only preflight, not a permit for subsequent
// writes. Actual transactions recheck and hold the gate until their commit.
func (s *Store) CheckRuntimeAdmission(ctx context.Context) error {
	tx, err := s.beginRuntimeTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	return tx.Commit(ctx)
}

func (s *Store) execRuntime(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

type RecoveryTargetSpec struct {
	TargetInstallationID string
	RestoreID            string
	BackupID             string
	SourceID             string
	CutUTC               string
	SchemaSHA256         string
}

type RecoveryTargetState struct {
	InstallationID string    `json:"installation_id"`
	State          string    `json:"state"`
	RestoreID      string    `json:"restore_id,omitempty"`
	BackupID       string    `json:"backup_id,omitempty"`
	SourceID       string    `json:"source_id,omitempty"`
	CutUTC         string    `json:"cut_utc,omitempty"`
	SchemaSHA256   string    `json:"schema_sha256,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	HeldAt         time.Time `json:"held_at,omitempty"`
}

var recoveryID = regexp.MustCompile(`^(restore|bak|inst)_[a-f0-9]{32}$`)

func validRecoveryID(value, prefix string) bool {
	return len(value) == len(prefix)+33 && value[:len(prefix)] == prefix && recoveryID.MatchString(value)
}

// EmbeddedSchemaSHA256 binds the ordered, trusted migration list. It is not a
// claim that a connected database has applied this exact schema.
func EmbeddedSchemaSHA256() string {
	raw, _ := json.Marshal(EmbeddedSchemaIdentity())
	return Hash(raw)
}

func verifyRecoverySchemaTx(ctx context.Context, tx pgx.Tx, schema string) error {
	rows, err := tx.Query(ctx, "SELECT name,sha256 FROM "+pgx.Identifier{schema, "schema_migrations"}.Sanitize()+" ORDER BY name")
	if err != nil {
		return err
	}
	defer rows.Close()
	expected := EmbeddedSchemaIdentity()
	i := 0
	for rows.Next() {
		var name, digest string
		if err := rows.Scan(&name, &digest); err != nil {
			return err
		}
		if i >= len(expected) || expected[i].Name != name || expected[i].SHA256 != digest {
			return domain.Err("conflict", "Recovery requires the exact trusted schema", 409)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if i != len(expected) {
		return domain.Err("conflict", "Recovery requires the exact trusted schema", 409)
	}
	return nil
}

func readRecoveryState(ctx context.Context, schema string, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (RecoveryTargetState, error) {
	var state RecoveryTargetState
	var cut, held *time.Time
	err := query.QueryRow(ctx, `SELECT installation_id,state,COALESCE(restore_id,''),COALESCE(backup_id,''),COALESCE(source_id,''),cut_at,COALESCE(schema_sha256,''),created_at,held_at FROM `+pgx.Identifier{schema, "runtime_recovery_control"}.Sanitize()+` WHERE singleton`).Scan(
		&state.InstallationID, &state.State, &state.RestoreID, &state.BackupID, &state.SourceID, &cut, &state.SchemaSHA256, &state.CreatedAt, &held)
	if err != nil {
		return RecoveryTargetState{}, recoveryGateError(err)
	}
	if cut != nil {
		state.CutUTC = cut.UTC().Format(time.RFC3339Nano)
	}
	if held != nil {
		state.HeldAt = held.UTC()
	}
	return state, nil
}

// InspectRecoveryTarget is an explicit trusted-host metadata read. It returns
// no tenant data, keys or credentials and is not exposed through normal HTTP.
func (s *Store) InspectRecoveryTarget(ctx context.Context) (RecoveryTargetState, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return RecoveryTargetState{}, err
	}
	defer tx.Rollback(context.Background())
	// Host inspection must not execute a substituted metadata view or type's
	// output routine. The complete catalogue is checked before relation reads.
	if _, err = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); err != nil {
		return RecoveryTargetState{}, err
	}
	state, err := readRecoveryState(ctx, s.runtimeSchema, tx)
	if err != nil {
		return RecoveryTargetState{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RecoveryTargetState{}, err
	}
	return state, nil
}

// PrepareRecoveryTarget only holds an already separately provisioned EMPTY
// target. It creates no database, imports no data and grants no new authority.
// No resume or import bypass is implemented by this operation. A lost commit
// acknowledgment must be inspected using the exact IDs, never blindly retried.
func (s *Store) PrepareRecoveryTarget(ctx context.Context, spec RecoveryTargetSpec) (RecoveryTargetState, error) {
	var empty RecoveryTargetState
	cut, err := time.Parse(time.RFC3339Nano, spec.CutUTC)
	if err != nil || cut.UTC().Format(time.RFC3339Nano) != spec.CutUTC || cut.Nanosecond()%1000 != 0 ||
		!validRecoveryID(spec.TargetInstallationID, "inst") ||
		!validRecoveryID(spec.RestoreID, "restore") || !validRecoveryID(spec.BackupID, "bak") ||
		!validRecoveryID(spec.SourceID, "inst") || spec.SchemaSHA256 != EmbeddedSchemaSHA256() {
		return empty, domain.Err("invalid_request", "Invalid recovery target specification", 400)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	// Migration admission comes first for all trusted recovery host paths.
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); err != nil {
		return empty, err
	}
	// This is an exclusive first acquisition, never an upgrade of a runtime
	// shared lock. Runtime transactions and SQL fallback guards use the same key.
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext($1))", s.runtimeSchema); err != nil {
		return empty, err
	}
	if _, err = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); err != nil {
		return empty, err
	}
	var guarded bool
	err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=$1 AND c.relkind IN('r','p') AND c.relname NOT IN('runtime_recovery_control','schema_migrations')
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_proc f ON f.oid=t.tgfoid WHERE t.tgrelid=c.oid
 AND t.tgname='recovery_runtime_write' AND t.tgenabled='O' AND t.tgtype=62 AND NOT t.tgisinternal
 AND f.proname='runtime_recovery_write_guard' AND f.pronamespace=c.relnamespace))`, s.runtimeSchema).Scan(&guarded)
	if err != nil {
		return empty, err
	}
	if !guarded {
		return empty, domain.Err("conflict", "Recovery target guards are incomplete", 409)
	}
	before, err := readRecoveryState(ctx, s.runtimeSchema, tx)
	if err != nil {
		return empty, err
	}
	if before.State != "normal" || before.InstallationID != spec.TargetInstallationID || before.InstallationID == spec.SourceID {
		return empty, domain.Err("conflict", "Recovery target is not a fresh distinct installation", 409)
	}
	// The trusted SQL transition guard checks all business tables for emptiness
	// under the exclusive gate, before changing the control row.
	_, err = tx.Exec(ctx, `UPDATE `+pgx.Identifier{s.runtimeSchema, "runtime_recovery_control"}.Sanitize()+` SET state='held',restore_id=$1,backup_id=$2,source_id=$3,cut_at=$4,schema_sha256=$5 WHERE singleton`,
		spec.RestoreID, spec.BackupID, spec.SourceID, cut, spec.SchemaSHA256)
	if err != nil {
		return empty, recoveryGateError(err)
	}
	state, err := readRecoveryState(ctx, s.runtimeSchema, tx)
	if err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return state, nil
}

// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrRecoveryStageTarget     = errors.New("recovery validation requires the exact empty held target")
	ErrRecoveryStageSchema     = errors.New("unsupported recovery validation schema")
	ErrRecoveryStageData       = errors.New("recovery data violates trusted row constraints")
	ErrRecoveryStageReferences = errors.New("recovery data violates trusted references")
	ErrRecoveryStageGenerated  = errors.New("recovery generated values differ")
	ErrRecoveryStageState      = errors.New("recovery data and snapshot state differ")
	ErrRecoveryStageCleanup    = errors.New("recovery validation rollback was not confirmed")
)

// RecoveryStageReport describes only rollback-only validation. It is neither
// an import receipt nor authorization to enable work or reuse archived access.
type RecoveryStageReport struct {
	Scope                  string                 `json:"scope"`
	TargetInstallationID   string                 `json:"target_installation_id"`
	RestoreID              string                 `json:"restore_id"`
	SemanticCatalogSHA256  string                 `json:"semantic_catalog_sha256"`
	Snapshot               RecoverySnapshotReport `json:"snapshot"`
	ForeignKeysChecked     int                    `json:"foreign_keys_checked"`
	GeneratedChecked       int                    `json:"generated_checked"`
	RollbackConfirmed      bool                   `json:"rollback_confirmed"`
	FullSemanticValidation bool                   `json:"full_semantic_validation"`
}

// CheckRecoverySnapshotData authenticates no key itself: input must come from
// recoveryarchive.Open with independently selected keys/metadata. This method
// binds the inner snapshot to that authenticated envelope, then checks data in
// private PostgreSQL temporary tables. Every exit rolls back; no target business
// table, trigger, constraint or sequence is written or altered. The caller owns
// and serializes use of archive and must close it. PostgreSQL staging is private
// database operational data, subject to its host/disk protection.
// Nondeclarative invariant coverage is partial: profile/intent hashes, complete
// allocation/quota and webhook projections still require additional validators.
// A successful stage is never full semantic-import acceptance.
func (s *Store) CheckRecoverySnapshotData(ctx context.Context, archive *recoveryarchive.VerifiedArchive, spec RecoveryTargetSpec, limits RecoverySnapshotLimits) (result RecoveryStageReport, resultErr error) {
	if archive == nil || !limits.valid() {
		return result, ErrRecoverySnapshotInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotIO)
	}
	snapshot, err := ValidateRecoverySnapshot(ctx, archive, limits)
	if err != nil {
		return result, err
	}
	manifest := archive.Manifest()
	m := snapshot.Metadata
	if manifest.Metadata != (recoveryarchive.Metadata{BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}) ||
		manifest.PlaintextSHA256 != snapshot.SHA256 || manifest.PlaintextBytes != snapshot.WireBytes ||
		spec.BackupID != m.BackupID || spec.SourceID != m.SourceID || spec.CutUTC != m.CutUTC || spec.SchemaSHA256 != m.SchemaSHA256 ||
		!validRecoveryID(spec.TargetInstallationID, "inst") || !validRecoveryID(spec.RestoreID, "restore") || spec.TargetInstallationID == m.SourceID {
		return result, ErrRecoverySnapshotInvalid
	}

	connection, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageTarget)
	}
	defer connection.Release()
	tx, err := connection.Begin(ctx)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageTarget)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := tx.Rollback(cleanup); err != nil {
			// A connection with an unconfirmed transaction must never return to
			// the pool for ordinary use. This stage performs no durable writes.
			_ = tx.Conn().Close(cleanup)
			result, resultErr = RecoveryStageReport{}, ErrRecoveryStageCleanup
			return
		}
		if resultErr == nil {
			result.RollbackConfirmed = true
		}
	}()
	if _, err = tx.Exec(ctx, "SET LOCAL search_path=pg_catalog,pg_temp"); err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageSchema)
	}
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.set_config('statement_timeout',$1,true),pg_catalog.set_config('lock_timeout','5000',true)", strconv.FormatInt(max(1, limits.MaxDuration.Milliseconds()), 10)); err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageSchema)
	}
	// Migrations take this gate first. Normal application admission is separate.
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageTarget)
	}
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext($1))", s.runtimeSchema); err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageTarget)
	}
	// Even metadata-looking application relations could have been replaced by
	// executable views/RLS/custom types. Verify only pg_catalog structure first;
	// the catalogue validator reads migration rows only after its complete pin.
	catalog, err := validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema)
	if err != nil {
		return result, err
	}
	held, err := readRecoveryState(ctx, s.runtimeSchema, tx)
	if err != nil || held.State != "held" || held.InstallationID != spec.TargetInstallationID || held.RestoreID != spec.RestoreID || held.BackupID != m.BackupID || held.SourceID != m.SourceID || held.CutUTC != m.CutUTC || held.SchemaSHA256 != m.SchemaSHA256 {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageTarget)
	}
	if _, err = tx.Exec(ctx, "SET LOCAL row_security=off"); err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageSchema)
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		return result, err
	}
	encoded, _ := json.Marshal(inventory)
	if Hash(encoded) != snapshot.InventorySHA256 || snapshot.InventorySHA256 != recoverySnapshotInventorySHA256 {
		return result, ErrRecoveryStageSchema
	}
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		var populated bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{s.runtimeSchema, table.Name}.Sanitize()+" LIMIT 1)").Scan(&populated); err != nil || populated {
			return result, recoverySnapshotError(ctx, ErrRecoveryStageTarget)
		}
	}
	var occupied bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_class WHERE relnamespace=pg_catalog.pg_my_temp_schema())").Scan(&occupied); err != nil || occupied {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageTarget)
	}
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		// These are deliberately enumerated LIKE options, never INCLUDING ALL.
		// COPY must accept historical generated/identity values as ordinary data.
		ddl := "CREATE TEMP TABLE " + pgx.Identifier{"pg_temp", table.Name}.Sanitize() + " (LIKE " + pgx.Identifier{s.runtimeSchema, table.Name}.Sanitize() + " INCLUDING CONSTRAINTS INCLUDING INDEXES EXCLUDING DEFAULTS EXCLUDING GENERATED EXCLUDING IDENTITY) ON COMMIT DROP"
		if _, err = tx.Exec(ctx, ddl); err != nil {
			return result, recoveryStageSchemaFailure(ctx, "temporary_table", err)
		}
	}
	var stageSchema string
	if err = tx.QueryRow(ctx, "SELECT nspname FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.pg_my_temp_schema()").Scan(&stageSchema); err != nil || stageSchema == "" {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageSchema)
	}
	if err = recoveryStageCheckShape(ctx, tx, s.runtimeSchema, stageSchema, inventory); err != nil {
		return result, err
	}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotIO)
	}
	if err = recoveryStageCopy(ctx, tx, archive, stageSchema, inventory, snapshot, limits); err != nil {
		return result, err
	}
	// A copied deferrable UNIQUE constraint must actually be flushed even
	// though this transaction is intentionally never committed.
	if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryStageData)
	}
	// Re-export the typed rows in canonical primary-key order. PostgreSQL may
	// normalize binary input: neither that nor a lost/extra row may go unnoticed.
	if err = recoveryStageCheckDigests(ctx, tx, stageSchema, inventory, snapshot, limits); err != nil {
		return result, err
	}
	generated := 0
	for _, column := range catalog.Columns {
		if column.Generated == "" {
			continue
		}
		var mismatch bool
		sql := "SELECT EXISTS(SELECT 1 FROM " + pgx.Identifier{stageSchema, column.Table}.Sanitize() + " WHERE " + pgx.Identifier{column.Name}.Sanitize() + " IS DISTINCT FROM (" + column.RawExpression + "))"
		if err = tx.QueryRow(ctx, sql).Scan(&mismatch); err != nil || mismatch {
			return result, recoverySnapshotError(ctx, ErrRecoveryStageGenerated)
		}
		generated++
	}
	fks, err := recoveryStageCheckReferences(ctx, tx, s.runtimeSchema, stageSchema, inventory, catalog)
	if err != nil {
		return result, err
	}
	if err = recoveryStageCheckState(ctx, tx, stageSchema, snapshot, limits); err != nil {
		return result, err
	}
	if err = validateRecoveryInvariants(ctx, tx, stageSchema); err != nil {
		return result, err
	}
	return RecoveryStageReport{Scope: "rollback_only_typed_stage", TargetInstallationID: held.InstallationID, RestoreID: held.RestoreID, SemanticCatalogSHA256: recoverySemanticCatalogSHA256, Snapshot: snapshot, ForeignKeysChecked: fks, GeneratedChecked: generated}, nil
}

func recoveryStageCheckShape(ctx context.Context, tx pgx.Tx, target, stage string, inventory RecoverySnapshotInventory) error {
	// LIKE preserves the binary array type but not its declared attndims
	// annotation. PostgreSQL does not enforce that annotation on values; the
	// bounded binary reader and full row-byte round trip validate actual arrays.
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		var valid bool
		err := tx.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=$2 AND c.relname=$3 AND a.attnum>0 AND NOT a.attisdropped AND (a.atthasdef OR a.attidentity<>'' OR a.attgenerated<>''))
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$2 AND c.relname=$3 AND NOT t.tgisinternal)
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_constraint co JOIN pg_catalog.pg_class c ON c.oid=co.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$2 AND c.relname=$3 AND co.contype='f')
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$2 AND c.relname=$3 AND (NOT i.indisvalid OR NOT i.indisready OR NOT i.indislive))
 AND (SELECT count(*) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$3)
   = (SELECT count(*) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$2 AND c.relname=$3)
 AND NOT EXISTS((SELECT a.attname,a.atttypid,a.atttypmod,a.attcollation,a.attnotnull FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$3 AND a.attnum>0 AND NOT a.attisdropped)
 EXCEPT (SELECT a.attname,a.atttypid,a.atttypmod,a.attcollation,a.attnotnull FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$2 AND c.relname=$3 AND a.attnum>0 AND NOT a.attisdropped))`, target, stage, table.Name).Scan(&valid)
		if err != nil || !valid {
			return recoveryStageSchemaFailure(ctx, "temporary_shape_"+table.Name, err)
		}
	}
	return nil
}

// Diagnostics contain only a fixed rule and a PostgreSQL SQLSTATE, never the
// database's Detail/Context/Message, SQL, paths, identifiers or row values.
func recoveryStageSchemaFailure(ctx context.Context, rule string, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var pgerr *pgconn.PgError
	if errors.As(cause, &pgerr) && len(pgerr.Code) == 5 {
		return fmt.Errorf("%s (%s): %w", rule, pgerr.Code, ErrRecoveryStageSchema)
	}
	return fmt.Errorf("%s: %w", rule, ErrRecoveryStageSchema)
}

func recoveryStageCheckReferences(ctx context.Context, tx pgx.Tx, target, stage string, inventory RecoverySnapshotInventory, catalog RecoverySemanticCatalog) (int, error) {
	tables := map[string]bool{}
	for _, table := range inventory.Tables {
		if table.Exported {
			tables[table.Name] = true
		}
	}
	checked := 0
	for _, fk := range catalog.Constraints {
		if fk.Type != "f" || !tables[fk.Table] {
			continue
		}
		if !tables[fk.RefTable] || fk.RefSchema != recoverySemanticNamespace || len(fk.Keys) == 0 || len(fk.Keys) != len(fk.RefKeys) || (fk.MatchType != "s" && fk.MatchType != "f") {
			return 0, ErrRecoveryStageSchema
		}
		nonnull, anynull, matches := []string{}, []string{}, []string{}
		for i, key := range fk.Keys {
			local := "c." + pgx.Identifier{key}.Sanitize()
			nonnull = append(nonnull, local+" IS NOT NULL")
			anynull = append(anynull, local+" IS NULL")
			matches = append(matches, local+"=p."+pgx.Identifier{fk.RefKeys[i]}.Sanitize())
		}
		bad := "(" + strings.Join(nonnull, " AND ") + ") AND NOT EXISTS(SELECT 1 FROM " + pgx.Identifier{stage, fk.RefTable}.Sanitize() + " p WHERE " + strings.Join(matches, " AND ") + ")"
		if fk.MatchType == "f" {
			bad = "(" + bad + ") OR ((" + strings.Join(anynull, " OR ") + ") AND NOT(" + strings.Join(anynull, " AND ") + "))"
		}
		var invalid bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{stage, fk.Table}.Sanitize()+" c WHERE "+bad+")").Scan(&invalid); err != nil || invalid {
			return 0, recoverySnapshotError(ctx, ErrRecoveryStageReferences)
		}
		checked++
	}
	return checked, nil
}

func recoveryStageCheckState(ctx context.Context, tx pgx.Tx, schema string, snapshot RecoverySnapshotReport, limits RecoverySnapshotLimits) error {
	// These ordinary temp rows retain the source's signed sequence observations;
	// ReadState independently recomputes the maxima from all staged data. No
	// nextval/setval or target sequence change is performed.
	for _, seq := range snapshot.Sequences {
		q := pgx.Identifier{schema, seq.Name}.Sanitize()
		if _, err := tx.Exec(ctx, "CREATE TEMP TABLE "+q+"(last_value bigint NOT NULL,is_called boolean NOT NULL) ON COMMIT DROP"); err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryStageSchema)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO "+q+" VALUES($1,$2)", seq.ObservedLast, seq.IsCalled); err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryStageState)
		}
	}
	state, err := recoverySnapshotReadState(ctx, tx, schema, limits)
	if err != nil || !reflect.DeepEqual(state.Sequences, snapshot.Sequences) || !reflect.DeepEqual(state.KeyReferences, snapshot.KeyReferences) || !reflect.DeepEqual(state.Organizations, snapshot.Organizations) {
		return recoverySnapshotError(ctx, ErrRecoveryStageState)
	}
	return nil
}

func recoveryStageReader(ctx context.Context, in io.Reader, limit int64) *recoverySnapshotReader {
	return &recoverySnapshotReader{ctx: ctx, reader: bufio.NewReaderSize(io.LimitReader(in, limit+1), 64<<10), hash: sha256.New(), limit: limit}
}

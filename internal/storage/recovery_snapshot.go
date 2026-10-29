// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"regexp"
	"time"

	"agentplatform.local/platform/internal/installation"
	"github.com/jackc/pgx/v5"
)

const RecoverySnapshotFormat = "runverge-pg18-copy-ndjson-v1"

// Recovery metadata is target-local authority/fencing, never source business data.
func recoveryBusinessTable(name string) bool {
	switch name {
	case "schema_migrations", "runtime_recovery_control", "recovery_import_receipts", "recovery_historical_rows":
		return false
	}
	return true
}

// Filled only from a disposable schema built with trusted embedded migrations.
// An embedded migration change intentionally fails closed until this typed
// inventory pin has been reviewed and the compatibility test updated.
const recoverySnapshotInventorySHA256 = "f6df86ff20c447e67132a5282b46a20aa2ccadecdd994767205ffb30c3362700"

const recoverySnapshotChunkSize = 32 << 10
const recoverySnapshotMaxRecordBytes = 2 << 20

var (
	ErrRecoverySnapshotInvalid    = errors.New("invalid recovery snapshot")
	ErrRecoverySnapshotLimit      = errors.New("recovery snapshot limit exceeded")
	ErrRecoverySnapshotSchema     = errors.New("unsupported recovery snapshot schema")
	ErrRecoverySnapshotSource     = errors.New("recovery snapshot source unavailable or held")
	ErrRecoverySnapshotIO         = errors.New("recovery snapshot input or output failed")
	recoverySnapshotName          = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)
	recoverySnapshotSourceID      = regexp.MustCompile(`^inst_[a-f0-9]{32}$`)
	recoverySnapshotBackupID      = regexp.MustCompile(`^bak_[a-f0-9]{32}$`)
	recoverySnapshotDigest        = regexp.MustCompile(`^[a-f0-9]{64}$`)
	recoverySnapshotCredentialRef = regexp.MustCompile(`^PLATFORM_(MODEL|TOOL|MCP|WEBHOOK)_KEY_[A-Z0-9_]{1,64}$`)
)

// Bounds apply to decoded COPY bytes, encoded wire bytes, rows, references and
// database time. A caller-supplied Reader/Writer must be bounded and cooperate
// with cancellation: an arbitrary blocking io.Reader/io.Writer cannot be
// interrupted safely by this function.
type RecoverySnapshotLimits struct {
	MaxTotalBytes      int64
	MaxTableBytes      int64
	MaxRowBytes        int64
	MaxWireBytes       int64
	MaxRows            int64
	MaxRowsPerTable    int64
	MaxMetadataEntries int
	MaxDuration        time.Duration
}

func DefaultRecoverySnapshotLimits() RecoverySnapshotLimits {
	return RecoverySnapshotLimits{
		MaxTotalBytes: 256 << 20, MaxTableBytes: 128 << 20, MaxRowBytes: 1 << 20,
		MaxWireBytes: 384 << 20, MaxRows: 1_000_000, MaxRowsPerTable: 500_000,
		MaxMetadataEntries: 10_000, MaxDuration: 10 * time.Minute,
	}
}

func (l RecoverySnapshotLimits) valid() bool {
	return l.MaxRowBytes >= 32 && l.MaxRowBytes <= 16<<20 &&
		l.MaxTableBytes >= l.MaxRowBytes && l.MaxTableBytes <= 4<<30 &&
		l.MaxTotalBytes >= l.MaxTableBytes && l.MaxTotalBytes <= 16<<30 &&
		l.MaxWireBytes >= l.MaxTotalBytes && l.MaxWireBytes <= 24<<30 &&
		l.MaxRowsPerTable > 0 && l.MaxRowsPerTable <= 10_000_000 &&
		l.MaxRows >= l.MaxRowsPerTable && l.MaxRows <= 50_000_000 &&
		l.MaxMetadataEntries > 0 && l.MaxMetadataEntries <= 10_000 &&
		l.MaxDuration > 0 && l.MaxDuration <= time.Hour
}

type RecoverySnapshotMetadata struct {
	SourceID     string `json:"source_id"`
	CutUTC       string `json:"cut_utc"`
	BackupID     string `json:"backup_id"`
	SchemaSHA256 string `json:"schema_sha256"`
}

type RecoverySnapshotColumn struct {
	Name         string `json:"name"`
	Number       int16  `json:"number"`
	TypeOID      uint32 `json:"type_oid"`
	TypeName     string `json:"type_name"`
	TypeModifier int32  `json:"type_modifier"`
	Dimensions   int16  `json:"dimensions"`
	NotNull      bool   `json:"not_null"`
	Generated    string `json:"generated"`
	Identity     string `json:"identity"`
}

type RecoverySnapshotTable struct {
	Name       string                   `json:"name"`
	Exported   bool                     `json:"exported"`
	Columns    []RecoverySnapshotColumn `json:"columns"`
	PrimaryKey []string                 `json:"primary_key"`
}

type RecoverySnapshotSequenceDefinition struct {
	Name      string `json:"name"`
	TypeOID   uint32 `json:"type_oid"`
	Start     int64  `json:"start,string"`
	Minimum   int64  `json:"minimum,string"`
	Maximum   int64  `json:"maximum,string"`
	Increment int64  `json:"increment,string"`
	Cache     int64  `json:"cache,string"`
	Cycle     bool   `json:"cycle"`
}

type RecoverySnapshotInventory struct {
	Tables    []RecoverySnapshotTable              `json:"tables"`
	Sequences []RecoverySnapshotSequenceDefinition `json:"sequences"`
}

type RecoverySnapshotTableSummary struct {
	Name   string `json:"name"`
	Rows   int64  `json:"rows,string"`
	Bytes  int64  `json:"bytes,string"`
	SHA256 string `json:"sha256"`
}

type RecoverySnapshotSequence struct {
	Name            string `json:"name"`
	ObservedLast    int64  `json:"observed_last,string"`
	IsCalled        bool   `json:"is_called"`
	IncludedMaximum int64  `json:"included_maximum,string"`
	NextMinimum     int64  `json:"next_minimum,string"`
}

type RecoverySnapshotKeyReference struct {
	Kind           string `json:"kind"`
	OrganizationID string `json:"organization_id"`
	ID             string `json:"id"`
	Version        int    `json:"version"`
	Environment    string `json:"environment"`
}

type RecoverySnapshotOrganization struct {
	OrganizationID       string `json:"organization_id"`
	EventHead            int64  `json:"event_head,string"`
	RetainedAfterMaximum int64  `json:"retained_after_maximum,string"`
}

// Report contains no row payloads, credentials or database connection strings.
// Key names and tenant IDs remain private operational metadata.
type RecoverySnapshotReport struct {
	Metadata        RecoverySnapshotMetadata       `json:"metadata"`
	Format          string                         `json:"format"`
	InventorySHA256 string                         `json:"inventory_sha256"`
	Tables          []RecoverySnapshotTableSummary `json:"tables"`
	Sequences       []RecoverySnapshotSequence     `json:"sequences"`
	KeyReferences   []RecoverySnapshotKeyReference `json:"key_references"`
	Organizations   []RecoverySnapshotOrganization `json:"organizations"`
	Rows            int64                          `json:"rows,string"`
	Bytes           int64                          `json:"bytes,string"`
	WireBytes       int64                          `json:"wire_bytes,string"`
	SHA256          string                         `json:"sha256"`
}

type recoverySnapshotHeader struct {
	Kind              string                    `json:"kind"`
	Format            string                    `json:"format"`
	Metadata          RecoverySnapshotMetadata  `json:"metadata"`
	APIVersion        string                    `json:"api_version"`
	PostgreSQLVersion int                       `json:"postgresql_version"`
	Encoding          string                    `json:"encoding"`
	MVCCSnapshot      string                    `json:"mvcc_snapshot"`
	Migrations        []installation.Migration  `json:"migrations"`
	InventorySHA256   string                    `json:"inventory_sha256"`
	Inventory         RecoverySnapshotInventory `json:"inventory"`
}

type recoverySnapshotTableStart struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type recoverySnapshotChunk struct {
	Kind string `json:"kind"`
	Data string `json:"data"`
}
type recoverySnapshotTableEnd struct {
	Kind    string                       `json:"kind"`
	Summary RecoverySnapshotTableSummary `json:"summary"`
}
type recoverySnapshotState struct {
	Kind          string                         `json:"kind"`
	Sequences     []RecoverySnapshotSequence     `json:"sequences"`
	KeyReferences []RecoverySnapshotKeyReference `json:"key_references"`
	Organizations []RecoverySnapshotOrganization `json:"organizations"`
}
type recoverySnapshotComplete struct {
	Kind               string `json:"kind"`
	Tables             int    `json:"tables"`
	Rows               int64  `json:"rows,string"`
	Bytes              int64  `json:"bytes,string"`
	PrecedingWireBytes int64  `json:"preceding_wire_bytes,string"`
	PrecedingSHA256    string `json:"preceding_sha256"`
}

func recoverySchemaSHA256() string {
	return EmbeddedSchemaSHA256()
}
func recoverySnapshotHash(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

func recoverySnapshotError(ctx context.Context, fallback error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fallback
}

// ExportRecoverySnapshot produces a private, data-only intermediate stream.
// It never applies migrations, creates a target, sets sequence values, reads
// process secrets, or enables recovery. Partial output is invalid and must not
// be published/sealed as a successful snapshot. The caller must protect and
// discard partial staging data. Use the separately authenticated encryption
// layer before storing or transporting a completed snapshot.
func (s *Store) ExportRecoverySnapshot(ctx context.Context, out io.Writer, limits RecoverySnapshotLimits) (RecoverySnapshotReport, error) {
	var report RecoverySnapshotReport
	if !limits.valid() || out == nil {
		return report, ErrRecoverySnapshotLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	defer tx.Rollback(context.Background())

	header, schema, err := recoverySnapshotInspect(ctx, tx, s.runtimeSchema)
	if err != nil {
		return report, err
	}
	if err = recoverySnapshotValidateHeader(header); err != nil {
		return report, err
	}

	// Check every row's binary size before any payload is sent to the client.
	// record_send includes 4 extra bytes per field plus 2 compared with COPY.
	counts, err := recoverySnapshotPreflight(ctx, tx, schema, header.Inventory, limits)
	if err != nil {
		return report, err
	}
	w := &recoverySnapshotWriter{ctx: ctx, out: out, hash: sha256.New(), limit: limits.MaxWireBytes}
	if err = w.record(header); err != nil {
		return report, err
	}
	report = RecoverySnapshotReport{Metadata: header.Metadata, Format: header.Format, InventorySHA256: header.InventorySHA256, Tables: []RecoverySnapshotTableSummary{}}
	for _, table := range header.Inventory.Tables {
		if !table.Exported {
			continue
		}
		if err = w.record(recoverySnapshotTableStart{Kind: "table", Name: table.Name}); err != nil {
			return RecoverySnapshotReport{}, err
		}
		cw := newRecoverySnapshotCopyWriter(w, table, limits, limits.MaxTotalBytes-report.Bytes, limits.MaxRows-report.Rows)
		tag, copyErr := tx.Conn().PgConn().CopyTo(ctx, cw, recoverySnapshotCopySQL(schema, table))
		if copyErr != nil {
			return RecoverySnapshotReport{}, recoverySnapshotError(ctx, cw.failureOr(ErrRecoverySnapshotSource))
		}
		if err = cw.finish(); err != nil {
			return RecoverySnapshotReport{}, err
		}
		if tag.RowsAffected() != cw.parser.rows || cw.parser.rows != counts[table.Name] {
			return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
		}
		summary := RecoverySnapshotTableSummary{Name: table.Name, Rows: cw.parser.rows, Bytes: cw.bytes, SHA256: recoverySnapshotHash(cw.hash)}
		if err = w.record(recoverySnapshotTableEnd{Kind: "table_end", Summary: summary}); err != nil {
			return RecoverySnapshotReport{}, err
		}
		report.Tables = append(report.Tables, summary)
		report.Rows += summary.Rows
		report.Bytes += summary.Bytes
	}
	state, err := recoverySnapshotReadState(ctx, tx, schema, limits)
	if err != nil {
		return RecoverySnapshotReport{}, err
	}
	if err = recoverySnapshotValidateState(state, header.Inventory, limits); err != nil {
		return RecoverySnapshotReport{}, err
	}
	if err = w.record(state); err != nil {
		return RecoverySnapshotReport{}, err
	}

	// A lost read-only COMMIT still withholds the final marker. No data mutation
	// occurred, but the caller must not call a partial stream successful.
	if err = tx.Commit(ctx); err != nil {
		return RecoverySnapshotReport{}, recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	done := recoverySnapshotComplete{Kind: "complete", Tables: len(report.Tables), Rows: report.Rows, Bytes: report.Bytes, PrecedingWireBytes: w.bytes, PrecedingSHA256: recoverySnapshotHash(w.hash)}
	if err = w.record(done); err != nil {
		return RecoverySnapshotReport{}, err
	}
	report.Sequences, report.KeyReferences, report.Organizations = state.Sequences, state.KeyReferences, state.Organizations
	report.WireBytes, report.SHA256 = w.bytes, recoverySnapshotHash(w.hash)
	return report, nil
}

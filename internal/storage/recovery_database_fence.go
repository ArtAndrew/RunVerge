// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryDatabaseFence = errors.New("database fence not confirmed; inspect the saved intent before any further operation")
var ErrRecoveryDatabaseFenceUnknown = errors.New("database fence commit outcome unknown; no sessions terminated by this attempt; inspect the saved intent")

// This host-only profile is for separate databases on one trusted primary.
// It does NOT fence in-memory dispatch permits, processes, external credentials,
// replicas/clones or a hostile database administrator. It never enables work.
type RecoveryDatabaseFenceIntent struct {
	Version              int       `json:"version"`
	OperationID          string    `json:"operation_id"`
	SystemID             string    `json:"system_id"`
	WALTimeline          string    `json:"wal_timeline,omitempty"`
	ServerStartedAt      time.Time `json:"server_started_at"`
	MaintenanceOID       uint32    `json:"maintenance_oid"`
	SourceOID            uint32    `json:"source_oid"`
	SourceDatabase       string    `json:"source_database"`
	TargetOID            uint32    `json:"target_oid"`
	TargetDatabase       string    `json:"target_database"`
	SourceInstallationID string    `json:"source_installation_id"`
	TargetInstallationID string    `json:"target_installation_id"`
	RestoreID            string    `json:"restore_id"`
	BackupID             string    `json:"backup_id,omitempty"`
	CutUTC               string    `json:"cut_utc,omitempty"`
	SchemaSHA256         string    `json:"schema_sha256,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

type RecoveryDatabaseFenceObservation struct {
	OperationID             string    `json:"operation_id"`
	State                   string    `json:"state"`
	SourceDatabase          string    `json:"source_database"`
	ConnectionsAllowed      bool      `json:"connections_allowed"`
	Sessions                int       `json:"sessions"`
	PreparedTransactions    int       `json:"prepared_transactions"`
	ObservedAt              time.Time `json:"observed_at"`
	ReceiptSHA256           string    `json:"receipt_sha256,omitempty"`
	PostmasterChanged       bool      `json:"postmaster_changed,omitempty"`
	ExternalExecutionFenced bool      `json:"external_execution_fenced"`
}

type recoveryFenceServer struct {
	SystemID    string
	WALTimeline string
	Started     time.Time
	DatabaseOID uint32
}

// Callers open a fresh, explicitly configured maintenance connection. Superuser
// is required in this first host profile to make session visibility complete.
func recoveryFenceServerIdentity(ctx context.Context, c *pgx.Conn) (recoveryFenceServer, error) {
	var id recoveryFenceServer
	var primary, super, version bool
	err := c.QueryRow(ctx, `SELECT s.system_identifier::pg_catalog.text,
		CASE WHEN pg_catalog.pg_is_in_recovery() THEN '' ELSE
			pg_catalog.left(pg_catalog.pg_walfile_name(pg_catalog.pg_current_wal_insert_lsn()),8) END,
		pg_catalog.pg_postmaster_start_time(), d.oid,
		NOT pg_catalog.pg_is_in_recovery(), r.rolsuper,
		pg_catalog.current_setting('server_version_num')::pg_catalog.int4 / 10000 = 18
		FROM pg_catalog.pg_control_system() s, pg_catalog.pg_database d, pg_catalog.pg_roles r
		WHERE d.datname=pg_catalog.current_database() AND r.rolname=CURRENT_USER`).Scan(&id.SystemID, &id.WALTimeline, &id.Started, &id.DatabaseOID, &primary, &super, &version)
	if err != nil || !primary || !super || !version || c.PgConn().TxStatus() != 'I' {
		return recoveryFenceServer{}, ErrRecoveryDatabaseFence
	}
	return id, nil
}

// CaptureRecoveryDatabaseFence observes live source/target backends from the
// maintenance connection. A fresh nonce avoids treating equal replica/clone
// control-file IDs or proxy endpoints alone as proof of a shared primary.
// The caller must persist this intent privately BEFORE calling Apply.
func CaptureRecoveryDatabaseFence(ctx context.Context, admin *pgx.Conn, source, target *Store, spec RecoveryTargetSpec) (RecoveryDatabaseFenceIntent, error) {
	var empty RecoveryDatabaseFenceIntent
	if admin == nil || source == nil || target == nil || !validRecoveryID(spec.SourceID, "inst") || !validRecoveryID(spec.TargetInstallationID, "inst") || !validRecoveryID(spec.RestoreID, "restore") {
		return empty, ErrRecoveryDatabaseFence
	}
	server, err := recoveryFenceServerIdentity(ctx, admin)
	if err != nil {
		return empty, err
	}
	so, sn, ss, err := witnessRecoveryFenceDatabase(ctx, admin, source)
	if err != nil {
		return empty, err
	}
	to, tn, ts, err := witnessRecoveryFenceDatabase(ctx, admin, target)
	if err != nil {
		return empty, err
	}
	if so == to || so == server.DatabaseOID || to == server.DatabaseOID || ss.State != "normal" || ss.InstallationID != spec.SourceID || ts.State != "held" || ts.InstallationID != spec.TargetInstallationID || ts.RestoreID != spec.RestoreID || ts.SourceID != spec.SourceID || ts.BackupID != spec.BackupID || ts.CutUTC != spec.CutUTC || ts.SchemaSHA256 != spec.SchemaSHA256 {
		return empty, ErrRecoveryDatabaseFence
	}
	intent := RecoveryDatabaseFenceIntent{
		Version: 2, OperationID: domain.NewID("fence"), SystemID: server.SystemID, WALTimeline: server.WALTimeline,
		ServerStartedAt: server.Started, MaintenanceOID: server.DatabaseOID,
		SourceOID: so, SourceDatabase: sn, SourceInstallationID: ss.InstallationID,
		TargetOID: to, TargetDatabase: tn, TargetInstallationID: ts.InstallationID,
		RestoreID: ts.RestoreID, BackupID: ts.BackupID, CutUTC: ts.CutUTC, SchemaSHA256: ts.SchemaSHA256,
		CreatedAt: time.Now().UTC(),
	}
	ob, err := InspectRecoveryDatabaseFence(ctx, admin, intent)
	if err != nil || !ob.ConnectionsAllowed || ob.PreparedTransactions != 0 {
		return empty, ErrRecoveryDatabaseFence
	}
	return intent, nil
}

func witnessRecoveryFenceDatabase(ctx context.Context, admin *pgx.Conn, s *Store) (uint32, string, RecoveryTargetState, error) {
	c, e := s.Pool.Acquire(ctx)
	if e != nil {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	defer c.Release()
	tx, e := c.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if e != nil {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	defer recoveryFenceRollback(tx, c.Conn())
	if _, e = tx.Exec(ctx, "SET LOCAL search_path = pg_catalog, pg_temp"); e != nil {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	if _, e = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); e != nil {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	state, e := readRecoveryState(ctx, s.runtimeSchema, tx)
	if e != nil {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	nonce := domain.NewID("fence")
	var ignored string
	if e = tx.QueryRow(ctx, "SELECT pg_catalog.set_config('application_name',$1,true)", nonce).Scan(&ignored); e != nil {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	var oid uint32
	var name string
	var pid int32
	var started time.Time
	var primary bool
	e = tx.QueryRow(ctx, `SELECT d.oid,d.datname,a.pid,a.backend_start,NOT pg_catalog.pg_is_in_recovery()
			FROM pg_catalog.pg_database d, pg_catalog.pg_stat_activity a
			WHERE d.datname=pg_catalog.current_database() AND a.pid=pg_catalog.pg_backend_pid()`).Scan(&oid, &name, &pid, &started, &primary)
	if e != nil || !primary {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	var witnessed bool
	// The admin connection must not retain a cached statistics snapshot.
	if _, e = admin.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()"); e != nil {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	e = admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity
			WHERE pid=$1 AND backend_start=$2 AND datid=$3 AND application_name=$4)`, pid, started, oid, nonce).Scan(&witnessed)
	if e != nil || !witnessed {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	if e = tx.Commit(ctx); e != nil {
		return 0, "", RecoveryTargetState{}, ErrRecoveryDatabaseFence
	}
	return oid, name, state, nil
}

func validRecoveryDatabaseFenceIntent(p RecoveryDatabaseFenceIntent) bool {
	if len(p.OperationID) != 38 || !strings.HasPrefix(p.OperationID, "fence_") || p.SystemID == "" || p.ServerStartedAt.IsZero() || p.CreatedAt.IsZero() {
		return false
	}
	if p.SourceOID == 0 || p.TargetOID == 0 || p.MaintenanceOID == 0 || p.SourceOID == p.TargetOID || p.SourceOID == p.MaintenanceOID || p.TargetOID == p.MaintenanceOID || p.SourceDatabase == p.TargetDatabase {
		return false
	}
	for _, name := range []string{p.SourceDatabase, p.TargetDatabase} {
		if name == "" || len(name) > 63 || strings.ContainsRune(name, '\x00') {
			return false
		}
	}
	if !validRecoveryID(p.SourceInstallationID, "inst") || !validRecoveryID(p.TargetInstallationID, "inst") || p.SourceInstallationID == p.TargetInstallationID || !validRecoveryID(p.RestoreID, "restore") {
		return false
	}
	switch p.Version {
	case 1:
		return p.WALTimeline == "" && p.BackupID == "" && p.CutUTC == "" && p.SchemaSHA256 == ""
	case 2:
		timeline, timelineErr := strconv.ParseUint(p.WALTimeline, 16, 32)
		cut, cutErr := time.Parse(time.RFC3339Nano, p.CutUTC)
		return len(p.WALTimeline) == 8 && timelineErr == nil && timeline != 0 && cutErr == nil && cut.UTC().Format(time.RFC3339Nano) == p.CutUTC && validRecoveryID(p.BackupID, "bak") && validDigest(p.SchemaSHA256)
	default:
		return false
	}
}

func checkRecoveryFenceServer(ctx context.Context, admin *pgx.Conn, p RecoveryDatabaseFenceIntent) (recoveryFenceServer, error) {
	if admin == nil || !validRecoveryDatabaseFenceIntent(p) {
		return recoveryFenceServer{}, ErrRecoveryDatabaseFence
	}
	id, err := recoveryFenceServerIdentity(ctx, admin)
	if err != nil || id.SystemID != p.SystemID || id.DatabaseOID != p.MaintenanceOID || (p.Version == 2 && id.WALTimeline != p.WALTimeline) {
		return recoveryFenceServer{}, ErrRecoveryDatabaseFence
	}
	return id, nil
}

const recoveryFenceSetting = "runverge.recovery_fence_v2"

// Read the database-wide catalog default, never a session setting or a role
// default. An array also lets us reject ambiguous duplicate reserved entries.
const recoveryFenceSettings = `ARRAY(SELECT name FROM pg_catalog.unnest(r.setconfig) AS config(name)
	WHERE pg_catalog.starts_with(name, '` + recoveryFenceSetting + `='))`

type recoveryFenceCatalog struct {
	RecoveryDatabaseFenceObservation
	settings                               []string
	sourceXMIN, settingsXMIN, snapshotXMAX string
}

func readRecoveryFenceCatalog(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, p RecoveryDatabaseFenceIntent) (recoveryFenceCatalog, error) {
	o := recoveryFenceCatalog{RecoveryDatabaseFenceObservation: RecoveryDatabaseFenceObservation{OperationID: p.OperationID, SourceDatabase: p.SourceDatabase}}
	var sourceTemplate, targetTemplate bool
	err := q.QueryRow(ctx, `SELECT s.datallowconn,s.datistemplate,t.datistemplate,
		(SELECT pg_catalog.count(*) FROM pg_catalog.pg_stat_activity a WHERE a.datid=s.oid),
		(SELECT pg_catalog.count(*) FROM pg_catalog.pg_prepared_xacts x WHERE x.database=s.datname),pg_catalog.clock_timestamp(),
		`+recoveryFenceSettings+`,s.xmin::pg_catalog.text,COALESCE(r.xmin::pg_catalog.text,''),
		pg_catalog.pg_snapshot_xmax(pg_catalog.pg_current_snapshot())::pg_catalog.text
		FROM pg_catalog.pg_database s CROSS JOIN pg_catalog.pg_database t
		LEFT JOIN pg_catalog.pg_db_role_setting r ON r.setdatabase=$1 AND r.setrole=0
		WHERE s.oid=$1 AND s.datname=$2 AND s.datconnlimit<>-2 AND t.oid=$3 AND t.datname=$4 AND t.datconnlimit<>-2`,
		p.SourceOID, p.SourceDatabase, p.TargetOID, p.TargetDatabase).Scan(&o.ConnectionsAllowed, &sourceTemplate, &targetTemplate,
		&o.Sessions, &o.PreparedTransactions, &o.ObservedAt, &o.settings, &o.sourceXMIN, &o.settingsXMIN, &o.snapshotXMAX)
	if err != nil || sourceTemplate || targetTemplate {
		return recoveryFenceCatalog{}, ErrRecoveryDatabaseFence
	}
	o.State = "connections_allowed"
	if !o.ConnectionsAllowed {
		o.State = "connections_disabled_unresolved"
		if o.Sessions == 0 && o.PreparedTransactions == 0 {
			o.State = "database_admission_fenced"
		}
	}
	return o, nil
}

func recoveryFenceReceiptPrefix(p RecoveryDatabaseFenceIntent) (string, error) {
	p.ServerStartedAt, p.CreatedAt = p.ServerStartedAt.UTC(), p.CreatedAt.UTC()
	raw, err := json.Marshal(p)
	if err != nil {
		return "", ErrRecoveryDatabaseFence
	}
	return Hash(append([]byte("runverge/database-fence/v2\x00"), raw...)) + ":", nil
}

func (o recoveryFenceCatalog) matchesFenceTransaction(xid uint64) bool {
	xmin := strconv.FormatUint(uint64(uint32(xid)), 10)
	return o.sourceXMIN == xmin && o.settingsXMIN == xmin
}

func (o recoveryFenceCatalog) matchesCommittedFence(p RecoveryDatabaseFenceIntent) bool {
	prefix, err := recoveryFenceReceiptPrefix(p)
	_, matches := o.committedFenceXID(prefix)
	return err == nil && matches
}

func (o recoveryFenceCatalog) committedFenceXID(prefix string) (uint64, bool) {
	if prefix == "" || len(o.settings) != 1 || o.ConnectionsAllowed {
		return 0, false
	}
	tail, hasPrefix := strings.CutPrefix(o.settings[0], recoveryFenceSetting+"="+prefix)
	xid, xidErr := strconv.ParseUint(tail, 10, 64)
	next, nextErr := strconv.ParseUint(o.snapshotXMAX, 10, 64)
	// Catalog xmin exposes only the low 32 bits. Keep the original full
	// transaction within one unambiguous epoch of the current snapshot.
	return xid, hasPrefix && xidErr == nil && nextErr == nil && tail == strconv.FormatUint(xid, 10) && xid >= 3 && next > xid && next-xid < 1<<31 && o.matchesFenceTransaction(xid)
}

// Inspect is a fresh observation. A V2 receipt can survive a same-primary
// restart but is never evidence about an executor or a permission to execute.
func InspectRecoveryDatabaseFence(ctx context.Context, admin *pgx.Conn, p RecoveryDatabaseFenceIntent) (RecoveryDatabaseFenceObservation, error) {
	o, err := inspectRecoveryDatabaseFence(ctx, admin, p)
	return o.RecoveryDatabaseFenceObservation, err
}

func inspectRecoveryDatabaseFence(ctx context.Context, admin *pgx.Conn, p RecoveryDatabaseFenceIntent) (recoveryFenceCatalog, error) {
	empty := recoveryFenceCatalog{}
	id, err := checkRecoveryFenceServer(ctx, admin, p)
	if err != nil {
		return empty, err
	}
	if _, err := admin.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()"); err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	o, err := readRecoveryFenceCatalog(ctx, admin, p)
	if err != nil {
		return empty, err
	}
	o.PostmasterChanged = !id.Started.Equal(p.ServerStartedAt)
	if p.Version == 2 && o.matchesCommittedFence(p) {
		o.ReceiptSHA256 = Hash([]byte(o.settings[0]))
		return o, nil
	}
	if o.PostmasterChanged || len(o.settings) != 0 || (p.Version == 2 && !o.ConnectionsAllowed) {
		return empty, ErrRecoveryDatabaseFence
	}
	return o, nil
}

// Apply disables new source connections in a transaction, then drains existing
// sessions only after a confirmed commit. Persist the captured intent first.
// An already-disabled source is not silently adopted; use Inspect then an
// explicitly authorized Drain if needed. There is no automatic reopen operation.
func ApplyRecoveryDatabaseFence(ctx context.Context, admin *pgx.Conn, p RecoveryDatabaseFenceIntent) (RecoveryDatabaseFenceObservation, error) {
	var empty RecoveryDatabaseFenceObservation
	if p.CreatedAt.After(time.Now().Add(time.Minute)) || time.Since(p.CreatedAt) > 15*time.Minute {
		return empty, ErrRecoveryDatabaseFence
	}
	id, err := checkRecoveryFenceServer(ctx, admin, p)
	if err != nil || !id.Started.Equal(p.ServerStartedAt) {
		return empty, ErrRecoveryDatabaseFence
	}
	tx, err := admin.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	defer recoveryFenceRollback(tx, admin)
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='15s'; SET LOCAL transaction_timeout='25s'; SET LOCAL synchronous_commit=on"); err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	// Serialize the check and change across maintenance databases. This brief
	// shared-catalog lock excludes concurrent database DDL but allows readers.
	if _, err = tx.Exec(ctx, "LOCK TABLE pg_catalog.pg_database IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	before, err := readRecoveryFenceCatalog(ctx, tx, p)
	if err != nil || !before.ConnectionsAllowed || before.PreparedTransactions != 0 || len(before.settings) != 0 {
		return empty, ErrRecoveryDatabaseFence
	}
	// Names are from the witnessed live catalog, never archive-controlled SQL.
	alter := "ALTER DATABASE " + pgx.Identifier{p.SourceDatabase}.Sanitize()
	if _, err = tx.Exec(ctx, alter+" ALLOW_CONNECTIONS false"); err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	var receipt string
	var fenceXID uint64
	if p.Version == 2 {
		prefix, prefixErr := recoveryFenceReceiptPrefix(p)
		var xid string
		if prefixErr != nil || tx.QueryRow(ctx, "SELECT pg_catalog.pg_current_xact_id()::pg_catalog.text").Scan(&xid) != nil {
			return empty, ErrRecoveryDatabaseFence
		}
		fenceXID, err = strconv.ParseUint(xid, 10, 64)
		if err != nil || fenceXID < 3 {
			return empty, ErrRecoveryDatabaseFence
		}
		// Only a generated hex digest and a decimal transaction ID reach the
		// literal. Existing unrelated database defaults are preserved.
		receipt = prefix + strconv.FormatUint(fenceXID, 10)
		if _, err = tx.Exec(ctx, alter+" SET "+recoveryFenceSetting+" = '"+receipt+"'"); err != nil {
			return empty, ErrRecoveryDatabaseFence
		}
	}
	after, err := readRecoveryFenceCatalog(ctx, tx, p)
	if err != nil || after.ConnectionsAllowed || after.PreparedTransactions != 0 {
		return empty, ErrRecoveryDatabaseFence
	}
	if p.Version == 2 && (len(after.settings) != 1 || after.settings[0] != recoveryFenceSetting+"="+receipt || !after.matchesFenceTransaction(fenceXID)) {
		return empty, ErrRecoveryDatabaseFence
	}
	if err = tx.Commit(ctx); err != nil {
		recoveryFenceClose(admin)
		return empty, ErrRecoveryDatabaseFenceUnknown
	}
	return DrainRecoveryDatabaseFence(ctx, admin, p)
}

// Drain is a separately authorized continuation after inspection of an already
// disabled database. It never reuses PIDs from the persisted intent, settles a
// prepared transaction or infers the outcome of an external request.
func DrainRecoveryDatabaseFence(ctx context.Context, admin *pgx.Conn, p RecoveryDatabaseFenceIntent) (RecoveryDatabaseFenceObservation, error) {
	var empty RecoveryDatabaseFenceObservation
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	before, err := inspectRecoveryDatabaseFence(ctx, admin, p)
	if err != nil || before.ConnectionsAllowed {
		return empty, ErrRecoveryDatabaseFence
	}
	rows, err := admin.Query(ctx, `SELECT pid,backend_start FROM pg_catalog.pg_stat_activity WHERE datid=$1 ORDER BY pid LIMIT 257`, p.SourceOID)
	if err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	type session struct {
		pid     int32
		started time.Time
	}
	sessions := []session{}
	for rows.Next() {
		var s session
		if err = rows.Scan(&s.pid, &s.started); err != nil {
			rows.Close()
			return empty, ErrRecoveryDatabaseFence
		}
		sessions = append(sessions, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(sessions) > 256 {
		return empty, ErrRecoveryDatabaseFence
	}
	if before.settings == nil {
		before.settings = []string{}
	}
	receiptXMIN := ""
	if before.ReceiptSHA256 != "" {
		receiptXMIN = before.settingsXMIN
	}
	for _, s := range sessions {
		// Check the exact receipt, database and backend birth in the same
		// statement as the signal. This connection never reconnects implicitly.
		var terminated bool
		err = admin.QueryRow(ctx, `SELECT pg_catalog.pg_terminate_backend(a.pid,1000::pg_catalog.int8)
			FROM pg_catalog.pg_database d
			JOIN pg_catalog.pg_stat_activity a ON a.datid=d.oid
			LEFT JOIN pg_catalog.pg_db_role_setting r ON r.setdatabase=d.oid AND r.setrole=0
			WHERE d.oid=$1 AND d.datname=$2 AND NOT d.datallowconn AND NOT d.datistemplate
			AND a.pid=$3 AND a.backend_start=$4 AND `+recoveryFenceSettings+`=$5::pg_catalog.text[]
			AND ($6='' OR (d.xmin::pg_catalog.text=$6 AND r.xmin::pg_catalog.text=$6))`,
			p.SourceOID, p.SourceDatabase, s.pid, s.started, before.settings, receiptXMIN).Scan(&terminated)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) || err == nil && !terminated {
			return empty, ErrRecoveryDatabaseFence
		}
	}
	after, err := InspectRecoveryDatabaseFence(ctx, admin, p)
	if err != nil || after.State != "database_admission_fenced" {
		return empty, ErrRecoveryDatabaseFence
	}
	return after, nil
}

// Failed cleanup must never put an uncertain transaction back into a pool.
func recoveryFenceClose(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}
func recoveryFenceRollback(tx pgx.Tx, c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		recoveryFenceClose(c)
	}
}

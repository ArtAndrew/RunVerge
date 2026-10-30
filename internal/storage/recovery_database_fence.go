// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
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
	ServerStartedAt      time.Time `json:"server_started_at"`
	MaintenanceOID       uint32    `json:"maintenance_oid"`
	SourceOID            uint32    `json:"source_oid"`
	SourceDatabase       string    `json:"source_database"`
	TargetOID            uint32    `json:"target_oid"`
	TargetDatabase       string    `json:"target_database"`
	SourceInstallationID string    `json:"source_installation_id"`
	TargetInstallationID string    `json:"target_installation_id"`
	RestoreID            string    `json:"restore_id"`
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
	ExternalExecutionFenced bool      `json:"external_execution_fenced"`
}

type recoveryFenceServer struct {
	SystemID    string
	Started     time.Time
	DatabaseOID uint32
}

// Callers open a fresh, explicitly configured maintenance connection. Superuser
// is required in this first host profile to make session visibility complete.
func recoveryFenceServerIdentity(ctx context.Context, c *pgx.Conn) (recoveryFenceServer, error) {
	var id recoveryFenceServer
	var primary, super, version bool
	err := c.QueryRow(ctx, `SELECT s.system_identifier::pg_catalog.text,
		pg_catalog.pg_postmaster_start_time(), d.oid,
		NOT pg_catalog.pg_is_in_recovery(), r.rolsuper,
		pg_catalog.current_setting('server_version_num')::pg_catalog.int4 / 10000 = 18
		FROM pg_catalog.pg_control_system() s, pg_catalog.pg_database d, pg_catalog.pg_roles r
		WHERE d.datname=pg_catalog.current_database() AND r.rolname=CURRENT_USER`).Scan(&id.SystemID, &id.Started, &id.DatabaseOID, &primary, &super, &version)
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
	observe := func(s *Store) (uint32, string, RecoveryTargetState, error) {
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
	so, sn, ss, err := observe(source)
	if err != nil {
		return empty, err
	}
	to, tn, ts, err := observe(target)
	if err != nil {
		return empty, err
	}
	if so == to || so == server.DatabaseOID || to == server.DatabaseOID || ss.State != "normal" || ss.InstallationID != spec.SourceID || ts.State != "held" || ts.InstallationID != spec.TargetInstallationID || ts.RestoreID != spec.RestoreID || ts.SourceID != spec.SourceID || ts.BackupID != spec.BackupID || ts.CutUTC != spec.CutUTC || ts.SchemaSHA256 != spec.SchemaSHA256 {
		return empty, ErrRecoveryDatabaseFence
	}
	intent := RecoveryDatabaseFenceIntent{Version: 1, OperationID: domain.NewID("fence"), SystemID: server.SystemID, ServerStartedAt: server.Started, MaintenanceOID: server.DatabaseOID, SourceOID: so, SourceDatabase: sn, TargetOID: to, TargetDatabase: tn, SourceInstallationID: ss.InstallationID, TargetInstallationID: ts.InstallationID, RestoreID: ts.RestoreID, CreatedAt: time.Now().UTC()}
	ob, err := InspectRecoveryDatabaseFence(ctx, admin, intent)
	if err != nil || !ob.ConnectionsAllowed || ob.PreparedTransactions != 0 {
		return empty, ErrRecoveryDatabaseFence
	}
	return intent, nil
}

func validRecoveryDatabaseFenceIntent(p RecoveryDatabaseFenceIntent) bool {
	return p.Version == 1 && len(p.OperationID) == 38 && strings.HasPrefix(p.OperationID, "fence_") && p.SystemID != "" && !p.ServerStartedAt.IsZero() && !p.CreatedAt.IsZero() && p.SourceOID != 0 && p.TargetOID != 0 && p.MaintenanceOID != 0 && p.SourceOID != p.TargetOID && p.SourceOID != p.MaintenanceOID && p.TargetOID != p.MaintenanceOID && p.SourceDatabase != "" && p.TargetDatabase != "" && len(p.SourceDatabase) <= 63 && len(p.TargetDatabase) <= 63 && !strings.ContainsRune(p.SourceDatabase, '\x00') && !strings.ContainsRune(p.TargetDatabase, '\x00') && p.SourceDatabase != p.TargetDatabase && validRecoveryID(p.SourceInstallationID, "inst") && validRecoveryID(p.TargetInstallationID, "inst") && validRecoveryID(p.RestoreID, "restore") && p.SourceInstallationID != p.TargetInstallationID
}

func checkRecoveryFenceServer(ctx context.Context, admin *pgx.Conn, p RecoveryDatabaseFenceIntent) error {
	if admin == nil || !validRecoveryDatabaseFenceIntent(p) {
		return ErrRecoveryDatabaseFence
	}
	id, err := recoveryFenceServerIdentity(ctx, admin)
	if err != nil || id.SystemID != p.SystemID || !id.Started.Equal(p.ServerStartedAt) || id.DatabaseOID != p.MaintenanceOID {
		return ErrRecoveryDatabaseFence
	}
	return nil
}

// Inspect is a current observation, never a cached completion certificate or
// permission to execute. A restart/failover identity mismatch fails closed.
func InspectRecoveryDatabaseFence(ctx context.Context, admin *pgx.Conn, p RecoveryDatabaseFenceIntent) (RecoveryDatabaseFenceObservation, error) {
	var empty RecoveryDatabaseFenceObservation
	if err := checkRecoveryFenceServer(ctx, admin, p); err != nil {
		return empty, err
	}
	if _, err := admin.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()"); err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	o := RecoveryDatabaseFenceObservation{OperationID: p.OperationID, SourceDatabase: p.SourceDatabase}
	var sourceTemplate, targetTemplate bool
	err := admin.QueryRow(ctx, `SELECT s.datallowconn,s.datistemplate,t.datistemplate,
		(SELECT pg_catalog.count(*) FROM pg_catalog.pg_stat_activity a WHERE a.datid=s.oid),
		(SELECT pg_catalog.count(*) FROM pg_catalog.pg_prepared_xacts x WHERE x.database=s.datname),pg_catalog.clock_timestamp()
		FROM pg_catalog.pg_database s,pg_catalog.pg_database t
		WHERE s.oid=$1 AND s.datname=$2 AND t.oid=$3 AND t.datname=$4`, p.SourceOID, p.SourceDatabase, p.TargetOID, p.TargetDatabase).Scan(&o.ConnectionsAllowed, &sourceTemplate, &targetTemplate, &o.Sessions, &o.PreparedTransactions, &o.ObservedAt)
	if err != nil || sourceTemplate || targetTemplate {
		return empty, ErrRecoveryDatabaseFence
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

// Apply disables new source connections in a transaction, then drains existing
// sessions only after a confirmed commit. Persist the captured intent first.
// An already-disabled source is not silently adopted; use Inspect then an
// explicitly authorized Drain if needed. There is no automatic reopen operation.
func ApplyRecoveryDatabaseFence(ctx context.Context, admin *pgx.Conn, p RecoveryDatabaseFenceIntent) (RecoveryDatabaseFenceObservation, error) {
	var empty RecoveryDatabaseFenceObservation
	if p.CreatedAt.After(time.Now().Add(time.Minute)) || time.Since(p.CreatedAt) > 15*time.Minute {
		return empty, ErrRecoveryDatabaseFence
	}
	before, err := InspectRecoveryDatabaseFence(ctx, admin, p)
	if err != nil || !before.ConnectionsAllowed || before.PreparedTransactions != 0 {
		return empty, ErrRecoveryDatabaseFence
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	defer recoveryFenceRollback(tx, admin)
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='15s'"); err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	// Names are from the witnessed live catalog, never archive-controlled SQL.
	if _, err = tx.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{p.SourceDatabase}.Sanitize()+" ALLOW_CONNECTIONS false"); err != nil {
		return empty, ErrRecoveryDatabaseFence
	}
	// Recheck AFTER ALTER acquired its object lock, before committing. A
	// rename/recreate race must roll back, not disable a different database.
	var matches bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_database s,pg_catalog.pg_database t
		WHERE s.oid=$1 AND s.datname=$2 AND NOT s.datallowconn AND NOT s.datistemplate
		AND t.oid=$3 AND t.datname=$4 AND NOT t.datistemplate)`, p.SourceOID, p.SourceDatabase, p.TargetOID, p.TargetDatabase).Scan(&matches)
	if err != nil || !matches {
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
	before, err := InspectRecoveryDatabaseFence(ctx, admin, p)
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
	for _, s := range sessions {
		if err = checkRecoveryFenceServer(ctx, admin, p); err != nil {
			return empty, err
		}
		// Recheck identity, admission and backend birth immediately before each
		// signal. The SELECT returns no row if the original session has exited.
		var terminated bool
		err = admin.QueryRow(ctx, `SELECT pg_catalog.pg_terminate_backend(a.pid,1000::pg_catalog.int8)
			FROM pg_catalog.pg_stat_activity a,pg_catalog.pg_database d
			WHERE d.oid=$1 AND d.datname=$2 AND NOT d.datallowconn
			AND a.datid=d.oid AND a.pid=$3 AND a.backend_start=$4`, p.SourceOID, p.SourceDatabase, s.pid, s.started).Scan(&terminated)
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

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
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrRecoveryProvision = errors.New("recovery target creation not confirmed; inspect the saved plan; do not overwrite or repeat")

type RecoveryProvisionIntent struct {
	Version         int       `json:"version"`
	OperationID     string    `json:"operation_id"`
	SystemID        string    `json:"system_id"`
	ServerStartedAt time.Time `json:"server_started_at"`
	MaintenanceOID  uint32    `json:"maintenance_oid"`
	AdminOID        uint32    `json:"admin_oid"`
	Database        string    `json:"database"`
	Owner           string    `json:"owner"`
	BackupID        string    `json:"backup_id"`
	SourceID        string    `json:"source_id"`
	SnapshotSHA256  string    `json:"snapshot_sha256"`
	SchemaSHA256    string    `json:"schema_sha256"`
	CreatedAt       time.Time `json:"created_at"`
}

type RecoveryProvisionObservation struct {
	OperationID               string    `json:"operation_id"`
	Database                  string    `json:"database"`
	DatabaseOID               uint32    `json:"database_oid,omitempty"`
	State                     string    `json:"state"`
	OrdinaryConnectionsDenied bool      `json:"ordinary_connections_denied"`
	ObservedAt                time.Time `json:"observed_at"`
}

var recoveryProvisionName = regexp.MustCompile(`^rvg_(target|owner)_[a-f0-9]{32}$`)

func validRecoveryProvision(p RecoveryProvisionIntent) bool {
	return p.Version == 1 && validRecoveryID(p.OperationID, "restore") && p.SystemID != "" && p.MaintenanceOID != 0 && p.AdminOID != 0 && !p.ServerStartedAt.IsZero() && !p.CreatedAt.IsZero() && recoveryProvisionName.MatchString(p.Database) && recoveryProvisionName.MatchString(p.Owner) && p.Database == "rvg_target_"+p.OperationID[8:] && p.Owner == "rvg_owner_"+p.OperationID[8:] && validRecoveryID(p.BackupID, "bak") && validRecoveryID(p.SourceID, "inst") && validDigest(p.SnapshotSHA256) && p.SchemaSHA256 == EmbeddedSchemaSHA256()
}

func recoveryProvisionMarker(p RecoveryProvisionIntent) string {
	body, _ := json.Marshal(p)
	return "runverge-recovery-provision-v1:" + Hash(body)
}

// PlanRecoveryProvision reads an explicitly trusted maintenance connection.
// The caller authenticates/selects the archive and persists this intent before
// creation. SourceID binds the selected snapshot, not live source processes.
func PlanRecoveryProvision(ctx context.Context, admin *pgx.Conn, backup, source, snapshot, schema string) (RecoveryProvisionIntent, error) {
	var p RecoveryProvisionIntent
	if admin == nil {
		return p, ErrRecoveryProvision
	}
	server, err := recoveryFenceServerIdentity(ctx, admin)
	if err != nil {
		return p, ErrRecoveryProvision
	}
	var role uint32
	if err = admin.QueryRow(ctx, "SELECT oid FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolname=current_user").Scan(&role); err != nil {
		return p, ErrRecoveryProvision
	}
	operation := domain.NewID("restore")
	p = RecoveryProvisionIntent{Version: 1, OperationID: operation, SystemID: server.SystemID, ServerStartedAt: server.Started, MaintenanceOID: server.DatabaseOID, AdminOID: role, Database: "rvg_target_" + operation[8:], Owner: "rvg_owner_" + operation[8:], BackupID: backup, SourceID: source, SnapshotSHA256: snapshot, SchemaSHA256: schema, CreatedAt: time.Now().UTC()}
	if !validRecoveryProvision(p) {
		return RecoveryProvisionIntent{}, ErrRecoveryProvision
	}
	observed, err := InspectRecoveryProvision(ctx, admin, p)
	if err != nil || observed.State != "not_created" {
		return RecoveryProvisionIntent{}, ErrRecoveryProvision
	}
	return p, nil
}

// Inspection never creates, repairs, adopts, migrates or drops a database.
// Matching names alone are insufficient: the unique owner and database markers
// must bind the exact plan. The DB administrator and endpoint remain trusted.
func InspectRecoveryProvision(ctx context.Context, admin *pgx.Conn, p RecoveryProvisionIntent) (RecoveryProvisionObservation, error) {
	ob := RecoveryProvisionObservation{OperationID: p.OperationID, Database: p.Database, ObservedAt: time.Now().UTC()}
	if admin == nil || !validRecoveryProvision(p) {
		return ob, ErrRecoveryProvision
	}
	server, err := recoveryFenceServerIdentity(ctx, admin)
	if err != nil || server.SystemID != p.SystemID || server.DatabaseOID != p.MaintenanceOID {
		return ob, ErrRecoveryProvision
	}
	var adminID uint32
	if err = admin.QueryRow(ctx, "SELECT oid FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolname=current_user").Scan(&adminID); err != nil || adminID != p.AdminOID {
		return ob, ErrRecoveryProvision
	}
	var owner uint32
	var safe bool
	err = admin.QueryRow(ctx, `SELECT r.oid, NOT r.rolcanlogin AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolinherit AND NOT r.rolreplication AND NOT r.rolbypassrls AND r.rolconnlimit=0 AND r.rolpassword IS NULL AND r.rolvaliduntil IS NULL AND pg_catalog.shobj_description(r.oid,'pg_authid')=$2 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m WHERE m.roleid=r.oid OR m.member=r.oid) FROM pg_catalog.pg_authid r WHERE r.rolname=$1`, p.Owner, recoveryProvisionMarker(p)).Scan(&owner, &safe)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE datname=$1)", p.Database).Scan(&exists) != nil || exists {
			return ob, ErrRecoveryProvision
		}
		ob.State = "not_created"
		return ob, nil
	}
	if err != nil || !safe {
		return ob, ErrRecoveryProvision
	}
	var allowed, restricted bool
	var marker string
	err = admin.QueryRow(ctx, `SELECT d.oid,d.datallowconn,COALESCE(pg_catalog.shobj_description(d.oid,'pg_database'),''),
 d.datdba=$2 AND d.datconnlimit=0 AND NOT d.datistemplate AND d.encoding=6 AND d.datlocprovider='c' AND d.datcollate='C' AND d.datctype='C' AND d.dattablespace=(SELECT oid FROM pg_catalog.pg_tablespace WHERE spcname='pg_default') AND
 NOT EXISTS(SELECT 1 FROM pg_catalog.aclexplode(COALESCE(d.datacl,pg_catalog.acldefault('d',d.datdba))) a WHERE a.grantee<>d.datdba)
 FROM pg_catalog.pg_database d WHERE d.datname=$1`, p.Database, owner).Scan(&ob.DatabaseOID, &allowed, &marker, &restricted)
	if errors.Is(err, pgx.ErrNoRows) {
		var other bool
		if admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE datdba=$1)", owner).Scan(&other) != nil || other {
			return ob, ErrRecoveryProvision
		}
		ob.State = "owner_created"
		return ob, nil
	}
	if err != nil {
		return ob, ErrRecoveryProvision
	}
	var exact bool
	if admin.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(datname=$2 AND datconnlimit=0 AND NOT datistemplate AND encoding=6 AND datlocprovider='c' AND datcollate='C' AND datctype='C' AND dattablespace=(SELECT oid FROM pg_catalog.pg_tablespace WHERE spcname='pg_default')) FROM pg_catalog.pg_database WHERE datdba=$1`, owner, p.Database).Scan(&exact) != nil || !exact {
		return ob, ErrRecoveryProvision
	}
	if !allowed && marker == "" {
		ob.State = "database_closed"
		ob.OrdinaryConnectionsDenied = true
		return ob, nil
	}
	if !allowed || !restricted || marker != recoveryProvisionMarker(p) {
		return ob, ErrRecoveryProvision
	}
	ob.State = "database_restricted"
	ob.OrdinaryConnectionsDenied = true
	return ob, nil
}

// CreateRecoveryProvision is deliberately one-shot. The host must first save a
// durable attempt marker; every failure leaves inspectable state, never cleanup.
// CREATE DATABASE cannot be transactional: it starts with connections disabled.
func CreateRecoveryProvision(ctx context.Context, admin *pgx.Conn, p RecoveryProvisionIntent) (RecoveryProvisionObservation, error) {
	var empty RecoveryProvisionObservation
	ob, err := InspectRecoveryProvision(ctx, admin, p)
	if err != nil || ob.State != "not_created" || time.Since(p.CreatedAt) < 0 || time.Since(p.CreatedAt) > 15*time.Minute {
		return empty, ErrRecoveryProvision
	}
	server, err := recoveryFenceServerIdentity(ctx, admin)
	if err != nil || !server.Started.Equal(p.ServerStartedAt) {
		return empty, ErrRecoveryProvision
	}
	owner, db := pgx.Identifier{p.Owner}.Sanitize(), pgx.Identifier{p.Database}.Sanitize()
	tx, err := admin.Begin(ctx)
	if err != nil {
		return empty, ErrRecoveryProvision
	}
	defer recoveryFenceRollback(tx, admin)
	if _, err = tx.Exec(ctx, "CREATE ROLE "+owner+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 0"); err != nil {
		return empty, ErrRecoveryProvision
	}
	// Marker is hex with a fixed prefix, generated exclusively from this plan.
	if _, err = tx.Exec(ctx, "COMMENT ON ROLE "+owner+" IS '"+recoveryProvisionMarker(p)+"'"); err != nil {
		return empty, ErrRecoveryProvision
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, ErrRecoveryProvision
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+db+" OWNER "+owner+" TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LC_COLLATE 'C' LC_CTYPE 'C' ALLOW_CONNECTIONS false CONNECTION LIMIT 0 IS_TEMPLATE false TABLESPACE pg_default"); err != nil {
		return empty, ErrRecoveryProvision
	}
	tx, err = admin.Begin(ctx)
	if err != nil {
		return empty, ErrRecoveryProvision
	}
	defer recoveryFenceRollback(tx, admin)
	if _, err = tx.Exec(ctx, "REVOKE ALL ON DATABASE "+db+" FROM PUBLIC"); err != nil {
		return empty, ErrRecoveryProvision
	}
	if _, err = tx.Exec(ctx, "COMMENT ON DATABASE "+db+" IS '"+recoveryProvisionMarker(p)+"'"); err != nil {
		return empty, ErrRecoveryProvision
	}
	if _, err = tx.Exec(ctx, "ALTER DATABASE "+db+" ALLOW_CONNECTIONS true"); err != nil {
		return empty, ErrRecoveryProvision
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, ErrRecoveryProvision
	}
	return InspectRecoveryProvision(ctx, admin, p)
}

// OpenRecoveryProvisionTarget checks every new backend before it can migrate
// or hold the target. The single-connection pool serializes those witnesses on
// the operator connection, including reconnects. Close it before closing admin.
func OpenRecoveryProvisionTarget(ctx context.Context, admin *pgx.Conn, dsn string, p RecoveryProvisionIntent, expectedOID uint32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, ErrRecoveryProvision
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		return recoveryProvisionWitness(ctx, admin, c, p, expectedOID)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, ErrRecoveryProvision
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, ErrRecoveryProvision
	}
	return &Store{Pool: pool, runtimeSchema: "public"}, nil
}

// Equal DSN names/control-file IDs alone do not establish that a routed
// connection reached the selected primary. Witness a nonce on the actual backend.
func recoveryProvisionWitness(ctx context.Context, admin, target *pgx.Conn, p RecoveryProvisionIntent, expectedOID uint32) error {
	ob, err := InspectRecoveryProvision(ctx, admin, p)
	if err != nil || ob.State != "database_restricted" || ob.DatabaseOID != expectedOID || target == nil {
		return ErrRecoveryProvision
	}
	tx, err := target.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrRecoveryProvision
	}
	defer recoveryFenceRollback(tx, target)
	nonce := domain.NewID("provision")
	var ignored string
	if tx.QueryRow(ctx, "SELECT pg_catalog.set_config('application_name',$1,true)", nonce).Scan(&ignored) != nil {
		return ErrRecoveryProvision
	}
	var pid int32
	var started time.Time
	var oid, role uint32
	if tx.QueryRow(ctx, `SELECT a.pid,a.backend_start,a.datid,a.usesysid FROM pg_catalog.pg_stat_activity a WHERE a.pid=pg_catalog.pg_backend_pid() AND NOT pg_catalog.pg_is_in_recovery() AND session_user=current_user AND current_schema()='public'`).Scan(&pid, &started, &oid, &role) != nil || oid != expectedOID || role != p.AdminOID {
		return ErrRecoveryProvision
	}
	if _, err = admin.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()"); err != nil {
		return ErrRecoveryProvision
	}
	var witnessed bool
	if admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid=$1 AND backend_start=$2 AND datid=$3 AND usesysid=$4 AND application_name=$5)`, pid, started, oid, role, nonce).Scan(&witnessed) != nil || !witnessed {
		return ErrRecoveryProvision
	}
	if tx.Commit(ctx) != nil {
		return ErrRecoveryProvision
	}
	return nil
}

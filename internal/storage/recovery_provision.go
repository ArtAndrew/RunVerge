// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrRecoveryProvision = errors.New("recovery target creation not confirmed; inspect the saved plan; do not overwrite or repeat")

type RecoveryProvisionIntent struct {
	Version          int       `json:"version"`
	OperationID      string    `json:"operation_id"`
	SystemID         string    `json:"system_id"`
	WALTimeline      string    `json:"wal_timeline,omitempty"`
	ServerStartedAt  time.Time `json:"server_started_at"`
	MaintenanceOID   uint32    `json:"maintenance_oid"`
	AdminOID         uint32    `json:"admin_oid"`
	Database         string    `json:"database"`
	Owner            string    `json:"owner"`
	BackupID         string    `json:"backup_id"`
	SourceID         string    `json:"source_id"`
	SnapshotSHA256   string    `json:"snapshot_sha256"`
	SchemaSHA256     string    `json:"schema_sha256"`
	CreatedAt        time.Time `json:"created_at"`
	ProvenanceSHA256 string    `json:"provenance_sha256,omitempty"`
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
	if p.ProvenanceSHA256 != "" && !validDigest(p.ProvenanceSHA256) {
		return false
	}
	if p.WALTimeline != "" && (p.ProvenanceSHA256 == "" || !validRecoveryRetirementServer(p.SystemID, p.WALTimeline)) {
		return false
	}
	return p.Version == 1 && validRecoveryID(p.OperationID, "restore") && p.SystemID != "" && p.MaintenanceOID != 0 && p.AdminOID != 0 && !p.ServerStartedAt.IsZero() && !p.CreatedAt.IsZero() && recoveryProvisionName.MatchString(p.Database) && recoveryProvisionName.MatchString(p.Owner) && p.Database == "rvg_target_"+p.OperationID[8:] && p.Owner == "rvg_owner_"+p.OperationID[8:] && validRecoveryID(p.BackupID, "bak") && validRecoveryID(p.SourceID, "inst") && validDigest(p.SnapshotSHA256) && SupportedRecoverySourceSchema(p.SchemaSHA256)
}

func recoveryProvisionMarker(p RecoveryProvisionIntent) string {
	body, _ := json.Marshal(p)
	if p.ProvenanceSHA256 != "" {
		return "runverge-recovery-provision-v2:" + Hash(body)
	}
	return "runverge-recovery-provision-v1:" + Hash(body)
}

type recoveryProvisionImportBinding struct {
	intent      RecoveryProvisionIntent
	databaseOID uint32
}

// Check the physical marker inside the actual import transaction. Removing
// handoff fields from host JSON cannot downgrade this target to generic import.
func (s *Store) checkRecoveryProvisionImportTx(ctx context.Context, tx pgx.Tx, spec RecoveryTargetSpec, snapshot string) error {
	var marker string
	var oid uint32
	if tx.QueryRow(ctx, `SELECT d.oid,COALESCE(pg_catalog.shobj_description(d.oid,'pg_database'),'') FROM pg_catalog.pg_database d WHERE d.datname=pg_catalog.current_database()`).Scan(&oid, &marker) != nil {
		return ErrRecoveryProvision
	}
	if s.recoveryProvision == nil {
		if strings.HasPrefix(marker, "runverge-recovery-provision-") && !strings.HasPrefix(marker, "runverge-recovery-provision-v1:") {
			return ErrRecoveryProvision
		}
		return nil
	}
	p := s.recoveryProvision.intent
	if oid != s.recoveryProvision.databaseOID || marker != recoveryProvisionMarker(p) || p.ProvenanceSHA256 == "" || p.OperationID != spec.RestoreID || p.BackupID != spec.BackupID || p.SourceID != spec.SourceID || p.SchemaSHA256 != spec.SchemaSHA256 || p.SnapshotSHA256 != snapshot {
		return ErrRecoveryProvision
	}
	return nil
}

// PlanRecoveryProvision reads an explicitly trusted maintenance connection.
// The caller authenticates/selects the archive and persists this intent before
// creation. SourceID binds the selected snapshot, not live source processes.
func PlanRecoveryProvision(ctx context.Context, admin *pgx.Conn, backup, source, snapshot, schema string) (RecoveryProvisionIntent, error) {
	return PlanRecoveryProvisionWithProvenance(ctx, admin, backup, source, snapshot, schema, "")
}

// PlanRecoveryProvisionWithProvenance additionally commits the host-verified
// source provenance into both physical provisioning markers. This is still a
// restricted held target, not clearance or a grant of ordinary credentials.
func PlanRecoveryProvisionWithProvenance(ctx context.Context, admin *pgx.Conn, backup, source, snapshot, schema, provenance string) (RecoveryProvisionIntent, error) {
	return planRecoveryProvision(ctx, admin, backup, source, snapshot, schema, provenance, nil)
}

// The retired source reserves one restore identity on an independently
// selected primary. A different target cannot adopt this held handoff slot.
func PlanRecoveryProvisionAtRetiredDestination(ctx context.Context, admin *pgx.Conn, backup, source, snapshot, schema, provenance string, destination RecoveryRetirementDestination) (RecoveryProvisionIntent, error) {
	return planRecoveryProvision(ctx, admin, backup, source, snapshot, schema, provenance, &destination)
}

func planRecoveryProvision(ctx context.Context, admin *pgx.Conn, backup, source, snapshot, schema, provenance string, destination *RecoveryRetirementDestination) (RecoveryProvisionIntent, error) {
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
	timeline := ""
	if destination != nil {
		d := *destination
		if !validRecoveryRetirementDestination(d) || server.SystemID != d.SystemID || server.WALTimeline != d.WALTimeline || server.DatabaseOID != d.MaintenanceOID || role != d.AdminOID || provenance == "" {
			return p, ErrRecoveryProvision
		}
		operation, timeline = d.RestoreID, d.WALTimeline
	}
	p = RecoveryProvisionIntent{Version: 1, OperationID: operation, SystemID: server.SystemID, WALTimeline: timeline, ServerStartedAt: server.Started, MaintenanceOID: server.DatabaseOID, AdminOID: role, Database: "rvg_target_" + operation[8:], Owner: "rvg_owner_" + operation[8:], BackupID: backup, SourceID: source, SnapshotSHA256: snapshot, SchemaSHA256: schema, CreatedAt: time.Now().UTC()}
	p.ProvenanceSHA256 = provenance
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
	return inspectRecoveryProvision(ctx, admin, p, 0)
}

func inspectRecoveryProvision(ctx context.Context, admin *pgx.Conn, p RecoveryProvisionIntent, runtimeOID uint32) (RecoveryProvisionObservation, error) {
	ob := RecoveryProvisionObservation{OperationID: p.OperationID, Database: p.Database, ObservedAt: time.Now().UTC()}
	if admin == nil || !validRecoveryProvision(p) {
		return ob, ErrRecoveryProvision
	}
	server, err := recoveryFenceServerIdentity(ctx, admin)
	if err != nil || server.SystemID != p.SystemID || server.DatabaseOID != p.MaintenanceOID || (p.WALTimeline != "" && server.WALTimeline != p.WALTimeline) {
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
	connectionLimit := 0
	if runtimeOID != 0 {
		connectionLimit = recoveryRuntimeConnectionLimit
	}
	err = admin.QueryRow(ctx, `SELECT d.oid,d.datallowconn,COALESCE(pg_catalog.shobj_description(d.oid,'pg_database'),''),
 d.datdba=$2 AND d.datconnlimit=$3 AND NOT d.datistemplate AND d.encoding=6 AND d.datlocprovider='c' AND d.datcollate='C' AND d.datctype='C' AND d.dattablespace=(SELECT oid FROM pg_catalog.pg_tablespace WHERE spcname='pg_default') AND
 NOT EXISTS(SELECT 1 FROM pg_catalog.aclexplode(COALESCE(d.datacl,pg_catalog.acldefault('d',d.datdba))) a
 WHERE a.grantee<>d.datdba AND NOT ($4<>0::oid AND a.grantee=$4 AND a.privilege_type='CONNECT' AND NOT a.is_grantable))
 AND ($4=0::oid OR EXISTS(SELECT 1 FROM pg_catalog.aclexplode(d.datacl) a
 WHERE a.grantee=$4 AND a.privilege_type='CONNECT' AND NOT a.is_grantable))
 FROM pg_catalog.pg_database d WHERE d.datname=$1`, p.Database, owner, connectionLimit, runtimeOID).Scan(&ob.DatabaseOID, &allowed, &marker, &restricted)
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
	if admin.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(datname=$2 AND datconnlimit=$3 AND NOT datistemplate AND encoding=6 AND datlocprovider='c' AND datcollate='C' AND datctype='C' AND dattablespace=(SELECT oid FROM pg_catalog.pg_tablespace WHERE spcname='pg_default')) FROM pg_catalog.pg_database WHERE datdba=$1`, owner, p.Database, connectionLimit).Scan(&exact) != nil || !exact {
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
	if runtimeOID != 0 {
		ob.State = "work_admitted"
		return ob, nil
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
	return openRecoveryProvisionTarget(ctx, admin, dsn, p, expectedOID, false)
}

// OpenAdmittedRecoveryProvisionTarget is the post-cutover maintenance path.
// It accepts neither a held database nor an ordinary runtime login.
func OpenAdmittedRecoveryProvisionTarget(ctx context.Context, admin *pgx.Conn, dsn string, p RecoveryProvisionIntent, expectedOID uint32) (*Store, error) {
	if !validRecoveryProvision(p) || p.WALTimeline == "" {
		return nil, ErrRecoveryProvision
	}
	return openRecoveryProvisionTarget(ctx, admin, dsn, p, expectedOID, true)
}

func openRecoveryProvisionTarget(ctx context.Context, admin *pgx.Conn, dsn string, p RecoveryProvisionIntent, expectedOID uint32, admitted bool) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, ErrRecoveryProvision
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		return recoveryProvisionWitness(ctx, admin, c, p, expectedOID, admitted)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, ErrRecoveryProvision
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, ErrRecoveryProvision
	}
	s := &Store{Pool: pool, runtimeSchema: "public"}
	if p.ProvenanceSHA256 != "" {
		s.recoveryProvision = &recoveryProvisionImportBinding{intent: p, databaseOID: expectedOID}
	}
	return s, nil
}

// Equal DSN names/control-file IDs alone do not establish that a routed
// connection reached the selected primary. Witness a nonce on the actual backend.
func recoveryProvisionWitness(ctx context.Context, admin, target *pgx.Conn, p RecoveryProvisionIntent, expectedOID uint32, admitted bool) error {
	var runtimeOID uint32
	state := "database_restricted"
	if admitted {
		state = "work_admitted"
		if admin == nil || admin.QueryRow(ctx, `SELECT r.oid FROM pg_catalog.pg_authid r WHERE r.rolname=$1 AND r.rolcanlogin
		 AND NOT (r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls OR r.rolinherit)
		 AND r.rolconnlimit=$2 AND r.rolvaliduntil IS NULL AND r.rolpassword LIKE 'SCRAM-SHA-256$4096:%'
		 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m WHERE m.member=r.oid OR m.roleid=r.oid)`,
			recoveryRuntimeRoleName(p.OperationID), recoveryRuntimeConnectionLimit).Scan(&runtimeOID) != nil || runtimeOID == 0 {
			return ErrRecoveryProvision
		}
	}
	ob, err := inspectRecoveryProvision(ctx, admin, p, runtimeOID)
	if err != nil || ob.State != state || ob.DatabaseOID != expectedOID || target == nil {
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
	if admitted {
		if tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.recovery_work_admissions a
		 JOIN public.runtime_recovery_control c ON c.singleton AND c.state='held' AND c.restore_id=a.restore_id AND c.installation_id=a.target_installation_id
		 JOIN public.recovery_import_receipts i ON i.restore_id=a.restore_id AND i.target_installation_id=a.target_installation_id
		 WHERE a.restore_id=$1 AND a.database_oid=$2 AND a.runtime_role=$3 AND a.runtime_role_oid=$4
		   AND i.backup_id=$5 AND i.source_id=$6 AND i.schema_sha256=$7 AND i.snapshot_sha256=$8)
		  AND NOT pg_catalog.has_schema_privilege($4::oid,'public','CREATE')`, p.OperationID, expectedOID, recoveryRuntimeRoleName(p.OperationID),
			runtimeOID, p.BackupID, p.SourceID, p.SchemaSHA256, p.SnapshotSHA256).Scan(&witnessed) != nil || !witnessed {
			return ErrRecoveryProvision
		}
	}
	if tx.Commit(ctx) != nil {
		return ErrRecoveryProvision
	}
	return nil
}

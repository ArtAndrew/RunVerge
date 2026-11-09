// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

var ErrRecoverySourceRetirement = errors.New("Compose source retirement not confirmed; inspect the saved intent")
var ErrRecoverySourceRetirementUnknown = errors.New("source retirement commit outcome unknown; inspect the saved retirement intent before any further operation")

// The selected administrator and database on a different primary own this
// single restore slot. The record contains no connection credentials.
type RecoveryRetirementDestination struct {
	SystemID       string `json:"system_id"`
	WALTimeline    string `json:"wal_timeline"`
	MaintenanceOID uint32 `json:"maintenance_oid"`
	AdminOID       uint32 `json:"admin_oid"`
	RestoreID      string `json:"restore_id"`
}

func PlanRecoveryRetirementDestination(ctx context.Context, admin *pgx.Conn) (RecoveryRetirementDestination, error) {
	return readRecoveryRetirementDestination(ctx, admin, domain.NewID("restore"))
}

func readRecoveryRetirementDestination(ctx context.Context, admin *pgx.Conn, restore string) (RecoveryRetirementDestination, error) {
	if admin == nil {
		return RecoveryRetirementDestination{}, ErrRecoverySourceRetirement
	}
	s, err := recoveryFenceServerIdentity(ctx, admin)
	var role uint32
	if err != nil || admin.QueryRow(ctx, `SELECT oid FROM pg_catalog.pg_roles WHERE rolname=SESSION_USER AND rolname=CURRENT_USER`).Scan(&role) != nil {
		return RecoveryRetirementDestination{}, ErrRecoverySourceRetirement
	}
	d := RecoveryRetirementDestination{s.SystemID, s.WALTimeline, s.DatabaseOID, role, restore}
	if !validRecoveryRetirementDestination(d) {
		return RecoveryRetirementDestination{}, ErrRecoverySourceRetirement
	}
	return d, nil
}

// Check the reserved slot before committing an irreversible source retirement.
// This is a fresh readiness check, not a distributed transaction or a promise
// that the destination will remain online throughout the cutover.
func CheckRecoveryRetirementDestination(ctx context.Context, admin *pgx.Conn, expected RecoveryRetirementDestination) error {
	d, err := readRecoveryRetirementDestination(ctx, admin, expected.RestoreID)
	if err != nil || d != expected {
		return ErrRecoverySourceRetirement
	}
	suffix := d.RestoreID[len("restore_"):]
	var unused bool
	if admin.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE datname=$1) AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=$2)`, "rvg_target_"+suffix, "rvg_owner_"+suffix).Scan(&unused) != nil || !unused {
		return ErrRecoverySourceRetirement
	}
	return nil
}

func validRecoveryRetirementServer(system, timeline string) bool {
	id, idErr := strconv.ParseUint(system, 10, 64)
	line, lineErr := strconv.ParseUint(timeline, 16, 32)
	return idErr == nil && id != 0 && strconv.FormatUint(id, 10) == system && lineErr == nil && line != 0 && len(timeline) == 8
}

func validRecoveryRetirementDestination(d RecoveryRetirementDestination) bool {
	return validRecoveryRetirementServer(d.SystemID, d.WALTimeline) && d.MaintenanceOID != 0 && d.AdminOID != 0 && validRecoveryID(d.RestoreID, "restore")
}

type RecoverySourceRetirementIntent struct {
	Version            int                           `json:"version"`
	SystemID           string                        `json:"system_id"`
	WALTimeline        string                        `json:"wal_timeline"`
	ServerStartedAt    time.Time                     `json:"server_started_at"`
	MaintenanceOID     uint32                        `json:"maintenance_oid"`
	MaintenanceRoleOID uint32                        `json:"maintenance_role_oid"`
	SourceOID          uint32                        `json:"source_oid"`
	ApplicationRoleOID uint32                        `json:"application_role_oid"`
	CaptureLinkSHA256  string                        `json:"capture_link_sha256"`
	BaselineSHA256     string                        `json:"baseline_sha256,omitempty"`
	SourceInstallation string                        `json:"source_installation_id"`
	Destination        RecoveryRetirementDestination `json:"destination"`
	CreatedAt          time.Time                     `json:"created_at"`
}

type RecoverySourceRetirementObservation struct {
	State                string                        `json:"state"`
	SourceInstallation   string                        `json:"source_installation_id"`
	CaptureLinkSHA256    string                        `json:"capture_link_sha256"`
	Destination          RecoveryRetirementDestination `json:"destination"`
	SourceConnections    bool                          `json:"source_connections_allowed"`
	ApplicationLogin     bool                          `json:"application_login_allowed"`
	MaintenanceOwnsDB    bool                          `json:"maintenance_owns_database"`
	Sessions             int                           `json:"sessions"`
	PreparedTransactions int                           `json:"prepared_transactions"`
	ObservedAt           time.Time                     `json:"observed_at"`
	ReceiptSHA256        string                        `json:"receipt_sha256,omitempty"`
	CutProofSHA256       string                        `json:"cut_proof_sha256,omitempty"`
	PostmasterChanged    bool                          `json:"postmaster_changed,omitempty"`
}

func (o RecoverySourceRetirementObservation) ConfirmsRetirement(p RecoverySourceRetirementIntent) bool {
	return validSourceRetirementVersion(p) && o.State == "source_admission_retired" &&
		o.SourceInstallation == p.SourceInstallation && o.CaptureLinkSHA256 == p.CaptureLinkSHA256 && o.Destination == p.Destination &&
		!o.SourceConnections && !o.ApplicationLogin && o.MaintenanceOwnsDB && o.Sessions == 0 && o.PreparedTransactions == 0 &&
		validDigest(o.ReceiptSHA256) && (p.Version == 1 && o.CutProofSHA256 == "" || p.Version == 2 && validDigest(o.CutProofSHA256))
}

// Plan pins the source to the drained, authenticated Compose export. The
// source backend and the postgres maintenance connection witness each other
// on the same live primary. The host must save the intent before applying.
func PlanRecoverySourceRetirement(ctx context.Context, admin *pgx.Conn, source *Store, link *recoveryarchive.VerifiedCaptureLink, dest RecoveryRetirementDestination) (RecoverySourceRetirementIntent, error) {
	var empty RecoverySourceRetirementIntent
	if admin == nil || source == nil || link == nil || !validRecoveryRetirementDestination(dest) {
		return empty, ErrRecoverySourceRetirement
	}
	l := link.Link()
	if l.Version != 2 || l.ExportAlgorithm != recoveryarchive.DrainedCaptureAlgorithm || l.SystemID == dest.SystemID {
		return empty, ErrRecoverySourceRetirement
	}
	id, err := recoveryFenceServerIdentity(ctx, admin)
	if err != nil || id.SystemID != l.SystemID || id.DatabaseOID == l.DatabaseOID || !isComposeRetirementMaintenance(admin) {
		return empty, ErrRecoverySourceRetirement
	}
	oid, name, state, err := witnessRecoveryFenceDatabase(ctx, admin, source)
	if err != nil || oid != l.DatabaseOID || name != "runverge" || state.State != "normal" || state.InstallationID != l.SourceID {
		return empty, ErrRecoverySourceRetirement
	}
	p := RecoverySourceRetirementIntent{
		Version: 1, SystemID: id.SystemID, WALTimeline: id.WALTimeline, ServerStartedAt: id.Started, MaintenanceOID: id.DatabaseOID,
		SourceOID: oid, CaptureLinkSHA256: link.SHA256(), SourceInstallation: l.SourceID, Destination: dest, CreatedAt: time.Now().UTC(),
	}
	o, err := readRecoveryRetirementCatalog(ctx, admin, p)
	if err != nil || !o.ConnectionsAllowed || !o.login || o.ownerOID != o.roleOID || len(o.settings) != 0 || len(o.cutSettings) != 0 || o.PreparedTransactions != 0 {
		return empty, ErrRecoverySourceRetirement
	}
	p.ApplicationRoleOID = o.roleOID
	p.MaintenanceRoleOID = o.adminOID
	return p, nil
}

func isComposeRetirementMaintenance(c *pgx.Conn) bool {
	return c.Config().User == "runverge_admin" && c.Config().Database == "postgres"
}

type recoveryRetirementCatalog struct {
	recoveryFenceCatalog
	roleOID     uint32
	ownerOID    uint32
	adminOID    uint32
	roleXMIN    string
	login       bool
	cutSettings []string
	cutXMIN     string
}

func readRecoveryRetirementCatalog(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, p RecoverySourceRetirementIntent) (recoveryRetirementCatalog, error) {
	var o recoveryRetirementCatalog
	err := q.QueryRow(ctx, `SELECT d.datallowconn,app.rolcanlogin,app.oid,d.datdba,admin.oid,app.xmin::pg_catalog.text,
		(SELECT pg_catalog.count(*) FROM pg_catalog.pg_stat_activity a WHERE a.datid=d.oid OR a.usesysid=app.oid),
		(SELECT pg_catalog.count(*) FROM pg_catalog.pg_prepared_xacts x WHERE x.database=d.datname OR x.owner=app.rolname),
		pg_catalog.clock_timestamp(),`+recoveryFenceSettings+`,d.xmin::pg_catalog.text,COALESCE(r.xmin::pg_catalog.text,''),
		pg_catalog.pg_snapshot_xmax(pg_catalog.pg_current_snapshot())::pg_catalog.text,
		ARRAY(SELECT name FROM pg_catalog.unnest(c.setconfig) AS config(name) WHERE pg_catalog.starts_with(name, '`+recoveryRetirementCutSetting+`=')),COALESCE(c.xmin::pg_catalog.text,'')
		FROM pg_catalog.pg_database d CROSS JOIN pg_catalog.pg_authid app CROSS JOIN pg_catalog.pg_authid admin
		LEFT JOIN pg_catalog.pg_db_role_setting r ON r.setdatabase=d.oid AND r.setrole=0
		LEFT JOIN pg_catalog.pg_db_role_setting c ON c.setdatabase=d.oid AND c.setrole=admin.oid
		WHERE d.oid=$1 AND d.datname='runverge' AND NOT d.datistemplate AND d.datconnlimit<>-2
		AND app.rolname='runverge' AND ($2::pg_catalog.oid=0 OR app.oid=$2) AND NOT app.rolsuper AND NOT app.rolcreatedb
		AND NOT app.rolcreaterole AND NOT app.rolinherit AND NOT app.rolreplication AND NOT app.rolbypassrls
		AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m WHERE m.member=app.oid OR m.roleid=app.oid)
		AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_database other WHERE other.datdba=app.oid AND other.oid<>d.oid)
		AND admin.rolname=SESSION_USER AND admin.rolsuper AND ($3::pg_catalog.oid=0 OR admin.oid=$3)
		AND SESSION_USER='runverge_admin' AND CURRENT_USER=SESSION_USER AND pg_catalog.current_database()='postgres'`, p.SourceOID, p.ApplicationRoleOID, p.MaintenanceRoleOID).
		Scan(&o.ConnectionsAllowed, &o.login, &o.roleOID, &o.ownerOID, &o.adminOID, &o.roleXMIN, &o.Sessions, &o.PreparedTransactions,
			&o.ObservedAt, &o.settings, &o.sourceXMIN, &o.settingsXMIN, &o.snapshotXMAX, &o.cutSettings, &o.cutXMIN)
	if err != nil {
		return recoveryRetirementCatalog{}, ErrRecoverySourceRetirement
	}
	return o, nil
}

func retirementServerAndPrefix(ctx context.Context, admin *pgx.Conn, p RecoverySourceRetirementIntent) (recoveryFenceServer, string, error) {
	if admin == nil || !isComposeRetirementMaintenance(admin) || !validSourceRetirementVersion(p) || !validRecoveryRetirementServer(p.SystemID, p.WALTimeline) || p.CreatedAt.IsZero() || p.ServerStartedAt.IsZero() || p.MaintenanceOID == 0 || p.SourceOID == 0 || p.SourceOID == p.MaintenanceOID || p.ApplicationRoleOID == 0 || p.MaintenanceRoleOID == 0 || p.MaintenanceRoleOID == p.ApplicationRoleOID || !validRecoveryID(p.SourceInstallation, "inst") || !validDigest(p.CaptureLinkSHA256) || !validRecoveryRetirementDestination(p.Destination) || p.SystemID == p.Destination.SystemID {
		return recoveryFenceServer{}, "", ErrRecoverySourceRetirement
	}
	id, err := recoveryFenceServerIdentity(ctx, admin)
	if err != nil || id.SystemID != p.SystemID || id.WALTimeline != p.WALTimeline || id.DatabaseOID != p.MaintenanceOID {
		return recoveryFenceServer{}, "", ErrRecoverySourceRetirement
	}
	p.ServerStartedAt, p.CreatedAt = p.ServerStartedAt.UTC(), p.CreatedAt.UTC()
	raw, err := json.Marshal(p)
	if err != nil {
		return recoveryFenceServer{}, "", ErrRecoverySourceRetirement
	}
	return id, Hash(append([]byte("runverge/compose-source-retirement/v1\x00"), raw...)) + ":", nil
}

func validSourceRetirementVersion(p RecoverySourceRetirementIntent) bool {
	return (p.Version == 1 && p.BaselineSHA256 == "") || (p.Version == 2 && validDigest(p.BaselineSHA256))
}

// Inspect is a current catalog check. It works after a same-primary restart
// only for the exact committed marker. A historical receipt alone says nothing
// about the current source or about pending external effects.
func InspectRecoverySourceRetirement(ctx context.Context, admin *pgx.Conn, p RecoverySourceRetirementIntent) (RecoverySourceRetirementObservation, error) {
	var empty RecoverySourceRetirementObservation
	id, prefix, err := retirementServerAndPrefix(ctx, admin, p)
	if err != nil {
		return empty, err
	}
	if _, err = admin.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()"); err != nil {
		return empty, ErrRecoverySourceRetirement
	}
	o, err := readRecoveryRetirementCatalog(ctx, admin, p)
	if err != nil {
		return empty, err
	}
	result := RecoverySourceRetirementObservation{
		SourceInstallation: p.SourceInstallation, CaptureLinkSHA256: p.CaptureLinkSHA256, Destination: p.Destination,
		SourceConnections: o.ConnectionsAllowed, ApplicationLogin: o.login, MaintenanceOwnsDB: o.ownerOID == o.adminOID, Sessions: o.Sessions,
		PreparedTransactions: o.PreparedTransactions, ObservedAt: o.ObservedAt, PostmasterChanged: !id.Started.Equal(p.ServerStartedAt),
	}
	xid, retired := o.retiredXID(prefix)
	if retired {
		result.State = "admission_disabled_unresolved"
		if o.Sessions == 0 && o.PreparedTransactions == 0 {
			result.State = "source_admission_retired"
		}
		result.ReceiptSHA256 = Hash([]byte(o.settings[0]))
		if len(o.cutSettings) != 0 {
			result.CutProofSHA256 = o.committedCutProof(p, result.ReceiptSHA256, xid)
			if result.CutProofSHA256 == "" {
				return empty, ErrRecoverySourceCut
			}
		}
		return result, nil
	}
	if !result.PostmasterChanged && len(o.settings) == 0 && len(o.cutSettings) == 0 && o.ConnectionsAllowed && o.login && o.ownerOID == o.roleOID {
		result.State = "source_admission_open"
		return result, nil
	}
	return empty, ErrRecoverySourceRetirement
}

// Apply changes only the dedicated Compose database and its dedicated owner.
// The application loses its login and database ownership in the same commit
// as the database ban. Even a connection finishing authentication during
// retirement cannot use the old owner authority to reopen the database.
func ApplyRecoverySourceRetirement(ctx context.Context, admin *pgx.Conn, p RecoverySourceRetirementIntent) (RecoverySourceRetirementObservation, error) {
	return applyRecoverySourceRetirement(ctx, admin, p, nil)
}

func lockRecoveryRetirementCatalog(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='15s'; SET LOCAL transaction_timeout='25s'; SET LOCAL synchronous_commit=on;
		LOCK TABLE pg_catalog.pg_database,pg_catalog.pg_authid,pg_catalog.pg_auth_members,pg_catalog.pg_db_role_setting IN SHARE ROW EXCLUSIVE MODE`)
	return err
}

func applyRecoverySourceRetirement(ctx context.Context, admin *pgx.Conn, p RecoverySourceRetirementIntent, keeper *recoverySourceCutBackend) (RecoverySourceRetirementObservation, error) {
	var empty RecoverySourceRetirementObservation
	if (p.Version == 2) != (keeper != nil) {
		return empty, ErrRecoverySourceCut
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	id, prefix, err := retirementServerAndPrefix(ctx, admin, p)
	age := time.Since(p.CreatedAt)
	if err != nil || !id.Started.Equal(p.ServerStartedAt) || age < 0 || age > 15*time.Minute {
		return empty, ErrRecoverySourceRetirement
	}
	tx, err := admin.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return empty, ErrRecoverySourceRetirement
	}
	defer recoveryFenceRollback(tx, admin)
	if err = lockRecoveryRetirementCatalog(ctx, tx); err != nil {
		return empty, ErrRecoverySourceRetirement
	}
	before, err := readRecoveryRetirementCatalog(ctx, tx, p)
	if err != nil || !before.ConnectionsAllowed || !before.login || before.ownerOID != before.roleOID || len(before.settings) != 0 || len(before.cutSettings) != 0 || before.PreparedTransactions != 0 || !keeper.matches(ctx, tx, p, before.Sessions) {
		return empty, ErrRecoverySourceRetirement
	}
	if _, err = tx.Exec(ctx, `ALTER ROLE runverge NOLOGIN; ALTER DATABASE runverge ALLOW_CONNECTIONS false; ALTER DATABASE runverge OWNER TO runverge_admin`); err != nil {
		return empty, ErrRecoverySourceRetirement
	}
	var stamp string
	if tx.QueryRow(ctx, "SELECT pg_catalog.pg_current_xact_id()::pg_catalog.text").Scan(&stamp) != nil {
		return empty, ErrRecoverySourceRetirement
	}
	xid, err := strconv.ParseUint(stamp, 10, 64)
	if err != nil || xid < 3 {
		return empty, ErrRecoverySourceRetirement
	}
	value := prefix + strconv.FormatUint(xid, 10)
	if _, err = tx.Exec(ctx, "ALTER DATABASE runverge SET "+recoveryFenceSetting+" = '"+value+"'"); err != nil {
		return empty, ErrRecoverySourceRetirement
	}
	after, err := readRecoveryRetirementCatalog(ctx, tx, p)
	if err != nil || after.ConnectionsAllowed || after.login || after.ownerOID != after.adminOID || after.PreparedTransactions != 0 || len(after.settings) != 1 || after.settings[0] != recoveryFenceSetting+"="+value || !after.matchesFenceTransaction(xid) || after.roleXMIN != strconv.FormatUint(uint64(uint32(xid)), 10) || (keeper != nil && !keeper.matches(ctx, tx, p, after.Sessions)) {
		return empty, ErrRecoverySourceRetirement
	}
	if err = tx.Commit(ctx); err != nil {
		recoveryFenceClose(admin)
		return empty, ErrRecoverySourceRetirementUnknown
	}
	return InspectRecoverySourceRetirement(ctx, admin, p)
}

// The host may recover an acknowledged or uncertain result only by inspecting
// the original source and the saved intent for this exact capture and slot.
func (p RecoverySourceRetirementIntent) MatchesCapture(link *recoveryarchive.VerifiedCaptureLink, dest RecoveryRetirementDestination) bool {
	if link == nil || !validSourceRetirementVersion(p) || p.CaptureLinkSHA256 != link.SHA256() || p.Destination != dest {
		return false
	}
	l := link.Link()
	return l.Version == 2 && l.SourceID == p.SourceInstallation && l.SystemID == p.SystemID && l.DatabaseOID == p.SourceOID && (p.Version == 1 || p.BaselineSHA256 == l.EvidenceSnapshotSHA256)
}

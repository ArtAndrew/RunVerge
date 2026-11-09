// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	recoveryRetirementCutSetting = "runverge.recovery_cut_v1"
	recoverySourceCutSchema      = "public"
)

var ErrRecoverySourceCut = errors.New("captured source data continuity not confirmed; inspect the saved intent and keep the destination held")

// RecoverySourceCut owns one read-only source transaction. Its locks are never
// recreated after a disconnect. Only the process that verified this archive
// and held the original transaction can record the cut proof.
type RecoverySourceCut struct {
	intent   RecoverySourceRetirementIntent
	conn     *pgx.Conn
	tx       pgx.Tx
	keeper   recoverySourceCutBackend
	snapshot RecoverySnapshotReport
	limits   RecoverySnapshotLimits
}

type recoverySourceCutBackend struct {
	pid         int
	started     time.Time
	transaction time.Time
	application string
}

func (b *recoverySourceCutBackend) matches(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, p RecoverySourceRetirementIntent, sessions int) bool {
	if b == nil {
		return sessions == 0
	}
	if sessions != 1 {
		return false
	}
	if _, err := q.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()"); err != nil {
		return false
	}
	var onlyKeeper bool
	err := q.QueryRow(ctx, `SELECT pg_catalog.count(*)=1 AND COALESCE(pg_catalog.bool_and(
		pid=$3 AND backend_start=$4 AND xact_start=$5 AND application_name=$6 AND datid=$1 AND usesysid=$7 AND state='idle in transaction'),false)
		FROM pg_catalog.pg_stat_activity WHERE datid=$1 OR usesysid=$2`,
		p.SourceOID, p.ApplicationRoleOID, b.pid, b.started, b.transaction, b.application, p.MaintenanceRoleOID).Scan(&onlyKeeper)
	return err == nil && onlyKeeper
}

func PrepareRecoverySourceCut(ctx context.Context, admin *pgx.Conn, source *Store, archive *recoveryarchive.VerifiedArchive, link *recoveryarchive.VerifiedCaptureLink, dest RecoveryRetirementDestination, limits RecoverySnapshotLimits) (*RecoverySourceCut, error) {
	if source == nil || source.runtimeSchema != recoverySourceCutSchema || archive == nil || link == nil || link.Link().Version != 2 || !link.MatchesEvidenceManifest(archive.Manifest()) || !limits.valid() {
		return nil, ErrRecoverySourceCut
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	snapshot, err := ValidateRecoveryHostSnapshot(ctx, archive, limits)
	if err != nil || snapshot.Format != RecoverySnapshotFormat || snapshot.Lineage != nil || snapshot.SHA256 != link.Link().EvidenceSnapshotSHA256 {
		return nil, ErrRecoverySourceCut
	}
	p, err := PlanRecoverySourceRetirement(ctx, admin, source, link, dest)
	if err != nil {
		return nil, err
	}
	p.Version, p.BaselineSHA256 = 2, snapshot.SHA256
	borrowed, err := source.Pool.Acquire(ctx)
	if err != nil {
		return nil, ErrRecoverySourceCut
	}
	cut := &RecoverySourceCut{intent: p, conn: borrowed.Hijack(), snapshot: snapshot, limits: limits}
	if err := cut.prepare(ctx); err != nil {
		cut.Close()
		return nil, err
	}
	return cut, nil
}

func (c *RecoverySourceCut) prepare(ctx context.Context) error {
	tx, err := c.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrRecoverySourceCut
	}
	c.tx = tx
	if _, err = tx.Exec(ctx, `SET LOCAL search_path=pg_catalog,pg_temp; SET LOCAL row_security=off; SET LOCAL max_parallel_workers_per_gather=0;
		SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='60s'; SET LOCAL idle_in_transaction_session_timeout='60s';
		SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079);
		SELECT pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext('public'))`); err != nil {
		return ErrRecoverySourceCut
	}
	h, _, err := recoverySnapshotInspect(ctx, tx, recoverySourceCutSchema, nil)
	if err != nil || h.Metadata.SourceID != c.intent.SourceInstallation || h.Metadata.SchemaSHA256 != c.snapshot.Metadata.SchemaSHA256 || h.InventorySHA256 != c.snapshot.InventorySHA256 {
		return ErrRecoverySourceCut
	}
	// The advisory gate coordinates RunVerge. These relation locks additionally
	// block direct writes to the reviewed tables until the database is retired.
	for _, table := range h.Inventory.Tables {
		if _, err = tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{recoverySourceCutSchema, table.Name}.Sanitize()+" IN SHARE MODE"); err != nil {
			return ErrRecoverySourceCut
		}
	}
	c.keeper.application = domain.NewID("cut")
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.set_config('application_name',$1,true)", c.keeper.application); err != nil {
		return ErrRecoverySourceCut
	}
	var system string
	var started time.Time
	var oid, role uint32
	err = tx.QueryRow(ctx, `SELECT sys.system_identifier::pg_catalog.text,pg_catalog.pg_postmaster_start_time(),
		a.datid,a.usesysid,a.pid,a.backend_start,pg_catalog.transaction_timestamp()
		FROM pg_catalog.pg_control_system() sys, pg_catalog.pg_stat_activity a
		WHERE a.pid=pg_catalog.pg_backend_pid() AND pg_catalog.current_database()='runverge'
		AND SESSION_USER='runverge_admin' AND CURRENT_USER=SESSION_USER AND NOT pg_catalog.pg_is_in_recovery()`).
		Scan(&system, &started, &oid, &role, &c.keeper.pid, &c.keeper.started, &c.keeper.transaction)
	p := c.intent
	if err != nil || system != p.SystemID || oid != p.SourceOID || role != p.MaintenanceRoleOID || !started.Equal(p.ServerStartedAt) {
		return ErrRecoverySourceCut
	}
	return c.compare(ctx)
}

func (c *RecoverySourceCut) checkCatalog(ctx context.Context) (RecoverySnapshotInventory, error) {
	h, _, err := recoverySnapshotInspect(ctx, c.tx, recoverySourceCutSchema, nil)
	if err != nil || h.Metadata.SourceID != c.intent.SourceInstallation || h.Metadata.SchemaSHA256 != c.snapshot.Metadata.SchemaSHA256 || h.InventorySHA256 != c.snapshot.InventorySHA256 {
		return RecoverySnapshotInventory{}, ErrRecoverySourceCut
	}
	if _, err = validateRecoverySemanticCatalog(ctx, c.tx, recoverySourceCutSchema); err != nil {
		return RecoverySnapshotInventory{}, ErrRecoverySourceCut
	}
	return h.Inventory, nil
}

func (c *RecoverySourceCut) compare(ctx context.Context) error {
	inventory, err := c.checkCatalog(ctx)
	if err != nil {
		return err
	}
	if err := recoveryStageCheckDigests(ctx, c.tx, recoverySourceCutSchema, inventory, c.snapshot, c.limits); err != nil {
		return ErrRecoverySourceCut
	}
	return c.checkState(ctx)
}

func (c *RecoverySourceCut) checkState(ctx context.Context) error {
	state, err := recoverySnapshotReadState(ctx, c.tx, recoverySourceCutSchema, c.limits)
	if err != nil || !slices.Equal(state.Sequences, c.snapshot.Sequences) ||
		!slices.Equal(state.Organizations, c.snapshot.Organizations) || !slices.Equal(state.KeyReferences, c.snapshot.KeyReferences) {
		return ErrRecoverySourceCut
	}
	return nil
}

func (c *RecoverySourceCut) Intent() RecoverySourceRetirementIntent { return c.intent }

func (c *RecoverySourceCut) Close() {
	if c == nil || c.conn == nil {
		return
	}
	// The connection was removed from the pool. Closing it also rolls back
	// the read-only transaction and prevents a now-banned session being reused.
	recoveryFenceClose(c.conn)
	c.conn, c.tx = nil, nil
}

func (c *RecoverySourceCut) Retire(ctx context.Context, admin *pgx.Conn) (RecoverySourceRetirementObservation, error) {
	if c == nil || c.conn == nil || c.conn.IsClosed() {
		return RecoverySourceRetirementObservation{}, ErrRecoverySourceCut
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	// Relation locks protect the compared rows. Recheck the live catalog and
	// non-transactional sequence state immediately before changing admission.
	if _, err := c.checkCatalog(ctx); err != nil {
		return RecoverySourceRetirementObservation{}, err
	}
	if err := c.checkState(ctx); err != nil {
		return RecoverySourceRetirementObservation{}, err
	}
	return applyRecoverySourceRetirement(ctx, admin, c.intent, &c.keeper)
}

func (o recoveryRetirementCatalog) retiredXID(prefix string) (uint64, bool) {
	xid, ok := o.committedFenceXID(prefix)
	return xid, ok && !o.login && o.ownerOID == o.adminOID && o.roleXMIN == strconv.FormatUint(uint64(uint32(xid)), 10)
}

func sourceCutProofPrefix(p RecoverySourceRetirementIntent, receipt string) string {
	return Hash([]byte("runverge/compose-source-cut/v1\x00"+p.BaselineSHA256+":"+receipt)) + ":"
}

func (o recoveryRetirementCatalog) committedCutProof(p RecoverySourceRetirementIntent, receipt string, baseXID uint64) string {
	if p.Version != 2 || len(o.cutSettings) != 1 {
		return ""
	}
	value, matches := strings.CutPrefix(o.cutSettings[0], recoveryRetirementCutSetting+"="+sourceCutProofPrefix(p, receipt))
	xid, err := strconv.ParseUint(value, 10, 64)
	next, nextErr := strconv.ParseUint(o.snapshotXMAX, 10, 64)
	if !matches || err != nil || nextErr != nil || strconv.FormatUint(xid, 10) != value || xid <= baseXID ||
		next <= xid || next-xid >= 1<<31 || o.cutXMIN != strconv.FormatUint(uint64(uint32(xid)), 10) {
		return ""
	}
	return Hash([]byte(o.cutSettings[0]))
}

// Confirm verifies the same data while the original locks still exist, after
// the database ban has committed and any previously admitted startup has ended.
// It never retries the admission mutation, even after a lost COMMIT response.
func (c *RecoverySourceCut) Confirm(ctx context.Context, admin *pgx.Conn) error {
	if c == nil || c.conn == nil || c.conn.IsClosed() {
		return ErrRecoverySourceCut
	}
	p := c.intent
	ctx, cancel := context.WithTimeout(ctx, c.limits.MaxDuration)
	defer cancel()
	id, prefix, err := retirementServerAndPrefix(ctx, admin, p)
	if err != nil || !id.Started.Equal(p.ServerStartedAt) {
		return ErrRecoverySourceCut
	}
	if err := c.waitForRetiredSource(ctx, admin, prefix); err != nil {
		return err
	}
	if err := c.compare(ctx); err != nil {
		return err
	}
	tx, err := admin.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return ErrRecoverySourceCut
	}
	defer recoveryFenceRollback(tx, admin)
	if err = lockRecoveryRetirementCatalog(ctx, tx); err != nil {
		return ErrRecoverySourceCut
	}
	o, err := readRecoveryRetirementCatalog(ctx, tx, p)
	baseXID, retired := o.retiredXID(prefix)
	if err != nil || !retired || o.PreparedTransactions != 0 || !c.keeper.matches(ctx, tx, p, o.Sessions) {
		return ErrRecoverySourceCut
	}
	receipt := Hash([]byte(o.settings[0]))
	if len(o.cutSettings) != 0 {
		if o.committedCutProof(p, receipt, baseXID) == "" {
			return ErrRecoverySourceCut
		}
		return nil
	}
	var stamp string
	if tx.QueryRow(ctx, "SELECT pg_catalog.pg_current_xact_id()::pg_catalog.text").Scan(&stamp) != nil {
		return ErrRecoverySourceCut
	}
	xid, err := strconv.ParseUint(stamp, 10, 64)
	if err != nil || xid <= baseXID || stamp != strconv.FormatUint(xid, 10) {
		return ErrRecoverySourceCut
	}
	value := sourceCutProofPrefix(p, receipt) + stamp
	// Store the proof under the maintenance identity. An already
	// authenticated application role can still change its own defaults.
	if _, err = tx.Exec(ctx, "ALTER ROLE runverge_admin IN DATABASE runverge SET "+recoveryRetirementCutSetting+" = '"+value+"'"); err != nil {
		return ErrRecoverySourceCut
	}
	after, err := readRecoveryRetirementCatalog(ctx, tx, p)
	afterXID, retired := after.retiredXID(prefix)
	if err != nil || !retired || afterXID != baseXID || after.PreparedTransactions != 0 || len(after.cutSettings) != 1 ||
		after.cutSettings[0] != recoveryRetirementCutSetting+"="+value || after.cutXMIN != strconv.FormatUint(uint64(uint32(xid)), 10) ||
		!c.keeper.matches(ctx, tx, p, after.Sessions) {
		return ErrRecoverySourceCut
	}
	if err = tx.Commit(ctx); err != nil {
		recoveryFenceClose(admin)
		return ErrRecoverySourceRetirementUnknown
	}
	return nil
}

func (c *RecoverySourceCut) waitForRetiredSource(ctx context.Context, admin *pgx.Conn, prefix string) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	p := c.intent
	for {
		// PG18 InitPostgres holds this lock from before checking datallowconn
		// until it registers activity. Observe locks before observing sessions.
		var noStartup bool
		err := admin.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_catalog.pg_locks
			WHERE locktype='object' AND classid='pg_catalog.pg_database'::pg_catalog.regclass AND objid=$1
			AND objsubid=0 AND mode='RowExclusiveLock')`, p.SourceOID).Scan(&noStartup)
		if err != nil {
			return ErrRecoverySourceCut
		}
		o, err := readRecoveryRetirementCatalog(ctx, admin, p)
		if _, retired := o.retiredXID(prefix); err != nil || !retired || o.PreparedTransactions != 0 {
			return ErrRecoverySourceCut
		}
		if noStartup && c.keeper.matches(ctx, admin, p, o.Sessions) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ErrRecoverySourceCut
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func retirementTestURL(t *testing.T, key, user string) *url.URL {
	t.Helper()
	u, err := url.Parse(os.Getenv(key))
	if err != nil || u.User == nil || u.User.Username() != user || u.Hostname() != "127.0.0.1" || net.ParseIP(u.Hostname()) == nil || u.Port() == "" || u.Path != "/postgres" || u.RawQuery != "sslmode=disable" {
		t.Fatal("retirement tests require a newly owned loopback maintenance connection")
	}
	return u
}

func testPrepareRecoverySourceCut(t *testing.T, ctx context.Context, admin *pgx.Conn, source *Store, original, late *recoveryarchive.VerifiedArchive, link *recoveryarchive.VerifiedCaptureLink, dest RecoveryRetirementDestination, limits RecoverySnapshotLimits, org string) *RecoverySourceCut {
	t.Helper()
	reject := func(archive *recoveryarchive.VerifiedArchive, reason string) {
		t.Helper()
		cut, err := PrepareRecoverySourceCut(ctx, admin, source, archive, link, dest, limits)
		if cut != nil {
			cut.Close()
		}
		if cut != nil || !errors.Is(err, ErrRecoverySourceCut) {
			t.Fatal("cut accepted a mismatching baseline", reason, err)
		}
	}
	reject(original, "different authenticated archive")
	var name string
	if err := source.Pool.QueryRow(ctx, "SELECT name FROM public.organizations WHERE id=$1", org).Scan(&name); err != nil {
		t.Fatal("owned organization unavailable", err)
	}
	update := "UPDATE public.organizations SET name=$2 WHERE id=$1"
	if _, err := source.Pool.Exec(ctx, update, org, name+" drift"); err != nil {
		t.Fatal("could not alter a row after capture", err)
	}
	reject(late, "same-count row mutation")
	if _, err := source.Pool.Exec(ctx, update, org, name); err != nil {
		t.Fatal("could not restore the owned row", err)
	}
	var last int64
	var called bool
	if err := source.Pool.QueryRow(ctx, "SELECT last_value,is_called FROM public.events_sequence_seq").Scan(&last, &called); err != nil {
		t.Fatal("could not read the owned sequence", err)
	}
	advance := func() {
		t.Helper()
		if _, err := source.Pool.Exec(ctx, "SELECT pg_catalog.nextval('public.events_sequence_seq')"); err != nil {
			t.Fatal("could not advance the owned sequence", err)
		}
	}
	restoreSequence := func() {
		t.Helper()
		if _, err := source.Pool.Exec(ctx, "SELECT pg_catalog.setval('public.events_sequence_seq',$1,$2)", last, called); err != nil {
			t.Fatal("could not restore the owned sequence", err)
		}
	}
	advance()
	reject(late, "sequence drift without a row mutation")
	restoreSequence()
	acquire := func() *RecoverySourceCut {
		t.Helper()
		cut, err := PrepareRecoverySourceCut(ctx, admin, source, late, link, dest, limits)
		if err != nil {
			t.Fatal("exact baseline did not acquire the source cut", err)
		}
		t.Cleanup(cut.Close)
		return cut
	}
	cut := acquire()
	if _, err := source.Pool.Exec(ctx, "CREATE TABLE public.cut_drift_fixture(id integer)"); err != nil {
		t.Fatal("could not add an owned schema object after acquiring the cut", err)
	}
	if _, err := cut.Retire(ctx, admin); !errors.Is(err, ErrRecoverySourceCut) {
		t.Fatal("cut accepted a schema change after preparation", err)
	}
	if err := cut.compare(ctx); !errors.Is(err, ErrRecoverySourceCut) {
		t.Fatal("final comparison accepted a schema change after preparation", err)
	}
	cut.Close()
	if _, err := source.Pool.Exec(ctx, "DROP TABLE public.cut_drift_fixture"); err != nil {
		t.Fatal("could not remove the owned schema object", err)
	}
	cut = acquire()
	advance()
	if _, err := cut.Retire(ctx, admin); !errors.Is(err, ErrRecoverySourceCut) {
		t.Fatal("cut accepted a sequence advance after preparation", err)
	}
	restoreSequence()
	writer, err := source.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal("owned competing writer unavailable", err)
	}
	defer writer.Release()
	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal("owned competing writer could not begin", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "LOCK TABLE public.organizations IN ROW EXCLUSIVE MODE NOWAIT"); err == nil {
		t.Fatal("source cut did not block a raw writer")
	}
	var locked *pgconn.PgError
	if !errors.As(err, &locked) || locked.Code != "55P03" {
		t.Fatal("competing raw writer did not encounter the expected lock", err)
	}
	return cut
}

// A PostgreSQL client can pass the database check before it registers as a
// visible source session. Hold that real startup across the admission commit.
func testDelayRecoverySourceStartup(t *testing.T, ctx context.Context, admin *pgx.Conn, sourceDSN string, oid uint32) <-chan error {
	t.Helper()
	config, err := pgx.ParseConfig(sourceDSN)
	if err != nil {
		t.Fatal("owned startup fixture connection could not be prepared")
	}
	config.RuntimeParams["post_auth_delay"] = "12s"
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	t.Cleanup(cancel)
	closed := make(chan error, 1)
	go func() {
		conn, err := pgx.ConnectConfig(startup, config)
		if err == nil {
			err = conn.Close(startup)
		}
		closed <- err
	}()
	wait, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	for {
		var locked bool
		err = admin.QueryRow(wait, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_locks WHERE locktype='object' AND
			classid='pg_catalog.pg_database'::pg_catalog.regclass AND objid=$1 AND objsubid=0 AND mode='RowExclusiveLock' AND granted)`, oid).Scan(&locked)
		if err != nil {
			t.Fatal("delayed startup never reached the source database lock", err)
		}
		if locked {
			return closed
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func testConfirmRecoverySourceCut(t *testing.T, ctx context.Context, keeper *RecoverySourceCut, admin *pgx.Conn, proxyDSN string) {
	t.Helper()
	p := keeper.Intent()
	for _, mode := range []string{"before_commit", "after_commit"} {
		victim, proxy := independentCommitProxyStore(t, proxyDSN)
		conn, err := victim.Pool.Acquire(ctx)
		if err != nil {
			t.Fatal("cut proof fault proxy unavailable")
		}
		plan := proxy.arm(t, "alter role runverge_admin in database runverge set runverge.recovery_cut_v1", mode)
		err = keeper.Confirm(ctx, conn.Conn())
		conn.Release()
		if !errors.Is(err, ErrRecoverySourceRetirementUnknown) {
			t.Fatal("lost cut proof acknowledgment did not report uncertainty", mode, err)
		}
		proxy.wait(t, plan)
		o, err := InspectRecoverySourceRetirement(ctx, admin, p)
		if err != nil || o.Sessions != 1 || o.State != "admission_disabled_unresolved" || (o.CutProofSHA256 != "") != (mode == "after_commit") || o.ConfirmsRetirement(p) {
			t.Fatal("fresh observation did not distinguish proof commit from remaining session", mode, err)
		}
	}
	keeper.Close()
	if err := keeper.Confirm(ctx, admin); !errors.Is(err, ErrRecoverySourceCut) {
		t.Fatal("closed keeper could record a proof", err)
	}
}

func TestRecoverySourceRetirementAuthorLifecycle(t *testing.T) {
	phase := os.Getenv("RUNVERGE_OWNED_RETIREMENT_PHASE")
	withCut := os.Getenv("RUNVERGE_OWNED_RETIREMENT_CUT") == "1"
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" || (phase != "apply" && phase != "restart") {
		t.Skip("requires the owned two-cluster retirement drill")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adminURI := retirementTestURL(t, "TEST_DATABASE_URL", "runverge_admin")
	targetURI := retirementTestURL(t, "TEST_RETIREMENT_TARGET_URL", "runverge_target_admin")
	appPassword := os.Getenv("TEST_RETIREMENT_APP_PASSWORD")
	if len(appPassword) < 24 || adminURI.Host == targetURI.Host {
		t.Fatal("retirement fixture requires separate primaries and a private test password")
	}
	root := os.Getenv("TEST_RETIREMENT_STATE")
	info, err := os.Lstat(root)
	if err != nil || !filepath.IsAbs(root) || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("private owned retirement state directory required")
	}
	admin, err := pgx.Connect(ctx, adminURI.String())
	if err != nil {
		t.Fatal("source maintenance unavailable")
	}
	defer admin.Close(context.Background())
	appURI := *adminURI
	appURI.User = url.UserPassword("runverge", appPassword)
	assertRejected := func(u url.URL, code string) {
		t.Helper()
		conn, e := pgx.Connect(ctx, u.String())
		if e == nil {
			conn.Close(ctx)
			t.Fatal("old connection unexpectedly admitted")
		}
		var failure *pgconn.PgError
		if !errors.As(e, &failure) || failure.Code != code {
			t.Fatal("old connection did not reach the expected admission check")
		}
	}
	intentPath, resultPath := filepath.Join(root, "retirement-intent.json"), filepath.Join(root, "retirement-observation.json")
	if phase == "restart" {
		var p RecoverySourceRetirementIntent
		var prior RecoverySourceRetirementObservation
		for path, dest := range map[string]any{intentPath: &p, resultPath: &prior} {
			raw, e := os.ReadFile(path)
			if e != nil || json.Unmarshal(raw, dest) != nil {
				t.Fatal("retirement drill lost its exact prior state")
			}
		}
		o, e := InspectRecoverySourceRetirement(ctx, admin, p)
		if e != nil || !o.ConfirmsRetirement(p) || !o.PostmasterChanged || o.ReceiptSHA256 != prior.ReceiptSHA256 || o.CutProofSHA256 != prior.CutProofSHA256 {
			t.Fatal("durable retirement receipt failed after real PostgreSQL restart", e)
		}
		assertRejected(appURI, "28000")
		oldDatabase := *adminURI
		oldDatabase.Path = "/runverge"
		assertRejected(oldDatabase, "55000")
		if withCut {
			if _, e = admin.Exec(ctx, "ALTER ROLE runverge_admin IN DATABASE runverge SET statement_timeout='2s'"); e != nil {
				t.Fatal("fixture cut proof row rewrite failed", e)
			}
			if _, e = InspectRecoverySourceRetirement(ctx, admin, p); !errors.Is(e, ErrRecoverySourceCut) {
				t.Fatal("rewritten cut proof still qualified", e)
			}
		}
		// A DBA transaction that restores both final flags and retains the old
		// setting must not pass as the original retirement transaction.
		if _, e = admin.Exec(ctx, "BEGIN; ALTER ROLE runverge LOGIN; ALTER DATABASE runverge ALLOW_CONNECTIONS true; ALTER DATABASE runverge ALLOW_CONNECTIONS false; ALTER ROLE runverge NOLOGIN; ALTER DATABASE runverge SET statement_timeout='2s'; COMMIT"); e != nil {
			t.Fatal("fixture catalog rewrite failed", e)
		}
		if _, e = InspectRecoverySourceRetirement(ctx, admin, p); !errors.Is(e, ErrRecoverySourceRetirement) {
			t.Fatal("rewritten source adopted its old retirement proof", e)
		}
		return
	}
	var createRole string
	if admin.QueryRow(ctx, "SELECT pg_catalog.format('CREATE ROLE runverge LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',$1::text)", appPassword).Scan(&createRole) != nil {
		t.Fatal("synthetic runtime credential could not be prepared")
	}
	if _, err = admin.Exec(ctx, createRole); err != nil {
		t.Fatal("synthetic runtime role creation failed")
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE runverge OWNER runverge TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		t.Fatal("owned source creation failed", err)
	}
	sourceURI := *adminURI
	sourceURI.Path = "/runverge"
	source, err := Open(ctx, sourceURI.String())
	if err != nil {
		t.Fatal("owned source unavailable", err)
	}
	defer source.Close()
	if err = source.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	limits := DefaultRecoverySnapshotLimits()
	var originalBytes, lateBytes bytes.Buffer
	before, err := source.ExportRecoverySnapshot(ctx, &originalBytes, limits)
	if err != nil {
		t.Fatal(err)
	}
	principal, _, _ := recoveryAuthorPopulate(t, source)
	lateReport, err := source.ExportRecoverySnapshot(ctx, &lateBytes, limits)
	if err != nil {
		t.Fatal(err)
	}
	original := recoveryStageAuthorArchive(t, originalBytes.Bytes())
	late := recoveryStageAuthorArchive(t, lateBytes.Bytes())
	server, err := recoveryFenceServerIdentity(ctx, admin)
	var sourceOID uint32
	if err != nil || admin.QueryRow(ctx, "SELECT oid FROM pg_catalog.pg_database WHERE datname='runverge'").Scan(&sourceOID) != nil {
		t.Fatal("owned source identification failed")
	}
	// Real authenticated archives. The capture metadata simulates the host
	// attestation. Linux Compose has a separate installed source-stop drill.
	l := recoveryarchive.CaptureLink{Version: 2, ExportAlgorithm: recoveryarchive.DrainedCaptureAlgorithm,
		OperationID: strings.TrimPrefix(domain.NewID("cap"), "cap_"), SourceID: before.Metadata.SourceID,
		SelectedReceiptSHA256: recoveryManifestReceipt(original), SelectedSnapshotSHA256: before.SHA256, SelectedCutUTC: before.Metadata.CutUTC,
		BindingSHA256: Hash([]byte("fixture source binding")), StoppedExecutorsSHA256: Hash([]byte("fixture stopped executors")),
		EvidenceReceiptSHA256: recoveryManifestReceipt(late), EvidenceSnapshotSHA256: lateReport.SHA256, EvidenceCutUTC: lateReport.Metadata.CutUTC,
		CapturedAt: time.Now().UTC(), SystemID: server.SystemID, DatabaseOID: sourceOID, ExportWireBytes: lateReport.WireBytes,
		EvidenceSchemaSHA256: lateReport.Metadata.SchemaSHA256, EvidenceBackupID: lateReport.Metadata.BackupID, ExecutorsStoppedAtUTC: lateReport.Metadata.CutUTC}
	keys, linkPath := filepath.Join(root, "archive-keys.json"), filepath.Join(root, "capture.json")
	if _, err = recoveryarchive.GenerateHostKeys(keys); err != nil {
		t.Fatal(err)
	}
	if err = recoveryarchive.WriteHostCaptureLink(linkPath, keys, l); err != nil {
		t.Fatal(err)
	}
	link, err := recoveryarchive.VerifyHostCaptureLink(linkPath, keys)
	if err != nil {
		t.Fatal(err)
	}
	destinationAdmin, err := pgx.Connect(ctx, targetURI.String())
	if err != nil {
		t.Fatal("destination maintenance unavailable")
	}
	defer destinationAdmin.Close(ctx)
	destination, err := PlanRecoveryRetirementDestination(ctx, destinationAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckRecoveryRetirementDestination(ctx, destinationAdmin, destination); err != nil {
		t.Fatal("unused reserved target could not be checked", err)
	}
	if err = CheckRecoveryRetirementDestination(ctx, admin, destination); !errors.Is(err, ErrRecoverySourceRetirement) {
		t.Fatal("foreign primary satisfied destination readiness", err)
	}
	p, err := PlanRecoverySourceRetirement(ctx, admin, source, link, destination)
	if err != nil || !p.MatchesCapture(link, destination) {
		t.Fatal("exact source could not reserve the separate target", err)
	}
	retire := func(maintenance *pgx.Conn) (RecoverySourceRetirementObservation, error) {
		return ApplyRecoverySourceRetirement(ctx, maintenance, p)
	}
	var keeper *RecoverySourceCut
	if withCut {
		keeper = testPrepareRecoverySourceCut(t, ctx, admin, source, original, late, link, destination, limits, principal.OrganizationID)
		defer keeper.Close()
		p = keeper.Intent()
		if p.Version != 2 || p.BaselineSHA256 != lateReport.SHA256 || !p.MatchesCapture(link, destination) {
			t.Fatal("verified keeper did not bind the exact archive")
		}
		if _, err = ApplyRecoverySourceRetirement(ctx, admin, p); !errors.Is(err, ErrRecoverySourceCut) {
			t.Fatal("V2 intent could be applied without its original keeper", err)
		}
		retire = func(maintenance *pgx.Conn) (RecoverySourceRetirementObservation, error) {
			return keeper.Retire(ctx, maintenance)
		}
	}
	if err = recoveryarchive.WriteHostJSON(intentPath, p); err != nil {
		t.Fatal(err)
	}
	source.Close()
	old, err := pgx.Connect(ctx, appURI.String())
	if err != nil {
		t.Fatal("fixture app role could not log into maintenance before retirement")
	}
	defer old.Close(ctx)
	if _, err = retire(admin); !errors.Is(err, ErrRecoverySourceRetirement) {
		t.Fatal("retirement ignored an active app session outside the old database", err)
	}
	old.Close(ctx)
	prepared, err := pgx.Connect(ctx, sourceURI.String())
	if err != nil {
		t.Fatal("owned source unavailable for prepared fixture")
	}
	defer prepared.Close(ctx)
	if _, err = prepared.Exec(ctx, "BEGIN; SELECT 1; PREPARE TRANSACTION 'rv_retirement_owned_prepared'"); err != nil {
		t.Fatal("owned prepared fixture failed", err)
	}
	prepared.Close(ctx)
	if _, err = retire(admin); !errors.Is(err, ErrRecoverySourceRetirement) {
		t.Fatal("retirement ignored a prepared transaction", err)
	}
	prepared, err = pgx.Connect(ctx, sourceURI.String())
	if err != nil {
		t.Fatal("failed apply changed the old database admission")
	}
	defer prepared.Close(ctx)
	if _, err = prepared.Exec(ctx, "ROLLBACK PREPARED 'rv_retirement_owned_prepared'"); err != nil {
		t.Fatal("owned prepared cleanup failed", err)
	}
	prepared.Close(ctx)
	proxyURI := *adminURI
	schema := domain.NewID("test")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	q := proxyURI.Query()
	q.Set("search_path", schema)
	proxyURI.RawQuery = q.Encode()
	var observed RecoverySourceRetirementObservation
	var starting <-chan error
	for _, mode := range []string{"before_commit", "after_commit"} {
		victim, proxy := independentCommitProxyStore(t, proxyURI.String())
		conn, e := victim.Pool.Acquire(ctx)
		if e != nil {
			t.Fatal("owned fault proxy unavailable")
		}
		cut := proxy.arm(t, "alter role runverge nologin", mode)
		if withCut && mode == "after_commit" {
			starting = testDelayRecoverySourceStartup(t, ctx, admin, sourceURI.String(), p.SourceOID)
		}
		got, e := retire(conn.Conn())
		conn.Release()
		if !errors.Is(e, ErrRecoverySourceRetirementUnknown) || !reflect.DeepEqual(got, RecoverySourceRetirementObservation{}) {
			t.Fatal("lost retirement acknowledgment did not fail closed", mode, e)
		}
		proxy.wait(t, cut)
		observed, e = InspectRecoverySourceRetirement(ctx, admin, p)
		open := mode == "before_commit"
		if e != nil || observed.SourceConnections != open || observed.ApplicationLogin != open || observed.MaintenanceOwnsDB == open || (observed.ReceiptSHA256 == "") != open {
			t.Fatal("fresh inspection disagrees with atomic role/database/receipt commit", mode, e)
		}
		if withCut && (observed.CutProofSHA256 != "" || observed.ConfirmsRetirement(p)) {
			t.Fatal("admission receipt was mistaken for completed source data proof", mode)
		}
		if withCut && open && !errors.Is(keeper.Confirm(ctx, admin), ErrRecoverySourceCut) {
			t.Fatal("cut proof could be written before a committed ban")
		}
	}
	if withCut {
		if err = keeper.Confirm(ctx, admin); !errors.Is(err, ErrRecoverySourceCut) {
			t.Fatal("cut proof was recorded before the old startup drained", err)
		}
		select {
		case err = <-starting:
			if err != nil {
				t.Fatal("delayed startup did not pass admission before the ban")
			}
		case <-ctx.Done():
			t.Fatal("delayed startup did not finish")
		}
		testConfirmRecoverySourceCut(t, ctx, keeper, admin, proxyURI.String())
	}
	observe, cancelObserve := context.WithTimeout(ctx, 3*time.Second)
	defer cancelObserve()
	for {
		observed, err = InspectRecoverySourceRetirement(observe, admin, p)
		if err != nil || observed.Sessions == 0 {
			break
		}
		select {
		case <-observe.Done():
		case <-time.After(30 * time.Millisecond):
		}
	}
	if err != nil || !observed.ConfirmsRetirement(p) {
		t.Fatal("retirement was not fully observed")
	}
	if err = recoveryarchive.WriteHostJSON(resultPath, observed); err != nil {
		t.Fatal(err)
	}
	assertRejected(appURI, "28000")
	assertRejected(sourceURI, "55000")
	// Exercise a command that already has the application's role, without
	// relying on NOLOGIN to prevent that particular connection from existing.
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SET LOCAL ROLE runverge"); err != nil {
		t.Fatal("fixture could not exercise the old application authority", err)
	}
	var denied *pgconn.PgError
	if withCut {
		if _, err = tx.Exec(ctx, "SAVEPOINT cut_proof_privilege"); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, "ALTER ROLE runverge_admin IN DATABASE runverge SET runverge.recovery_cut_v1='forged'")
		if !errors.As(err, &denied) || denied.Code != "42501" {
			t.Fatal("old application authority could change the cut proof")
		}
		if _, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT cut_proof_privilege"); err != nil {
			t.Fatal(err)
		}
		denied = nil
	}
	_, err = tx.Exec(ctx, "ALTER DATABASE runverge ALLOW_CONNECTIONS true")
	if !errors.As(err, &denied) || denied.Code != "42501" {
		t.Fatal("old application authority retained database ownership")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wrong := p
	wrong.Destination.RestoreID = domain.NewID("restore")
	if _, err = InspectRecoverySourceRetirement(ctx, admin, wrong); !errors.Is(err, ErrRecoverySourceRetirement) {
		t.Fatal("different restore slot adopted the retirement", err)
	}
	if _, err = retire(admin); !errors.Is(err, ErrRecoverySourceRetirement) && !errors.Is(err, ErrRecoverySourceCut) {
		t.Fatal("committed retirement repeated", err)
	}
	provenance := Hash([]byte("owned retired handoff provenance"))
	plan := func(c *pgx.Conn) (RecoveryProvisionIntent, error) {
		return PlanRecoveryProvisionAtRetiredDestination(ctx, c, l.EvidenceBackupID, l.SourceID, l.EvidenceSnapshotSHA256, l.EvidenceSchemaSHA256, provenance, destination)
	}
	if _, err = plan(admin); !errors.Is(err, ErrRecoveryProvision) {
		t.Fatal("the old primary adopted the reserved destination", err)
	}
	first, err := plan(destinationAdmin)
	if err != nil || first.OperationID != destination.RestoreID || first.WALTimeline != destination.WALTimeline {
		t.Fatal("exact destination slot not selected", err)
	}
	second, err := plan(destinationAdmin)
	if err != nil || first.Database != second.Database {
		t.Fatal("a second plan minted a second database name", err)
	}
	created, err := CreateRecoveryProvision(ctx, destinationAdmin, first)
	if err != nil {
		t.Fatal("reserved target provisioning failed", err)
	}
	if err = CheckRecoveryRetirementDestination(ctx, destinationAdmin, destination); !errors.Is(err, ErrRecoverySourceRetirement) {
		t.Fatal("occupied target slot reported as unused", err)
	}
	if _, err = CreateRecoveryProvision(ctx, destinationAdmin, second); !errors.Is(err, ErrRecoveryProvision) {
		t.Fatal("second plan could create or adopt the reserved target", err)
	}
	actual := *targetURI
	actual.Path = "/" + first.Database
	target, err := OpenRecoveryProvisionTarget(ctx, destinationAdmin, actual.String(), first, created.DatabaseOID)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err = target.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := target.InspectRecoveryTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec := RecoveryTargetSpec{TargetInstallationID: state.InstallationID, SourceID: l.SourceID, RestoreID: first.OperationID, BackupID: l.EvidenceBackupID, CutUTC: l.EvidenceCutUTC, SchemaSHA256: l.EvidenceSchemaSHA256}
	if _, err = target.PrepareRecoveryTarget(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err = target.ImportRecoverySnapshot(ctx, late, spec, limits); err != nil || !errors.Is(target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("reserved destination did not import under the execution hold", err)
	}
}

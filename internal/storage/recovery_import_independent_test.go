// SPDX-License-Identifier: Elastic-2.0
package storage

// These tests require the explicit independent owned-cluster opt-in. They use
// fresh schemas and signed synthetic archives; they never consume archive SQL,
// contact a provider, require superuser, or exercise a production installation.
// The trusted control fault below is ONLY a negative-test device. It is not a
// supported hold-clearance, credential-recovery or authority-reactivation flow.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	oidcidentity "agentplatform.local/platform/internal/identity"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type independentImportedFixture struct {
	source, target *Store
	targetDSN      string
	principal      domain.Principal
	bootstrap      Bootstrap
	agent          domain.Agent
	completedLease *domain.Lease
	liveLease      *domain.Lease
	expiredLease   *domain.Lease
	reservations   []domain.Reservation
	unknownKeys    []string
	session        oidcidentity.BrowserSession
	service        domain.ServiceAccount
	serviceToken   string
	cachedTaskKey  string
	archive        *recoveryarchive.VerifiedArchive
	snapshot       RecoverySnapshotReport
	inventory      RecoverySnapshotInventory
	spec           RecoveryTargetSpec
	held           RecoveryTargetState
	receipt        RecoveryImportReceipt
	rows           map[string][]string
}

func independentImportFixture(t *testing.T) *independentImportedFixture {
	t.Helper()
	f := independentImportPreparedFixture(t)
	var err error
	f.receipt, err = f.target.ImportRecoverySnapshot(context.Background(), f.archive, f.spec, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("authenticated populated held import failed", err)
	}
	return f
}

func independentImportPreparedFixture(t *testing.T) *independentImportedFixture {
	t.Helper()
	ctx := context.Background()
	f := &independentImportedFixture{}
	f.source, _ = independentRecoveryStore(t)
	f.principal, f.bootstrap = identity(t, f.source, 1_000_000)
	f.agent = agent(t, f.source, f.principal)
	_ = task(t, f.source, f.principal, f.agent)
	completed, err := f.source.ClaimFixture(ctx, "independent-import-completed", 5*time.Minute)
	if err != nil || completed == nil {
		t.Fatal("synthetic source completion lease unavailable", err)
	}
	if err = f.source.CompleteFixture(ctx, completed, "Тестовый результат\n"+domain.ExecutionInput(*completed)); err != nil {
		t.Fatal("synthetic source completion failed", err)
	}
	f.completedLease = completed
	_ = task(t, f.source, f.principal, f.agent)
	f.liveLease, err = f.source.ClaimFixture(ctx, "independent-import-live", 5*time.Minute)
	if err != nil || f.liveLease == nil {
		t.Fatal("live source lease unavailable", err)
	}
	for _, amount := range []int64{0, 37} {
		r, e := f.source.ReserveCost(ctx, f.liveLease, domain.ReserveCostRequest{
			ChargeKey: domain.NewID("charge"), Category: "compute", Provider: "independent-import-fixture",
			PriceVersion: "fixture-v1", AmountMicroUSD: amount, IntentHash: Hash([]byte("synthetic stored cost; no external request")),
		})
		if e != nil {
			t.Fatal("source reservation failed", e)
		}
		unknownKey := domain.NewID("unknown")
		r, e = f.source.MarkCostUnknown(ctx, f.principal.OrganizationID, r.ID, unknownKey, "Synthetic unresolved cost retained for import")
		if e != nil || r.Status != "unknown" || r.AmountMicroUSD != amount || r.ActionID == "" {
			t.Fatal("source must include real zero/nonzero unknown reservations", e)
		}
		f.reservations = append(f.reservations, r)
		f.unknownKeys = append(f.unknownKeys, unknownKey)
	}
	if err = f.source.CheckLease(ctx, f.liveLease); err != nil {
		t.Fatal("source saved lease must be live before export", err)
	}
	_ = task(t, f.source, f.principal, f.agent)
	f.expiredLease, err = f.source.ClaimFixture(ctx, "independent-import-expired", 5*time.Minute)
	if err != nil || f.expiredLease == nil {
		t.Fatal("second source lease unavailable", err)
	}
	// Make an OLD operation genuinely selector-eligible; otherwise an unexpired
	// lease alone would not prove that the selector excludes historical work.
	if _, err = f.source.Pool.Exec(ctx, "UPDATE operations SET lease_until=clock_timestamp()-interval '1 second' WHERE organization_id=$1 AND id=$2", f.principal.OrganizationID, f.expiredLease.OperationID); err != nil {
		t.Fatal("synthetic expired source lease setup failed", err)
	}
	_ = task(t, f.source, f.principal, f.agent) // older queued work must not starve NEW work
	if err = f.source.Pool.QueryRow(ctx, "SELECT key FROM idempotency_keys WHERE organization_id=$1 AND principal_id=$2 AND scope='POST /tasks' LIMIT 1", f.principal.OrganizationID, f.principal.ID).Scan(&f.cachedTaskKey); err != nil {
		t.Fatal("source cached successful command is required", err)
	}
	f.session = independentSession(t, f.source, f.principal, domain.NewID("subject"), time.Hour)
	f.service = independentSecurityService(t, f.source, f.principal, "member")
	_, f.serviceToken, _ = independentSecurityKey(t, f.source, f.service, "member", time.Hour)
	if _, err = f.source.AuthenticateBrowserSession(ctx, f.session.TokenHash, f.principal.OrganizationID); err != nil {
		t.Fatal("source browser credential must be valid before export", err)
	}
	var raw bytes.Buffer
	f.snapshot, err = f.source.ExportRecoverySnapshot(ctx, &raw, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("independent populated export failed", err)
	}
	var header recoverySnapshotHeader
	if err = json.Unmarshal(bytes.SplitN(raw.Bytes(), []byte("\n"), 2)[0], &header); err != nil {
		t.Fatal("synthetic inventory extraction failed")
	}
	f.inventory = header.Inventory
	f.rows = independentImportRows(t, f.source, f.inventory, false)
	f.archive = recoveryStageAuthorArchive(t, raw.Bytes())
	f.target, f.targetDSN = independentRecoveryStore(t)
	f.spec = independentRecoverySpec(t, f.target)
	m := f.snapshot.Metadata
	f.spec.BackupID, f.spec.SourceID, f.spec.CutUTC, f.spec.SchemaSHA256 = m.BackupID, m.SourceID, m.CutUTC, m.SchemaSHA256
	f.held, err = f.target.PrepareRecoveryTarget(ctx, f.spec)
	if err != nil {
		t.Fatal("independent target hold setup failed", err)
	}
	inspection, err := f.target.InspectRecoveryImport(ctx, f.spec, f.snapshot.SHA256)
	if err != nil || inspection.State != "empty_held" || inspection.Receipt != nil {
		t.Fatal("empty held target inspection failed", err)
	}
	return f
}

// Observe every physical column without production import/staging digest code.
// Sorting canonical JSON text makes this independent of source/target row order;
// row bodies (which can contain synthetic credentials) are never printed.
func independentImportRows(t *testing.T, s *Store, inventory RecoverySnapshotInventory, historicalOnly bool) map[string][]string {
	t.Helper()
	result := map[string][]string{}
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		query := "SELECT pg_catalog.to_jsonb(r)::text AS value FROM " + pgx.Identifier{s.runtimeSchema, table.Name}.Sanitize() + " r"
		if historicalOnly {
			query += " WHERE EXISTS(SELECT 1 FROM " + pgx.Identifier{s.runtimeSchema, "recovery_historical_rows"}.Sanitize() + " h WHERE h.table_name=$1 AND h.pk_sha256=" + independentImportKeySQL(table) + ")"
		}
		query += ` ORDER BY pg_catalog.to_jsonb(r)::text COLLATE "C"`
		var args []any
		if historicalOnly {
			args = []any{table.Name}
		}
		rows, err := s.Pool.Query(context.Background(), query, args...)
		if err != nil {
			t.Fatal("physical row observation failed for", table.Name, err)
		}
		values := []string{}
		for rows.Next() {
			var value string
			if err = rows.Scan(&value); err != nil {
				rows.Close()
				t.Fatal("physical row scan failed for", table.Name)
			}
			values = append(values, value)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal("physical row enumeration failed for", table.Name)
		}
		result[table.Name] = values
	}
	return result
}

// Deliberately inline the specified target-bound physical-PK encoding rather
// than calling recovery_row_key/recovery_is_historical under test.
func independentImportKeySQL(table RecoverySnapshotTable) string {
	keys := make([]string, len(table.PrimaryKey))
	for i, key := range table.PrimaryKey {
		keys[i] = "r." + pgx.Identifier{key}.Sanitize()
	}
	return "(SELECT pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(pg_catalog.jsonb_build_array('recovery-row-key-v1',receipt.target_installation_id,receipt.restore_id,$1::text,pg_catalog.jsonb_build_array(" + strings.Join(keys, ",") + "))::text,'UTF8')),'hex') FROM recovery_import_receipts receipt WHERE receipt.singleton)"
}

func independentImportAssertRows(t *testing.T, want, got map[string][]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatal("physical exported table inventory changed")
	}
	for name, rows := range want {
		if !reflect.DeepEqual(rows, got[name]) {
			t.Fatal("physical source rows changed for", name)
		}
	}
}

func independentImportAssertMarkers(t *testing.T, f *independentImportedFixture) {
	t.Helper()
	ctx := context.Background()
	var expectedTotal int64
	for _, table := range f.inventory.Tables {
		if !table.Exported {
			continue
		}
		// Both EXCEPT directions catch same-count missing/replaced markers, not
		// merely a count mismatch. Restore binding is checked independently too.
		query := "WITH expected AS (SELECT " + independentImportKeySQL(table) + " AS key FROM " + pgx.Identifier{f.target.runtimeSchema, table.Name}.Sanitize() + " r), actual AS (SELECT pk_sha256 AS key FROM recovery_historical_rows WHERE table_name=$1) SELECT (SELECT count(*) FROM expected), (SELECT count(*) FROM ((SELECT key FROM expected EXCEPT SELECT key FROM actual) UNION ALL (SELECT key FROM actual EXCEPT SELECT key FROM expected)) difference), (SELECT count(*) FROM recovery_historical_rows WHERE table_name=$1 AND restore_id<>$2)"
		var count, difference, wrongRestore int64
		if err := f.target.Pool.QueryRow(ctx, query, table.Name, f.spec.RestoreID).Scan(&count, &difference, &wrongRestore); err != nil {
			t.Fatal("marker identity observation failed for", table.Name, err)
		}
		if count != int64(len(f.rows[table.Name])) || difference != 0 || wrongRestore != 0 {
			t.Fatal("historical marker identities are incomplete for", table.Name)
		}
		var guardsEnabled bool
		if err := f.target.Pool.QueryRow(ctx, "SELECT count(*)=2 AND bool_and(tgenabled='O') FROM pg_trigger WHERE tgrelid=$1::regclass AND tgname IN('000_recovery_history_row','000_recovery_history_truncate')", pgx.Identifier{f.target.runtimeSchema, table.Name}.Sanitize()).Scan(&guardsEnabled); err != nil || !guardsEnabled {
			t.Fatal("historical relation guards not restored for", table.Name, err)
		}
		expectedTotal += count
	}
	var actualTotal int64
	if err := f.target.Pool.QueryRow(ctx, "SELECT count(*) FROM recovery_historical_rows").Scan(&actualTotal); err != nil || actualTotal != expectedTotal {
		t.Fatal("unexpected historical marker table or total", err)
	}
}

func TestRecoveryImportIndependentPopulatedExactHeldAndReopen(t *testing.T) {
	f := independentImportFixture(t)
	ctx := context.Background()
	r := f.receipt
	if r.Profile != "pg18-held-import-v1" || r.TargetInstallationID != f.spec.TargetInstallationID || r.RestoreID != f.spec.RestoreID || r.BackupID != f.spec.BackupID || r.SourceID != f.spec.SourceID || r.CutUTC != f.spec.CutUTC || r.SchemaSHA256 != f.spec.SchemaSHA256 || r.SnapshotSHA256 != f.snapshot.SHA256 || r.InventorySHA256 != f.snapshot.InventorySHA256 || r.ImportedAt.IsZero() || len(r.CatalogSHA256) != 64 || !reflect.DeepEqual(r.Tables, f.snapshot.Tables) || !reflect.DeepEqual(r.Sequences, f.snapshot.Sequences) {
		t.Fatal("committed receipt does not bind exact authenticated import")
	}
	independentImportAssertRows(t, f.rows, independentImportRows(t, f.target, f.inventory, false))
	independentImportAssertMarkers(t, f)
	for _, seq := range r.Sequences {
		var last int64
		var called bool
		if err := f.target.Pool.QueryRow(ctx, "SELECT last_value,is_called FROM "+pgx.Identifier{f.target.runtimeSchema, seq.Name}.Sanitize()).Scan(&last, &called); err != nil || last != seq.NextMinimum || called {
			t.Fatal("import sequence is not at exact unused next minimum", err)
		}
	}
	state, err := f.target.InspectRecoveryTarget(ctx)
	if err != nil || state != f.held || !errors.Is(f.target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("committed import must preserve original hold", err)
	}
	if _, err = f.target.Authenticate(ctx, f.bootstrap.Token, f.principal.OrganizationID); !errors.Is(err, RecoveryHeld) {
		t.Fatal("held imported credential was admitted")
	}
	f.target.Close()
	reopened, err := Open(ctx, f.targetDSN)
	if err != nil {
		t.Fatal("target reopen failed", err)
	}
	t.Cleanup(reopened.Close)
	f.target = reopened
	inspection, err := reopened.InspectRecoveryImport(ctx, f.spec, f.snapshot.SHA256)
	if err != nil || inspection.State != "committed_held" || inspection.Receipt == nil || !reflect.DeepEqual(*inspection.Receipt, r) {
		t.Fatal("fresh reopen inspection lost exact committed receipt", err)
	}
	for _, field := range []string{"target", "restore", "backup", "source", "cut", "schema", "snapshot"} {
		t.Run("wrong_"+field, func(t *testing.T) {
			wrong, digest := f.spec, f.snapshot.SHA256
			switch field {
			case "target":
				wrong.TargetInstallationID = "inst_" + strings.Repeat("d", 32)
			case "restore":
				wrong.RestoreID = "restore_" + strings.Repeat("d", 32)
			case "backup":
				wrong.BackupID = "bak_" + strings.Repeat("d", 32)
			case "source":
				wrong.SourceID = "inst_" + strings.Repeat("d", 32)
			case "cut":
				wrong.CutUTC = "2026-10-28T12:34:56Z"
			case "schema":
				wrong.SchemaSHA256 = strings.Repeat("d", 64)
			case "snapshot":
				digest = strings.Repeat("d", 64)
			}
			if _, e := reopened.InspectRecoveryImport(ctx, wrong, digest); e == nil {
				t.Fatal("inspection accepted a different exact recovery tuple")
			}
		})
	}
	if _, err = reopened.ImportRecoverySnapshot(ctx, f.archive, f.spec, DefaultRecoverySnapshotLimits()); err == nil || errors.Is(err, ErrRecoveryImportOutcomeUnknown) {
		t.Fatal("second populated import must be refused before any commit uncertainty")
	}
	independentImportAssertRows(t, f.rows, independentImportRows(t, reopened, f.inventory, false))
	independentImportAssertMarkers(t, f)
	inspection, err = reopened.InspectRecoveryImport(ctx, f.spec, f.snapshot.SHA256)
	if err != nil || inspection.Receipt == nil || !reflect.DeepEqual(*inspection.Receipt, r) || !errors.Is(reopened.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("refused import changed committed receipt or hold", err)
	}
}

// This deliberately bypasses ONLY the control-transition row guard in our
// disposable target. All historical guards remain enabled. Restore the control
// guard before committing, then prove global admission is normal before testing
// independent historical fences. No production clearance protocol is inferred.
func independentImportFaultControl(t *testing.T, f *independentImportedFixture, held bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal("trusted test control transaction failed")
	}
	defer tx.Rollback(ctx)
	q := pgx.Identifier{f.target.runtimeSchema, "runtime_recovery_control"}.Sanitize()
	if _, err = tx.Exec(ctx, "ALTER TABLE "+q+" DISABLE TRIGGER recovery_control_change"); err != nil {
		t.Fatal("trusted control fault guard suspension failed", err)
	}
	if held {
		_, err = tx.Exec(ctx, "UPDATE "+q+" SET state='held',restore_id=$1,backup_id=$2,source_id=$3,cut_at=$4,schema_sha256=$5,held_at=$6 WHERE singleton", f.spec.RestoreID, f.spec.BackupID, f.spec.SourceID, f.heldCut(t), f.spec.SchemaSHA256, f.held.HeldAt)
	} else {
		_, err = tx.Exec(ctx, "UPDATE "+q+" SET state='normal',restore_id=NULL,backup_id=NULL,source_id=NULL,cut_at=NULL,schema_sha256=NULL,held_at=NULL WHERE singleton")
	}
	if err != nil {
		t.Fatal("trusted test control update failed", err)
	}
	if _, err = tx.Exec(ctx, "ALTER TABLE "+q+" ENABLE TRIGGER recovery_control_change"); err != nil {
		t.Fatal("trusted test control guard restoration failed", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("trusted control test transaction failed", err)
	}
	var enabled bool
	if err = f.target.Pool.QueryRow(ctx, "SELECT count(*)=2 AND bool_and(tgenabled='O') FROM pg_trigger WHERE tgrelid=$1::regclass AND tgname IN('recovery_control_change','recovery_control_statement')", q).Scan(&enabled); err != nil || !enabled {
		t.Fatal("control protection was not restored before negative probes", err)
	}
}

func (f *independentImportedFixture) heldCut(t *testing.T) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, f.spec.CutUTC)
	if err != nil {
		t.Fatal("invalid synthetic cut")
	}
	return v
}

func independentImportFence(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, RecoveryHeld) {
		return
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "55000" {
		return
	}
	// A stale/expired lease, invalid request, idempotent replay, or unrelated
	// permission failure is not proof that the historical admission fence ran.
	t.Fatal("historical mutation lacked the specific recovery fence")
}

func TestRecoveryImportIndependentHistoricalFencesAndFreshProgress(t *testing.T) {
	f := independentImportFixture(t)
	ctx := context.Background()
	independentImportFaultControl(t, f, false)
	t.Cleanup(func() { independentImportFaultControl(t, f, true) })
	if err := f.target.CheckRuntimeAdmission(ctx); err != nil {
		t.Fatal("global hold still masks historical fence probes", err)
	}
	var liveUnexpired, expiredEligible bool
	if err := f.target.Pool.QueryRow(ctx, "SELECT lease_until>clock_timestamp() FROM operations WHERE organization_id=$1 AND id=$2", f.principal.OrganizationID, f.liveLease.OperationID).Scan(&liveUnexpired); err != nil || !liveUnexpired {
		t.Fatal("saved live lease expired before the independent fence probes", err)
	}
	if err := f.target.Pool.QueryRow(ctx, "SELECT status='leased' AND lease_until<clock_timestamp() FROM operations WHERE organization_id=$1 AND id=$2", f.principal.OrganizationID, f.expiredLease.OperationID).Scan(&expiredEligible); err != nil || !expiredEligible {
		t.Fatal("historical expired operation is not genuinely selector eligible", err)
	}
	if _, err := f.target.Authenticate(ctx, f.bootstrap.Token, f.principal.OrganizationID); !errors.Is(err, domain.Unauthenticated) {
		t.Fatal("historical human bearer regained authority")
	}
	if _, err := f.target.Authenticate(ctx, f.serviceToken, f.principal.OrganizationID); !errors.Is(err, domain.Unauthenticated) {
		t.Fatal("historical service credential regained authority")
	}
	if _, err := f.target.AuthenticateBrowserSession(ctx, f.session.TokenHash, f.principal.OrganizationID); !errors.Is(err, domain.Unauthenticated) {
		t.Fatal("historical browser credential regained authority")
	}
	if _, err := f.target.GetSession(ctx, f.session.TokenHash); !errors.Is(err, oidcidentity.ErrSessionInvalid) {
		t.Fatal("historical browser session remained reusable")
	}
	if _, err := f.target.ResolveIdentity(ctx, f.session.Issuer, f.session.Subject); !errors.Is(err, oidcidentity.ErrIdentityUnmapped) {
		t.Fatal("historical identity mapping reactivated its principal")
	}
	if _, err := f.target.CreateTask(ctx, f.principal, f.cachedTaskKey, domain.CreateTask{Goal: "A bounded test", AgentID: f.agent.ID, BudgetMicroUSD: 10_000}); !errors.Is(err, domain.Unauthenticated) {
		t.Fatal("cached successful command bypassed historical authority exclusion")
	}
	replacement := f.session
	replacement.ID = domain.NewID("ses")
	replacement.TokenHash = Hash([]byte(domain.NewID("new-browser-secret")))
	replacement.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	if err := f.target.CreateSession(ctx, replacement, ""); err == nil {
		t.Fatal("historical identity minted a replacement browser session")
	}
	if err := f.target.RotateSession(ctx, f.session.TokenHash, replacement); err == nil {
		t.Fatal("historical session rotation regained authority")
	}
	if _, _, err := f.target.IssueMemberToken(ctx, f.principal.OrganizationID, f.principal.ID, time.Hour); err == nil {
		t.Fatal("trusted ordinary issuance reactivated historical human")
	}
	if _, _, err := f.target.IssueServiceToken(ctx, f.principal.OrganizationID, f.service.PrincipalID, "member", time.Hour, "Synthetic historical issuance rejection"); err == nil {
		t.Fatal("trusted ordinary issuance reactivated historical service")
	}
	independentImportFence(t, f.target.CheckLease(ctx, f.liveLease))
	_, err := f.target.ReserveCost(ctx, f.liveLease, domain.ReserveCostRequest{ChargeKey: domain.NewID("charge"), Category: "compute", Provider: "independent-import-fixture", PriceVersion: "fixture-v1", AmountMicroUSD: 0, IntentHash: Hash([]byte("new zero cost against historical lease"))})
	independentImportFence(t, err)
	independentImportFence(t, f.target.PrepareCostDispatch(ctx, f.liveLease, f.reservations[0].ID))
	independentImportFence(t, f.target.CompleteFixture(ctx, f.liveLease, "Тестовый результат\n"+domain.ExecutionInput(*f.liveLease)))
	independentImportFence(t, f.target.CheckLease(ctx, f.completedLease))
	independentImportFence(t, f.target.CompleteFixture(ctx, f.completedLease, "Тестовый результат\n"+domain.ExecutionInput(*f.completedLease)))
	_, err = f.target.MarkCostUnknown(ctx, f.principal.OrganizationID, f.completedLease.ReservationID, domain.NewID("terminal"), "Terminal historical reservation remains immutable")
	independentImportFence(t, err)
	_, err = f.target.ReleaseUnsentCost(ctx, f.principal.OrganizationID, f.completedLease.ReservationID, domain.NewID("terminal"))
	independentImportFence(t, err)
	for i, r := range f.reservations {
		// Even an exact former successful accounting command must pass the
		// historical gate before returning its cached/no-write replay branch.
		_, err = f.target.MarkCostUnknown(ctx, r.OrganizationID, r.ID, f.unknownKeys[i], "Synthetic unresolved cost retained for import")
		independentImportFence(t, err)
		// NEW keys intentionally bypass ledger replay: a replay with no write
		// would not establish protection of the old financial facts.
		_, err = f.target.MarkCostUnknown(ctx, r.OrganizationID, r.ID, domain.NewID("unknown"), "Historical unknown must remain unchanged")
		independentImportFence(t, err)
		_, err = f.target.ReleaseUnsentCost(ctx, r.OrganizationID, r.ID, domain.NewID("release"))
		independentImportFence(t, err)
		_, err = f.target.AccountCost(ctx, r.OrganizationID, r.ID, domain.CostEvidence{Source: r.Provider, Reference: domain.NewID("fact"), PayloadDigest: Hash([]byte("synthetic zero final cost")), ActualMicroUSD: 0, Reason: "Historical financial evidence is immutable", Final: true, Currency: "USD"})
		independentImportFence(t, err)
	}
	for name, query := range map[string]string{
		"old_task_noop":     "UPDATE tasks SET status=status WHERE organization_id=$1 AND id=$2",
		"old_task_pk":       "UPDATE tasks SET id='tsk_independent_replacement' WHERE organization_id=$1 AND id=$2",
		"fresh_op_old_task": "INSERT INTO operations(organization_id,id,task_id,run_id,status) SELECT organization_id,'op_independent_new',task_id,run_id,'queued' FROM operations WHERE organization_id=$1 AND task_id=$2 LIMIT 1",
	} {
		t.Run(name, func(t *testing.T) {
			_, e := f.target.Pool.Exec(ctx, query, f.principal.OrganizationID, f.liveLease.TaskID)
			independentRecoverySQLState(t, e, "55000")
		})
	}
	for name, query := range map[string]string{
		"delete_zero_unknown": "DELETE FROM reservations WHERE organization_id=$1 AND id=$2",
		"new_zero_ledger":     "INSERT INTO ledger_entries(organization_id,id,reservation_id,task_id,root_id,kind,delta_reserved_microusd,delta_spent_microusd,event_key,event_hash) SELECT organization_id,'led_independent_new',id,task_id,root_id,'unknown',0,0,'independent-new-zero-ledger',repeat('a',64) FROM reservations WHERE organization_id=$1 AND id=$2",
	} {
		t.Run(name, func(t *testing.T) {
			_, e := f.target.Pool.Exec(ctx, query, f.principal.OrganizationID, f.reservations[0].ID)
			independentRecoverySQLState(t, e, "55000")
		})
	}
	_, err = f.target.Pool.Exec(ctx, "INSERT INTO access_tokens(token_hash,principal_id) VALUES($1,$2)", Hash([]byte(domain.NewID("forbidden-secret"))), f.principal.ID)
	independentRecoverySQLState(t, err, "55000")
	for _, query := range []string{
		"TRUNCATE reservations CASCADE",
		"UPDATE recovery_import_receipts SET imported_at=imported_at",
		"DELETE FROM recovery_import_receipts",
		"TRUNCATE recovery_import_receipts CASCADE",
		"UPDATE recovery_historical_rows SET pk_sha256=pk_sha256",
		"DELETE FROM recovery_historical_rows",
		"TRUNCATE recovery_historical_rows",
	} {
		_, err = f.target.Pool.Exec(ctx, query)
		independentRecoverySQLState(t, err, "55000")
	}
	independentImportAssertRows(t, f.rows, independentImportRows(t, f.target, f.inventory, false))
	independentImportAssertMarkers(t, f)
	// Provision a genuinely NEW test identity in the same organization using
	// trusted fixture SQL, not an old recovered token. Historical principals are
	// never re-enabled. This is deliberately not a product credential-recovery
	// flow; it isolates selector progress and permitted shared aggregate writes.
	freshID := domain.NewID("usr")
	if _, err = f.target.Pool.Exec(ctx, "INSERT INTO principals(id,name) VALUES($1,'Fresh independent test identity')", freshID); err != nil {
		t.Fatal("fresh principal fixture failed", err)
	}
	if _, err = f.target.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role) VALUES($1,$2,'owner')", f.principal.OrganizationID, freshID); err != nil {
		t.Fatal("fresh membership fixture failed", err)
	}
	freshToken, _, err := f.target.IssueMemberToken(ctx, f.principal.OrganizationID, freshID, time.Hour)
	if err != nil {
		t.Fatal("fresh-only credential issuance failed", err)
	}
	fresh, err := f.target.Authenticate(ctx, freshToken, f.principal.OrganizationID)
	if err != nil || fresh.ID != freshID {
		t.Fatal("fresh authority unavailable with global gate proven normal", err)
	}
	freshTask := task(t, f.target, fresh, f.agent)
	claimed, err := f.target.ClaimFixture(ctx, "independent-import-fresh-worker", time.Minute)
	if err != nil || claimed == nil || claimed.TaskID != freshTask.ID || claimed.OperationID == f.expiredLease.OperationID {
		t.Fatal("historical queued/expired operations starved fresh same-org work", err)
	}
	if err = f.target.CompleteFixture(ctx, claimed, "Тестовый результат\n"+domain.ExecutionInput(*claimed)); err != nil {
		t.Fatal("fresh same-org fixture could not complete", err)
	}
	// Shared organization counters may advance for NEW work; compare every
	// remaining organization field and every other historical physical row.
	independentImportAssertRows(t, independentImportStableRows(t, f.rows), independentImportStableRows(t, independentImportRows(t, f.target, f.inventory, true)))
	if err = f.target.CheckRuntimeAdmission(ctx); err != nil {
		t.Fatal("fresh work unexpectedly re-held global control", err)
	}
	independentImportFaultControl(t, f, true)
	state, err := f.target.InspectRecoveryTarget(ctx)
	if err != nil || state != f.held {
		t.Fatal("trusted test fault did not restore exact original hold", err)
	}
}

func independentImportStableRows(t *testing.T, input map[string][]string) map[string][]string {
	t.Helper()
	result := make(map[string][]string, len(input))
	for name, rows := range input {
		result[name] = append([]string{}, rows...)
		if name != "organizations" {
			continue
		}
		for i, row := range rows {
			var fields map[string]json.RawMessage
			if json.Unmarshal([]byte(row), &fields) != nil {
				t.Fatal("shared aggregate observation could not decode")
			}
			for _, field := range []string{"reserved_microusd", "spent_microusd", "budget_microusd", "financial_hold"} {
				delete(fields, field)
			}
			encoded, err := json.Marshal(fields)
			if err != nil {
				t.Fatal("shared aggregate observation could not normalize")
			}
			result[name][i] = string(encoded)
		}
	}
	return result
}

// The existing pgwire fixture intentionally accepts literal loopback only.
// This separate, test-only opaque forwarder adapts ONLY the explicitly owned
// disposable CI service postgres:5432 to that unchanged fixture. It does not
// parse, record or log authentication/query packets and is not a generic remote
// proxy. It never reads saved application configuration.
type independentImportTCPForwarder struct {
	listener net.Listener
	mu       sync.Mutex
	closed   bool
	peers    map[net.Conn]net.Conn
	wg       sync.WaitGroup
}

func independentImportCommitProxy(t *testing.T, dsn string) (*Store, *independentCommitCutProxy) {
	t.Helper()
	if os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" || os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("commit cuts require both explicit newly owned recovery cluster flags")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("invalid independent disposable proxy configuration")
	}
	q := u.Query()
	schema := q.Get("search_path")
	if len(q["search_path"]) != 1 || len(schema) != 37 || !strings.HasPrefix(schema, "test_") || strings.Trim(schema[5:], "0123456789abcdef") != "" {
		t.Fatal("import proxy requires the canonical newly owned test schema")
	}
	for key := range q {
		switch strings.ToLower(key) {
		case "host", "hostaddr", "port", "service", "servicefile", "options":
			t.Fatal("connection overrides must not bypass the owned import proxy")
		}
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() && u.Port() != "" {
		store, proxy := independentCommitProxyStore(t, dsn)
		if store.runtimeSchema != schema {
			t.Fatal("import proxy opened a different physical schema")
		}
		return store, proxy
	}
	if u.Host != "postgres:5432" {
		t.Fatal("import forwarder accepts only the owned postgres CI service or literal loopback")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot start owned CI import forwarder")
	}
	f := &independentImportTCPForwarder{listener: listener, peers: map[net.Conn]net.Conn{}}
	f.wg.Add(1)
	go f.accept()
	t.Cleanup(func() {
		f.mu.Lock()
		f.closed = true
		_ = f.listener.Close()
		for client, server := range f.peers {
			_ = client.Close()
			_ = server.Close()
		}
		f.mu.Unlock()
		done := make(chan struct{})
		go func() { f.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned CI import forwarder did not stop")
		}
	})
	u.Host = listener.Addr().String()
	store, proxy := independentCommitProxyStore(t, u.String())
	if store.runtimeSchema != schema {
		t.Fatal("CI import proxy opened a different physical schema")
	}
	return store, proxy
}

func (f *independentImportTCPForwarder) accept() {
	defer f.wg.Done()
	for {
		client, err := f.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.DialTimeout("tcp", "postgres:5432", time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		f.mu.Lock()
		if f.closed || len(f.peers) >= 16 {
			f.mu.Unlock()
			_ = client.Close()
			_ = server.Close()
			continue
		}
		f.peers[client] = server
		f.wg.Add(1)
		f.mu.Unlock()
		go f.forward(client, server)
	}
}

func (f *independentImportTCPForwarder) forward(client, server net.Conn) {
	defer f.wg.Done()
	defer func() {
		_ = client.Close()
		_ = server.Close()
		f.mu.Lock()
		delete(f.peers, client)
		f.mu.Unlock()
	}()
	_ = client.SetDeadline(time.Now().Add(25 * time.Second))
	_ = server.SetDeadline(time.Now().Add(25 * time.Second))
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(server, io.LimitReader(client, 64<<20)); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, io.LimitReader(server, 64<<20)); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = server.Close()
	<-done
}

type independentImportSequenceState struct {
	Last   int64
	Called bool
}

func independentImportSequenceStates(t *testing.T, s *Store, sequences []RecoverySnapshotSequence) map[string]independentImportSequenceState {
	t.Helper()
	result := make(map[string]independentImportSequenceState, len(sequences))
	for _, sequence := range sequences {
		var state independentImportSequenceState
		if err := s.Pool.QueryRow(context.Background(), "SELECT last_value,is_called FROM "+pgx.Identifier{s.runtimeSchema, sequence.Name}.Sanitize()).Scan(&state.Last, &state.Called); err != nil {
			t.Fatal("independent import sequence observation failed")
		}
		result[sequence.Name] = state
	}
	return result
}

func TestRecoveryImportIndependentRealCommitAcknowledgmentLoss(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" || os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("commit cuts require both explicit newly owned recovery cluster flags")
	}
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentImportPreparedFixture(t)
			sequencesBefore := independentImportSequenceStates(t, f.target, f.snapshot.Sequences)
			victim, proxy := independentImportCommitProxy(t, f.targetDSN)
			// Match this actual import transaction, not the staging transaction or
			// an unrelated read/COMMIT. The original fixture observes and cuts the
			// PostgreSQL wire before forwarding COMMIT or after its real success.
			match := "insert into " + pgx.Identifier{victim.runtimeSchema, "recovery_import_receipts"}.Sanitize()
			plan := proxy.arm(t, match, mode)
			importCtx, stopImport := context.WithTimeout(context.Background(), 24*time.Second)
			defer stopImport()
			receipt, err := victim.ImportRecoverySnapshot(importCtx, f.archive, f.spec, DefaultRecoverySnapshotLimits())
			if !errors.Is(err, ErrRecoveryImportOutcomeUnknown) || !reflect.DeepEqual(receipt, RecoveryImportReceipt{}) {
				t.Fatal("lost import acknowledgment must return only explicit outcome uncertainty")
			}
			proxy.wait(t, plan)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Inspect through a NEW direct connection, independent of both proxy
			// and failed import result. Do not retry, drop, or clear the hold.
			observer, err := Open(ctx, f.targetDSN)
			if err != nil {
				t.Fatal("independent post-loss observer could not open")
			}
			t.Cleanup(observer.Close)
			f.target = observer
			inspection, err := observer.InspectRecoveryImport(ctx, f.spec, f.snapshot.SHA256)
			if err != nil {
				t.Fatal("fresh exact-tuple inspection failed after wire acknowledgment loss", err)
			}
			state, err := observer.InspectRecoveryTarget(ctx)
			if err != nil || state != f.held || !errors.Is(observer.CheckRuntimeAdmission(ctx), RecoveryHeld) {
				t.Fatal("wire failure changed or cleared the exact target hold", err)
			}
			var receipts, markers int64
			if err = observer.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM recovery_import_receipts),(SELECT count(*) FROM recovery_historical_rows)").Scan(&receipts, &markers); err != nil {
				t.Fatal("post-loss durable import facts unavailable")
			}
			if mode == "before_commit" {
				if inspection.State != "empty_held" || inspection.Receipt != nil || receipts != 0 || markers != 0 {
					t.Fatal("pre-COMMIT loss was replayed or left a partial import")
				}
				empty := make(map[string][]string, len(f.rows))
				for name := range f.rows {
					empty[name] = []string{}
				}
				independentImportAssertRows(t, empty, independentImportRows(t, observer, f.inventory, false))
				if !reflect.DeepEqual(sequencesBefore, independentImportSequenceStates(t, observer, f.snapshot.Sequences)) {
					t.Fatal("pre-COMMIT connection loss changed target sequence state")
				}
			} else {
				r := inspection.Receipt
				if inspection.State != "committed_held" || r == nil || receipts != 1 || markers == 0 || r.TargetInstallationID != f.spec.TargetInstallationID || r.RestoreID != f.spec.RestoreID || r.BackupID != f.spec.BackupID || r.SourceID != f.spec.SourceID || r.CutUTC != f.spec.CutUTC || r.SchemaSHA256 != f.spec.SchemaSHA256 || r.SnapshotSHA256 != f.snapshot.SHA256 || r.InventorySHA256 != f.snapshot.InventorySHA256 || !reflect.DeepEqual(r.Tables, f.snapshot.Tables) || !reflect.DeepEqual(r.Sequences, f.snapshot.Sequences) {
					t.Fatal("post-COMMIT loss did not retain exactly one complete bound import")
				}
				independentImportAssertRows(t, f.rows, independentImportRows(t, observer, f.inventory, false))
				independentImportAssertMarkers(t, f)
				sequencesAfter := independentImportSequenceStates(t, observer, f.snapshot.Sequences)
				for _, sequence := range f.snapshot.Sequences {
					if sequencesAfter[sequence.Name] != (independentImportSequenceState{Last: sequence.NextMinimum}) {
						t.Fatal("committed import lost exact unused sequence minimum")
					}
				}
			}
			wrong := f.spec
			wrong.RestoreID = "restore_" + strings.Repeat("e", 32)
			if _, err = observer.InspectRecoveryImport(ctx, wrong, f.snapshot.SHA256); err == nil {
				t.Fatal("post-loss inspection accepted a different restore tuple")
			}
			again, err := observer.InspectRecoveryImport(ctx, f.spec, f.snapshot.SHA256)
			if err != nil || !reflect.DeepEqual(again, inspection) {
				t.Fatal("repeated read-only outcome inspection changed durable import facts")
			}
		})
	}
}

// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const recoveryUsagePeerKey = "synthetic-owned-usage-peer-key"
const recoveryUsagePeerBody = `{"data":{"id":"gen-usage-selected","model":"example/usage-model","total_cost":0.0000001,"is_byok":true}}`

func verifyOwnedColdUsage(t *testing.T, ctx context.Context, operator *Store, spec RecoveryTargetSpec, f *evidenceAuthorFixture,
	link *recoveryarchive.VerifiedCaptureLink, stop *recoveryarchive.VerifiedSourceStop, reservation domain.Reservation, activation, interleavedOrg string,
	interleavedReceipt recoveryAccountingReceipt, freeCapacity func()) {
	t.Helper()
	limits := DefaultRecoverySnapshotLimits()
	selection := RecoveryUsageSelection{OrganizationID: reservation.OrganizationID, ReservationID: reservation.ID}
	if _, err := operator.PlanRecoveryUsageReadback(ctx, f.selected, f.late, link, spec, limits, selection); !errors.Is(err, ErrRecoveryUsageBinding) {
		t.Fatal("usage was planned without the authenticated retirement binding", err)
	}
	bound, err := link.WithRetiredSourceStop(stop)
	if err != nil {
		t.Fatal("fixture retirement did not bind its exact capture", err)
	}
	plan, err := operator.PlanRecoveryUsageReadback(ctx, f.selected, f.late, bound, spec, limits, selection)
	l := bound.Link()
	if err != nil || plan.State != "request_identity_available" || plan.GenerationID != "gen-handoff-late" ||
		plan.SelectedSnapshotSHA256 != l.EvidenceSnapshotSHA256 || plan.OriginalSnapshotSHA256 != l.SelectedSnapshotSHA256 || plan.ColdActivationID != activation {
		t.Fatal("late-only request did not bind the admitted baseline", err)
	}

	// Use real private authority and the production database executor with a
	// synthetic response. The fixed network transport runs in the Linux gate.
	parent := t.TempDir()
	if os.Chmod(parent, 0700) != nil {
		t.Fatal("private authority parent")
	}
	request := recoveryUsageRequestFromPlan(plan)
	policy := recoveryaccess.UsageHostPolicy{SourceID: plan.SourceID, OrganizationID: selection.OrganizationID,
		ModelConnectionID: plan.ModelConnectionID, ModelConnectionVersion: plan.ModelConnectionVersion, ConnectionIdentitySHA256: plan.ConnectionIdentitySHA,
		ArchivedProviderAccount: plan.ArchivedProviderAccount, CredentialReference: "owned-cold-test", ExpiresAt: time.Now().UTC().Add(time.Hour), Enabled: true}
	info, err := recoveryaccess.InitUsageHost(ctx, filepath.Join(parent, "authority"), request.Binding, policy, recoveryUsagePeerKey, "owned-cold-test")
	if err != nil {
		t.Fatal("admitted-baseline provider authority failed", err)
	}
	now := time.Now().UTC()
	raw := []byte(`{"data":{"id":"gen-handoff-late","model":"example/handoff","total_cost":0.0000001,"is_byok":true}}`)
	receipt := recoveryaccess.UsageAcquisitionReceipt{Version: 1, Request: request, Policy: *info.Usage, PolicyEpoch: info.Epoch, PolicySHA256: digestValue(info),
		PolicyOperationID: info.OperationID, PolicyConfirmedAt: info.ConfirmedAt, OperatorReference: info.OperatorReference, AccountMappingMethod: "current_host_operator_attestation",
		Observation: modelgateway.UsageObservationRecord{Profile: "openrouter-generation-observation-v1", GenerationID: plan.GenerationID, Model: plan.Model,
			TotalCostCredits: "0.0000001", IsBYOK: true, MicroUSDStatus: "fractional", ResponseSHA256: Hash(raw), ObservedAt: now, SettlementState: "unverified"}}
	receipt.ReceiptSHA256 = digestValue(receipt)
	if !recoveryaccess.UsageReceiptConsistent(receipt, raw) {
		t.Fatal("owned synthetic usage receipt inconsistent")
	}
	admission := recoveryaccess.UsagePolicyAdmission{PolicySHA256: receipt.PolicySHA256, PolicyEpoch: info.Epoch,
		PolicyOperationID: info.OperationID, AdmittedAt: now, ExpiresAt: policy.ExpiresAt}
	op := strings.TrimPrefix(domain.NewID("usage"), "usage_")
	before := recoveryUsageBusinessState(t, operator)
	evidence, err := operator.bindRecoveryUsageEvidence(ctx, f.selected, f.late, bound, spec, limits)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := operator.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	current, err := operator.planRecoveryUsageReadbackTx(ctx, tx, f.selected, f.late, bound, spec, limits, selection, evidence)
	if err != nil {
		t.Fatal("could not rederive selected usage under gate", err)
	}
	recorded, err := operator.recordRecoveryUsageTx(ctx, tx, spec, bound, current, admission, op, receipt, raw)
	if err != nil || recorded.ObservationState != "observed_unverified" {
		t.Fatal("cold usage bytes could not be retained", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("cold usage commit failed", err)
	}
	inspected, err := operator.InspectRecoveryUsage(ctx, f.selected, f.late, bound, spec, limits, selection, op, receipt.ReceiptSHA256)
	if err != nil || inspected != recorded || recoveryUsageBusinessState(t, operator) != before {
		t.Fatal("cold usage inspection changed or failed", err)
	}
	var retained []byte
	var chunks int
	if err = operator.Pool.QueryRow(ctx, `SELECT u.response,b.chunk_count FROM recovery_usage_observations u
	 JOIN recovery_evidence_batches b ON b.id=u.evidence_batch_id WHERE u.id=$1`, op).Scan(&retained, &chunks); err != nil || string(retained) != string(raw) || chunks != 0 {
		t.Fatal("cold observation lost exact bytes or duplicated archive data", err)
	}
	verifyOwnedColdFinance(t, ctx, operator, spec, f, bound, plan, op, receipt.ReceiptSHA256, parent, interleavedOrg, interleavedReceipt, freeCapacity)
}

func verifyOwnedColdFinance(t *testing.T, ctx context.Context, operator *Store, spec RecoveryTargetSpec, f *evidenceAuthorFixture,
	bound *recoveryarchive.VerifiedCaptureLink, plan RecoveryUsageReadbackPlan, observation, acquisition, parent, interleavedOrg string,
	interleavedReceipt recoveryAccountingReceipt, freeCapacity func()) {
	t.Helper()
	scope := recoveryarchive.FinanceScope{TargetID: plan.TargetID, RestoreID: plan.RestoreID, ActivationID: plan.ColdActivationID,
		OrganizationID: plan.Selection.OrganizationID, Account: plan.ArchivedProviderAccount}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("finance fixture key", err)
	}
	defer clear(key)
	trust := filepath.Join(parent, "finance-trust.json")
	if err = recoveryarchive.WriteHostJSON(trust, struct {
		Version int                          `json:"version"`
		Scope   recoveryarchive.FinanceScope `json:"scope"`
		Key     []byte                       `json:"public_key"`
	}{1, scope, pub}); err != nil {
		t.Fatal("finance fixture trust", err)
	}
	document := []byte("synthetic final account statement")
	documentPath := filepath.Join(parent, "statement-document.txt")
	if err = os.WriteFile(documentPath, document, 0600); err != nil {
		t.Fatal("finance fixture document", err)
	}
	var revision int
	if err = operator.Pool.QueryRow(ctx, "SELECT revision FROM reservations WHERE organization_id=$1 AND id=$2",
		plan.Selection.OrganizationID, plan.Selection.ReservationID).Scan(&revision); err != nil {
		t.Fatal("finance fixture revision", err)
	}
	makeStatement := func(file string, st recoveryarchive.FinanceStatement) recoveryarchive.VerifiedFinanceStatement {
		t.Helper()
		body, _ := json.Marshal(st)
		path := filepath.Join(parent, file)
		if err := recoveryarchive.WriteHostJSON(path, struct {
			Statement recoveryarchive.FinanceStatement `json:"statement"`
			Signature []byte                           `json:"signature"`
		}{st, ed25519.Sign(key, append([]byte("RunVerge final finance statement v1\x00"), body...))}); err != nil {
			t.Fatal("finance fixture envelope", err)
		}
		verified, err := recoveryarchive.VerifyHostFinanceStatement(path, trust, documentPath)
		if err != nil {
			t.Fatal("finance fixture attestation", err)
		}
		return verified
	}
	apply := func(v recoveryarchive.VerifiedFinanceStatement, post bool) (RecoveryFinanceResult, error) {
		return operator.SettleRecoveryFinanceStatement(ctx, f.selected, f.late, bound, spec, DefaultRecoverySnapshotLimits(), plan.Selection, v, post)
	}
	st := recoveryarchive.FinanceStatement{Version: 1, Scope: scope, ObservationID: observation, AcquisitionSHA: acquisition,
		InvoiceLine: "invoice-001:line-1", DocumentSHA: Hash(document), AmountMicroUSD: 7, ExpectedRevision: revision}
	statement := makeStatement("original-statement.json", st)
	posted, err := apply(statement, true)
	if err != nil || posted.State != "recorded" || posted.Outcome == nil || posted.Outcome.Status != "settled" ||
		posted.Outcome.Reservation.ActualMicroUSD == nil || *posted.Outcome.Reservation.ActualMicroUSD != 7 {
		t.Fatal("final signed amount did not post", err)
	}
	replay, err := apply(statement, true)
	if err != nil || replay.State != "already_recorded" || replay.OperationID != posted.OperationID || replay.Outcome == nil ||
		replay.Outcome.Reservation.Revision != posted.Outcome.Reservation.Revision {
		t.Fatal("same invoice line posted again", err)
	}
	st.AmountMicroUSD = 9
	if _, err = apply(makeStatement("changed-line.json", st), true); !errors.Is(err, ErrRecoveryAccounting) {
		t.Fatal("changed statement for an already posted line was accepted", err)
	}
	st.InvoiceLine, st.ExpectedRevision, st.CorrectionReason = "invoice-002:line-1", replay.Outcome.Reservation.Revision, "provider correction"
	corrected, err := apply(makeStatement("correction.json", st), true)
	if err != nil || corrected.Outcome == nil || corrected.Outcome.Reservation.ActualMicroUSD == nil ||
		*corrected.Outcome.Reservation.ActualMicroUSD != 9 || corrected.Outcome.Reservation.Revision != st.ExpectedRevision+1 {
		t.Fatal("independent signed correction did not post", err)
	}
	inspection, err := apply(statement, false)
	if err != nil || inspection.State != "already_recorded" || inspection.OperationID != posted.OperationID {
		t.Fatal("prior signed line was not inspectable after correction", err)
	}
	limits := DefaultRecoverySnapshotLimits()
	st.InvoiceLine, st.ExpectedRevision, st.AmountMicroUSD = "invoice-003:line-1", corrected.Outcome.Reservation.Revision, math.MaxInt64
	overflow := makeStatement("overflow.json", st)
	pending, err := apply(overflow, true)
	if err != nil || pending.Outcome == nil || pending.Outcome.Status != "quarantined" || pending.Outcome.QuarantineID == "" {
		t.Fatal("signed overflow did not retain its statement", err)
	}
	request := RecoveryFinanceResolution{QuarantineID: pending.Outcome.QuarantineID, ExpectedRevision: pending.Outcome.Reservation.Revision}
	resolve := func(v recoveryarchive.VerifiedFinanceStatement, r RecoveryFinanceResolution, write bool) (RecoveryFinanceResult, error) {
		return operator.ResolveRecoveryFinanceStatement(ctx, f.selected, f.late, bound, spec, limits, plan.Selection, v, r, write)
	}
	retained, err := resolve(overflow, request, true)
	if err != nil || retained.State != "quarantine_retained" || retained.Outcome == nil || retained.Outcome.Reservation.Revision != request.ExpectedRevision {
		t.Fatal("resolution with insufficient capacity changed accounting", err)
	}
	wrong := request
	wrong.ExpectedRevision--
	if _, err = resolve(overflow, wrong, true); !errors.Is(err, ErrRecoveryAccounting) {
		t.Fatal("stale quarantine revision was accepted", err)
	}
	if _, err = resolve(statement, request, true); !errors.Is(err, ErrRecoveryAccounting) {
		t.Fatal("unrelated statement was accepted for the quarantine", err)
	}
	freeCapacity()
	resolved, err := resolve(overflow, request, true)
	if err != nil || resolved.State != "recorded" || resolved.OperationID != retained.OperationID || resolved.Outcome == nil ||
		resolved.Outcome.Status != "settled" || resolved.Outcome.Reservation.ActualMicroUSD == nil ||
		*resolved.Outcome.Reservation.ActualMicroUSD != math.MaxInt64 || resolved.Outcome.Reservation.Revision != request.ExpectedRevision+1 {
		t.Fatal("signed quarantine did not resolve exactly once", err)
	}
	st.InvoiceLine, st.ExpectedRevision, st.AmountMicroUSD = "invoice-004:line-1", resolved.Outcome.Reservation.Revision, 11
	if _, err = apply(makeStatement("post-resolution-correction.json", st), true); err != nil {
		t.Fatal("correction after signed resolution failed", err)
	}
	replayed, err := resolve(overflow, request, false)
	if err != nil || replayed.State != "already_recorded" || replayed.OperationID != resolved.OperationID {
		t.Fatal("committed resolution was not inspectable after a correction", err)
	}
	var exported bytes.Buffer
	var report RecoverySnapshotReport
	snapshot, err := operator.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback(ctx)
	if err = writeRecoveryLineageGenerationTx(ctx, snapshot, operator.runtimeSchema,
		&recoverySnapshotWriter{ctx: ctx, out: &exported, hash: sha256.New(), limit: limits.MaxWireBytes}, limits, &report); err != nil {
		t.Fatal("retained finance export failed", err)
	}
	if err = snapshot.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var header recoveryLineageGenerationStart
	if err = json.Unmarshal(bytes.SplitN(exported.Bytes(), []byte("\n"), 2)[0], &header); err != nil {
		t.Fatal(err)
	}
	check, err := operator.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Rollback(ctx)
	inventory, err := recoverySnapshotInventory(ctx, check, operator.runtimeSchema, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stageRecoveryLineageGeneration(ctx, check, operator.runtimeSchema, bytes.NewReader(exported.Bytes()), inventory, header, limits); err != nil {
		t.Fatal("retained signed statements failed repeat-restore validation", err)
	}
	verifyOwnedLineageAdmissionBounds(t, ctx, check, header, interleavedOrg, interleavedReceipt)
}

func verifyOwnedLineageAdmissionBounds(t *testing.T, ctx context.Context, tx pgx.Tx, header recoveryLineageGenerationStart, org string, later recoveryAccountingReceipt) {
	t.Helper()
	if header.Profile != recoveryLineageAdmittedProfile {
		t.Fatal("activated source did not retain the admitted generation profile")
	}
	var schema string
	if err := tx.QueryRow(ctx, "SELECT nspname FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.pg_my_temp_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	legacy := header
	legacy.Profile, legacy.AdmissionReceiptTime = "sql031-current-generation-v1", ""
	if !errors.Is(validateRecoveryLineageRelations(ctx, tx, schema, legacy), ErrRecoveryStageData) {
		t.Fatal("a generation without admission accepted the interleaved mutation")
	}
	for _, change := range []struct {
		name, query string
		args        []any
	}{
		{"missing admission", "DELETE FROM pg_temp.recovery_work_admissions", nil},
		{"unadmitted organization", "UPDATE pg_temp.recovery_work_admissions SET organizations=array_remove(organizations,$1)", []any{org}},
		{"accounting head", "UPDATE pg_temp.recovery_work_admissions SET accounting_head_id=$1,accounting_head_ordinal=$2", []any{later.ID, later.Ordinal}},
		{"receipt commitment", `UPDATE pg_temp.recovery_work_admissions SET import_receipt_sha256=repeat('a',64);
 UPDATE pg_temp.recovery_evidence_batches SET report=jsonb_set(report,'{import_receipt_sha256}',to_jsonb(repeat('a',64))) WHERE payload_bytes=0`, nil},
	} {
		if _, err := tx.Exec(ctx, "SAVEPOINT broken_admission"); err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(ctx, change.query, change.args...)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatal("could not alter staged admission", change.name, err)
		}
		if err = validateRecoveryLineageRelations(ctx, tx, schema, header); !errors.Is(err, ErrRecoveryStageData) {
			t.Fatal("accepted altered staged admission", change.name, err)
		}
		if _, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT broken_admission"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "RELEASE SAVEPOINT broken_admission"); err != nil {
			t.Fatal(err)
		}
	}
}

// Runs only inside the disposable CI container whose hosts mapping sends the
// fixed production hostname to loopback. The test does not edit the user's host,
// supply an endpoint override, expose a credential callback, or contact a model.
func recoveryUsageOwnedHTTPS(t *testing.T) *atomic.Int32 {
	t.Helper()
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_USAGE_HTTPS") != "1" || os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("disposable owned HTTPS and PostgreSQL environment required")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("owned trust fixture requires disposable Linux container")
	}
	ips, err := net.LookupIP("openrouter.ai")
	if err != nil || len(ips) == 0 {
		t.Fatal("owned hostname unavailable")
	}
	for _, ip := range ips {
		if !ip.Equal(net.ParseIP("127.0.0.1")) {
			t.Fatal("refuse non-owned provider address")
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(17), DNSNames: []string{"openrouter.ai"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDir := t.TempDir()
	caPath := filepath.Join(privateDir, "owned-ca.pem")
	if err = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", caPath)
	t.Setenv("SSL_CERT_DIR", privateDir)
	listener, err := net.Listen("tcp", "127.0.0.1:443")
	if err != nil {
		t.Fatal("owned TLS listener", err)
	}
	var calls atomic.Int32
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/generation" || r.URL.RawQuery != "id=gen-usage-selected" || r.Header.Get("Authorization") != "Bearer "+recoveryUsagePeerKey {
			http.Error(w, "unexpected synthetic request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(recoveryUsagePeerBody))
	})}
	tlsListener := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	done := make(chan error, 1)
	go func() { done <- server.Serve(tlsListener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return &calls
}

func recoveryUsageRecordingFixture(t *testing.T) (*evidenceAuthorFixture, RecoveryUsageSelection, string, recoveryaccess.AcquiredUsageObservation) {
	t.Helper()
	f, selection := recoveryUsageAuthorFixture(t, "captured")
	ctx := context.Background()
	limits := DefaultRecoverySnapshotLimits()
	plan, err := f.target.PlanRecoveryUsageReadback(ctx, f.selected, f.late, f.verified, f.spec, limits, selection)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if os.Chmod(parent, 0700) != nil {
		t.Fatal("private authority parent")
	}
	path := filepath.Join(parent, "usage")
	_, err = recoveryaccess.InitUsageHost(ctx, path, recoveryaccess.Binding{TargetID: plan.TargetID, RestoreID: plan.RestoreID, SnapshotSHA256: plan.SelectedSnapshotSHA256}, recoveryaccess.UsageHostPolicy{SourceID: plan.SourceID, OrganizationID: selection.OrganizationID, ModelConnectionID: plan.ModelConnectionID, ModelConnectionVersion: plan.ModelConnectionVersion, ConnectionIdentitySHA256: plan.ConnectionIdentitySHA, ArchivedProviderAccount: plan.ArchivedProviderAccount, CredentialReference: "owned-test-key", ExpiresAt: time.Now().UTC().Add(time.Hour), Enabled: true}, recoveryUsagePeerKey, "operator:owned-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.target.AcquirePlannedRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, selection, path, strings.Repeat("0", 64)); !errors.Is(err, ErrRecoveryUsageBinding) {
		t.Fatal("wrong reviewed usage plan accepted", err)
	}
	o, err := f.target.AcquirePlannedRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, selection, path, plan.PlanSHA256)
	if err != nil {
		t.Fatal("owned acquisition", err)
	}
	if string(o.RawResponse()) != recoveryUsagePeerBody || o.Receipt().Observation.MicroUSDStatus != "fractional" || !o.Receipt().Observation.IsBYOK {
		t.Fatal("owned response representation")
	}
	return f, selection, path, o
}

func recoveryUsageBusinessState(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		t.Fatal(err)
	}
	var result strings.Builder
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		var raw []byte
		if err = tx.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]'::jsonb) FROM "+pgx.Identifier{s.runtimeSchema, table.Name}.Sanitize()+" r").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		result.WriteString(table.Name)
		result.Write(raw)
	}
	return Hash([]byte(result.String()))
}

func recoveryUsageSQLRefusals(t *testing.T, s *Store, originalID string) {
	t.Helper()
	ctx := context.Background()
	q := pgx.Identifier{s.runtimeSchema, "recovery_usage_observations"}.Sanitize()
	stored, err := scanRecoveryUsageStored(s.Pool.QueryRow(ctx, "SELECT "+recoveryUsageStoredColumns+" FROM "+q+" WHERE id=$1", originalID))
	if err != nil {
		t.Fatal(err)
	}
	type obj = map[string]any
	cases := []struct {
		name   string
		mutate func(obj, obj, obj, obj)
	}{
		{"control", func(p, a, d, r obj) {}},
		{"paired_null_model", func(p, a, d, r obj) {
			p["model"] = nil
			a["request"].(obj)["model"] = nil
			a["observation"].(obj)["model"] = nil
			r["data"].(obj)["model"] = nil
		}},
		{"paired_missing_generation", func(p, a, d, r obj) {
			delete(p, "generation_id")
			delete(a["request"].(obj), "generation_id")
			delete(a["observation"].(obj), "generation_id")
			delete(r["data"].(obj), "id")
		}},
		{"paired_null_account", func(p, a, d, r obj) {
			p["archived_provider_account"] = nil
			a["request"].(obj)["archived_provider_account"] = nil
			a["policy"].(obj)["archived_provider_account"] = nil
		}},
		{"string_connection_version", func(p, a, d, r obj) {
			p["model_connection_version"] = "1"
			a["request"].(obj)["model_connection_version"] = "1"
			a["policy"].(obj)["model_connection_version"] = "1"
		}},
		{"string_policy_epoch", func(p, a, d, r obj) { a["policy_epoch"] = "1"; d["policy_epoch"] = "1" }},
		{"paired_missing_catalog", func(p, a, d, r obj) { delete(p, "target_catalog_sha256"); delete(d, "target_catalog_sha256") }},
		{"missing_credential_digest", func(p, a, d, r obj) { delete(a["policy"].(obj), "credential_sha256") }},
		{"response_identity", func(p, a, d, r obj) { r["data"].(obj)["id"] = "gen-wrong" }},
		{"settled_observation", func(p, a, d, r obj) { a["observation"].(obj)["settlement_state"] = "settled" }},
		{"wrong_task", func(p, a, d, r obj) { p["task_id"] = "task-other" }},
		{"missing_operator_time", func(p, a, d, r obj) { delete(a, "policy_confirmed_at") }},
	}
	for index, c := range cases {
		t.Run("sql_"+c.name, func(t *testing.T) {
			var p, a, d, r obj
			for raw, target := range map[string]*obj{string(stored.plan): &p, string(stored.acquisition): &a, string(stored.admission): &d, string(stored.response): &r} {
				if json.Unmarshal([]byte(raw), target) != nil {
					t.Fatal("fixture JSON")
				}
			}
			op := fmt.Sprintf("%032x", index+1)
			d["operation_id"] = op
			// Give the attempted row a distinct acquisition digest, so uniqueness
			// cannot substitute for refusal by the identity/scope trigger.
			a["operator_reference"] = "operator:guard-" + c.name
			c.mutate(p, a, d, r)
			raw, _ := json.Marshal(r)
			responseSHA := Hash(raw)
			a["observation"].(obj)["response_sha256"] = responseSHA
			plan, _ := json.Marshal(p)
			planSHA := Hash(plan)
			a["request"].(obj)["source_plan_sha256"] = planSHA
			acquisition, _ := json.Marshal(a)
			acquisitionSHA := Hash(acquisition)
			d["source_plan_sha256"], d["acquisition_sha256"], d["response_sha256"] = planSHA, acquisitionSHA, responseSHA
			admission, _ := json.Marshal(d)
			tx, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var recorded time.Time
			started := time.Now().Add(-5 * time.Second)
			err = tx.QueryRow(ctx, "INSERT INTO "+q+`(id,restore_id,target_installation_id,evidence_batch_id,organization_id,reservation_id,source_plan_sha256,source_plan_commitment,acquisition_sha256,acquisition_commitment,response_sha256,response,admission_sha256,admission_commitment,recorded_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'2001-01-01T00:00:00Z') RETURNING recorded_at`, op, stored.restore, stored.target, stored.batch, stored.org, stored.reservation, planSHA, plan, acquisitionSHA, acquisition, responseSHA, raw, Hash(admission), admission).Scan(&recorded)
			var pgErr *pgconn.PgError
			guarded := errors.As(err, &pgErr) && pgErr.Code == "55000"
			if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
				t.Fatal("guard test rollback", rollbackErr)
			}
			if c.name == "control" {
				if err != nil || recorded.Before(started) {
					t.Fatal("SQL fixture control or server timestamp failed", err)
				}
			} else if !guarded {
				t.Fatal("exact identity guard did not refuse", err)
			}
		})
	}
}

// Advance the verified accounting chain by an exact financial-hold mutation.
// This is the existing typed executor, not a forged source invoice or settlement.
func recoveryUsageAdvanceAccounting(t *testing.T, f *evidenceAuthorFixture, selection RecoveryUsageSelection) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	held, _, err := f.target.evidenceTargetTx(ctx, tx, f.spec, f.before.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	q := func(n string) string { return pgx.Identifier{f.target.runtimeSchema, n}.Sanitize() }
	b := recoveryAccountingBatch{id: strings.Repeat("e", 32), evidenceBatchID: f.verified.Link().OperationID, organizationID: selection.OrganizationID, reservationID: selection.ReservationID, requestSHA256: strings.Repeat("a", 64), planSHA256: strings.Repeat("b", 64)}
	if err = tx.QueryRow(ctx, "SELECT revision FROM "+q("reservations")+" WHERE organization_id=$1 AND id=$2", selection.OrganizationID, selection.ReservationID).Scan(&b.expectedRevision); err != nil {
		t.Fatal(err)
	}
	m := recoveryAccountingMutation{table: "organizations"}
	if err = tx.QueryRow(ctx, "SELECT to_jsonb(o),to_jsonb(o)||jsonb_build_object('financial_hold',true) FROM "+q("organizations")+" o WHERE id=$1 AND NOT financial_hold", selection.OrganizationID).Scan(&m.before, &m.after); err != nil {
		t.Fatal(err)
	}
	if _, err = f.target.recordRecoveryAccountingMutationsTx(ctx, tx, held, b, []recoveryAccountingMutation{m}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryUsageAuthorOwnedAcquisitionPersistence(t *testing.T) {
	calls := recoveryUsageOwnedHTTPS(t)
	t.Run("actual_database_restart", func(t *testing.T) {
		f, selection, path, o := recoveryUsageRecordingFixture(t)
		beforeCalls := calls.Load()
		ctx := context.Background()
		limits := DefaultRecoverySnapshotLimits()
		op := strings.Repeat("9", 32)
		before := recoveryUsageBusinessState(t, f.target)
		written, err := f.target.RecordRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, selection, path, op, o)
		if err != nil {
			t.Fatal("record before restart", err)
		}
		fresh := recoveryUsageRestartOwnedPostgres(t, f.target)
		inspected, err := fresh.InspectRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, selection, op, o.Receipt().ReceiptSHA256)
		if err != nil || inspected.State != "recorded" || inspected.AdmissionSHA256 != written.AdmissionSHA256 || inspected.ResponseSHA256 != written.ResponseSHA256 || !inspected.RecordedAt.Equal(written.RecordedAt) {
			t.Fatal("restart changed observation", err)
		}
		if recoveryUsageBusinessState(t, fresh) != before || calls.Load() != beforeCalls {
			t.Fatal("restart inspection changed business state or called provider")
		}
	})
	t.Run("record_duplicate_conflict_revoke_fresh_inspect", func(t *testing.T) {
		f, selection, path, o := recoveryUsageRecordingFixture(t)
		beforeCalls := calls.Load()
		ctx := context.Background()
		limits := DefaultRecoverySnapshotLimits()
		op := strings.Repeat("a", 32)
		before := recoveryUsageBusinessState(t, f.target)
		record := func(s *Store, id string, ob recoveryaccess.AcquiredUsageObservation) (RecoveryUsageRecording, error) {
			return s.RecordRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, selection, path, id, ob)
		}
		got, err := record(f.target, op, o)
		if err != nil || got.State != "recorded" || got.ObservationState != "observed_unverified" || !got.ExecutionHeld {
			t.Fatal("record", err)
		}
		recoveryUsageSQLRefusals(t, f.target, op)
		duplicate, err := record(f.target, strings.Repeat("b", 32), o)
		if err != nil || duplicate.State != "already_recorded" || duplicate.OperationID != op || duplicate.AdmissionSHA256 != got.AdmissionSHA256 {
			t.Fatal("duplicate", err)
		}
		var raw []byte
		if err = f.target.Pool.QueryRow(ctx, "SELECT response FROM "+pgx.Identifier{f.target.runtimeSchema, "recovery_usage_observations"}.Sanitize()+" WHERE id=$1", op).Scan(&raw); err != nil || string(raw) != recoveryUsagePeerBody {
			t.Fatal("durable exact bytes", err)
		}
		// A separate explicit acquisition is distinct even when the reply bytes
		// match. It must not overwrite a prior caller operation identifier.
		second, err := f.target.AcquireRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, selection, path)
		if err != nil {
			t.Fatal(err)
		}
		beforeCalls++
		if result, err := record(f.target, op, second); err == nil || !reflect.DeepEqual(result, RecoveryUsageRecording{}) {
			t.Fatal("conflicting operation accepted")
		}
		victim, proxy := independentImportCommitProxy(t, f.target.Pool.Config().ConnString())
		cut := proxy.arm(t, "rollback", "before_statement")
		if result, err := record(victim, op, o); !errors.Is(err, ErrRecoveryStageCleanup) || !reflect.DeepEqual(result, RecoveryUsageRecording{}) {
			t.Fatal("rollback failure returned duplicate success", err)
		}
		proxy.wait(t, cut)
		if recoveryUsageBusinessState(t, f.target) != before {
			t.Fatal("observation changed business state")
		}
		recoveryUsageAdvanceAccounting(t, f, selection)
		before = recoveryUsageBusinessState(t, f.target)
		if result, err := record(f.target, strings.Repeat("f", 32), second); !errors.Is(err, ErrRecoveryUsageRecording) || !reflect.DeepEqual(result, RecoveryUsageRecording{}) {
			t.Fatal("stale acquisition head admitted", err)
		}
		info, err := recoveryaccess.InspectHost(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		policy := *info.Current.Usage
		policy.Enabled = false
		if _, err = recoveryaccess.ReplaceUsageHost(ctx, path, info.Current.Epoch, policy, "", "operator:revoke"); err != nil {
			t.Fatal(err)
		}
		if result, err := record(f.target, strings.Repeat("c", 32), second); err == nil || !reflect.DeepEqual(result, RecoveryUsageRecording{}) {
			t.Fatal("revoked admission")
		}
		fresh, err := Open(ctx, f.target.Pool.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		defer fresh.Close()
		inspected, err := fresh.InspectRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, selection, op, o.Receipt().ReceiptSHA256)
		if err != nil || inspected.OperationID != op || inspected.AdmissionSHA256 != got.AdmissionSHA256 {
			t.Fatal("fresh historical inspection", err)
		}
		wrong := selection
		wrong.OrganizationID = "other-org"
		if result, err := fresh.InspectRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, wrong, op, o.Receipt().ReceiptSHA256); err == nil || !reflect.DeepEqual(result, RecoveryUsageRecording{}) {
			t.Fatal("cross-organization inspection")
		}
		if recoveryUsageBusinessState(t, f.target) != before || f.target.CheckRuntimeAdmission(ctx) != RecoveryHeld || calls.Load() != beforeCalls {
			t.Fatal("observation changed business state, hold or repeated HTTP")
		}
	})
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f, selection, path, o := recoveryUsageRecordingFixture(t)
			beforeCalls := calls.Load()
			ctx := context.Background()
			limits := DefaultRecoverySnapshotLimits()
			op := strings.Repeat("d", 32)
			victim, proxy := independentImportCommitProxy(t, f.target.Pool.Config().ConnString())
			cut := proxy.arm(t, "insert into "+pgx.Identifier{victim.runtimeSchema, "recovery_usage_observations"}.Sanitize(), mode)
			result, err := victim.RecordRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, selection, path, op, o)
			if !errors.Is(err, ErrRecoveryUsageUnknown) || !reflect.DeepEqual(result, RecoveryUsageRecording{}) {
				t.Fatal("commit loss", err)
			}
			proxy.wait(t, cut)
			observer, err := Open(ctx, f.target.Pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			inspected, err := observer.InspectRecoveryUsage(ctx, f.selected, f.late, f.verified, f.spec, limits, selection, op, o.Receipt().ReceiptSHA256)
			want := "recorded"
			if mode == "before_commit" {
				want = "not_recorded"
			}
			if err != nil || inspected.State != want || calls.Load() != beforeCalls || observer.CheckRuntimeAdmission(ctx) != RecoveryHeld {
				t.Fatal("unknown commit inspection", err)
			}
		})
	}
}

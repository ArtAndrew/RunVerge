// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	oidcidentity "agentplatform.local/platform/internal/identity"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
	"github.com/jackc/pgx/v5"
)

type ownedHandoffOIDC struct {
	oldSession oidcidentity.BrowserSession
	orgs       [2]string
}

type ownedHandoffInvoices struct {
	archive                     *recoveryarchive.VerifiedArchive
	link                        *recoveryarchive.VerifiedCaptureLink
	initial, correction, offset RecoveryAccountingSelection
}

func TestRecoveryHandoffAuthorReviewExactLateTarget(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" {
		t.Skip("requires a new owned cluster for whole database provisioning")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var lateReservation domain.Reservation
	var offset domain.AccountingOutcome
	var fixtureSource *Store
	var oldPrincipal string
	var oidc ownedHandoffOIDC
	oldToken := domain.NewID("ap") + domain.NewID("key")
	f := evidenceAuthorSetupWithMutations(t, func(s *Store) {
		if err := s.Pool.QueryRow(ctx, "SELECT principal_id FROM memberships WHERE role='owner'").Scan(&oldPrincipal); err != nil {
			t.Fatal("historical principal unavailable", err)
		}
	}, func(s *Store) {
		fixtureSource = s
		first, _ := identity(t, s, 100000)
		second, _ := identity(t, s, 100000)
		browser, oldSession, oidcRuntime := independentSecurityBrowser(t, s, first)
		policy := independentSecurityPolicy(t, s, browser, oidcRuntime)
		independentSecurityPolicyUpdate(t, s, browser, policy, "oidc_required", oldSession.Issuer, false, policy.MaxServiceKeyTTLSeconds, oidcRuntime)
		oidc = ownedHandoffOIDC{oldSession, [2]string{first.OrganizationID, second.OrganizationID}}
		if _, err := s.Pool.Exec(ctx, "INSERT INTO access_tokens(token_hash,principal_id) VALUES($1,$2)", Hash([]byte(oldToken)), oldPrincipal); err != nil {
			t.Fatal("historical credential fixture failed", err)
		}
		p, _ := identity(t, s, 100000)
		model := domain.ModelConnection{ID: "handoff-window-model", OrganizationID: p.OrganizationID, Version: 1, WireProfile: domain.ModelWireOpenRouterTextV1,
			Model: "example/handoff", ProviderAccount: "synthetic-handoff", PriceVersion: "synthetic-v1", Endpoint: modelgateway.OpenRouterEndpoint,
			CredentialEnvironment: "PLATFORM_MODEL_KEY_HANDOFF_TEST", MaxChargeMicroUSD: 37, MaxInputTokens: 32, InputMicroUSDPerMillion: 1000000,
			MaxOutputTokens: 16, MaxInputBytes: 4096, MaxOutputBytes: 4096, TimeoutMilliseconds: 2000, TokenLimitField: "max_tokens",
			BoundAssumptions: "Synthetic history only", Active: true}
		model, err := s.InstallModelConnection(ctx, model)
		if err != nil {
			t.Fatal(err)
		}
		var complete func() domain.Artifact
		lateReservation, complete = recoveryUsageAuthorAttempt(t, s, p, model, "gen-handoff-late")
		complete()
		prior, complete := recoveryUsageAuthorAttempt(t, s, p, model, "gen-handoff-offset")
		complete()
		offset, err = s.AccountCost(ctx, p.OrganizationID, prior.ID, domain.CostEvidence{Source: prior.Provider,
			Reference: "owned-handoff-offset", PayloadDigest: Hash([]byte("owned offset")), ActualMicroUSD: 1, Final: true, Currency: "USD"})
		if err != nil || offset.Status != "settled" {
			t.Fatal("could not establish the exact overflow offset", err)
		}
		if _, err = s.Pool.Exec(ctx, "UPDATE organizations SET financial_hold=true WHERE id=$1", p.OrganizationID); err != nil {
			t.Fatal("could not establish the owned financial hold", err)
		}
	})
	// The snapshots and provisioned target are real. This signed V2 metadata
	// is synthetic and does not substitute for an installed Docker stop drill.
	late := f.late.Manifest()
	l := f.verified.Link()
	l.Version, l.ExportAlgorithm = 2, recoveryarchive.DrainedCaptureAlgorithm
	l.SystemID, l.DatabaseOID = "123", 123
	l.ExportWireBytes, l.EvidenceSchemaSHA256, l.EvidenceBackupID = late.PlaintextBytes, late.Metadata.SchemaSHA256, late.Metadata.BackupID
	l.ExecutorsStoppedAtUTC = late.Metadata.CutUTC
	sourceStop := ownedHandoffPeerStopRecord(t, l)
	l.BindingSHA256 = digestValue(sourceStop.Binding)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keys, linkPath := filepath.Join(dir, "keys"), filepath.Join(dir, "capture")
	if _, err := recoveryarchive.GenerateHostKeys(keys); err != nil {
		t.Fatal(err)
	}
	if err := recoveryarchive.WriteHostCaptureLink(linkPath, keys, l); err != nil {
		t.Fatal(err)
	}
	link, err := recoveryarchive.VerifyHostCaptureLink(linkPath, keys)
	if err != nil {
		t.Fatal(err)
	}
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal("owned maintenance connection unavailable")
	}
	defer admin.Close(context.Background())
	destination, err := PlanRecoveryRetirementDestination(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PlanRecoveryProvisionAtRetiredDestination(ctx, admin, late.Metadata.BackupID, late.Metadata.SourceID, late.PlaintextSHA256, late.Metadata.SchemaSHA256, Hash([]byte("owned handoff provenance fixture")), destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, e := admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgx.Identifier{p.Database}.Sanitize()+" WITH (FORCE)"); e != nil {
			t.Error("owned database cleanup failed")
		}
		if _, e := admin.Exec(cleanup, "DROP ROLE IF EXISTS "+pgx.Identifier{p.Owner}.Sanitize()); e != nil {
			t.Error("owned role cleanup failed")
		}
		if _, e := admin.Exec(cleanup, "DROP ROLE IF EXISTS "+pgx.Identifier{recoveryRuntimeRoleName(p.OperationID)}.Sanitize()); e != nil {
			t.Error("owned runtime role cleanup failed")
		}
	}()
	ob, err := CreateRecoveryProvision(ctx, admin, p)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal("invalid test connection format")
	}
	u.Path, u.RawPath = "/"+p.Database, ""
	target, err := OpenRecoveryProvisionTarget(ctx, admin, u.String(), p, ob.DatabaseOID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if target != nil {
			target.Close()
		}
	}()
	if err = target.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := target.InspectRecoveryTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec := RecoveryTargetSpec{TargetInstallationID: state.InstallationID, RestoreID: p.OperationID, SourceID: p.SourceID,
		BackupID: p.BackupID, CutUTC: late.Metadata.CutUTC, SchemaSHA256: p.SchemaSHA256}
	if _, err = target.PrepareRecoveryTarget(ctx, spec); err != nil {
		t.Fatal(err)
	}
	limits := DefaultRecoverySnapshotLimits()
	review := func() (RecoveryHandoffTargetReview, error) {
		return target.ReviewRecoveryHandoffTarget(ctx, f.selected, f.late, link, spec, limits, nil)
	}
	if _, err = review(); err == nil {
		t.Fatal("empty held target treated as a committed import")
	}
	imported, err := target.ImportRecoverySnapshot(ctx, f.late, spec, limits)
	if err != nil {
		t.Fatal(err)
	}
	invoices := ownedHandoffInvoice(t, ctx, fixtureSource, f, offset.Reservation)
	if _, err = target.IngestRecoveryEvidence(ctx, f.late, invoices.archive, invoices.link, spec, limits); err != nil {
		t.Fatal("could not retain the synthetic invoice before cutover", err)
	}
	result, err := review()
	if err != nil || result.Target.State != "held" || result.DatabaseOID != ob.DatabaseOID || result.ProvenanceSHA256 != p.ProvenanceSHA256 || result.ImportReceiptSHA256 != recoveryImportReceiptCommitment(imported) || result.AccountingHeadID != "" || result.AccountingHeadOrdinal != 0 || result.ExecutionWindow.CoverageProven || result.ExecutionWindow.SourceStopRechecked || result.ExecutionWindow.NewWorkAllowed {
		t.Fatal("invalid exact-target review", err)
	}
	lateFound, zeroUnknown := false, false
	for _, entry := range result.ExecutionWindow.Records {
		if entry.RecordID == lateReservation.ActionID && entry.OrganizationID == lateReservation.OrganizationID {
			lateFound = entry.Comparison == "captured_only" && entry.Original == nil && entry.Captured.DispatchRecorded
		}
	}
	for _, entry := range result.OpenReservations {
		if entry.OrganizationID == f.oldOrg && entry.Position.Amount == 0 && entry.Position.Status == "unknown" && entry.Position.Actual == nil {
			zeroUnknown = true
		}
	}
	if !lateFound || !zeroUnknown || !errors.Is(target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("lost a late action, zero unknown reserve or execution hold")
	}
	closed, peer, verifiedStop := verifyOwnedHandoffPeerWindow(t, ctx, dir, keys, target, spec, f, link, sourceStop, result, lateReservation.OrganizationID)
	repeat, err := review()
	if err != nil || !reflect.DeepEqual(result, repeat) {
		t.Fatal("repeated review was unstable", err)
	}
	if _, err = f.target.ReviewRecoveryHandoffTarget(ctx, f.selected, f.late, link, f.spec, limits, nil); err == nil {
		t.Fatal("generic original target adopted a cold handoff")
	}
	if _, err = target.ReviewRecoveryHandoffTarget(ctx, f.selected, f.late, f.verified, spec, limits, nil); err == nil {
		t.Fatal("V1 evidence promoted to drained handoff")
	}
	if _, err = target.ReviewRecoveryHandoffTarget(ctx, f.late, f.late, link, spec, limits, nil); err == nil {
		t.Fatal("a different original archive was accepted")
	}
	// Corrupt one marker without changing the count. Only the fixture uses
	// privileged trigger suspension, within its own disposable target.
	var marker string
	if err = target.Pool.QueryRow(ctx, `SELECT pk_sha256 FROM recovery_historical_rows WHERE table_name='principals' ORDER BY pk_sha256 LIMIT 1`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	replaceMarker := func(from, to string) {
		t.Helper()
		tx, e := target.Pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, "ALTER TABLE recovery_historical_rows DISABLE TRIGGER recovery_mark_permanent"); e != nil {
			t.Fatal(e)
		}
		tag, e := tx.Exec(ctx, "UPDATE recovery_historical_rows SET pk_sha256=$1 WHERE table_name='principals' AND pk_sha256=$2", to, from)
		if e != nil || tag.RowsAffected() != 1 {
			t.Fatal("owned fixture marker replacement failed", e)
		}
		if _, e = tx.Exec(ctx, "ALTER TABLE recovery_historical_rows ENABLE TRIGGER recovery_mark_permanent"); e != nil || tx.Commit(ctx) != nil {
			t.Fatal("owned fixture marker replacement commit failed", e)
		}
	}
	badMarker := strings.Repeat("f", 64)
	replaceMarker(marker, badMarker)
	if _, err = review(); !errors.Is(err, ErrRecoveryHandoffReview) {
		t.Fatal("wrong key with unchanged count accepted", err)
	}
	replaceMarker(badMarker, marker)
	for _, sequence := range imported.Sequences {
		if sequence.Name == "events_sequence_seq" && sequence.NextMinimum > 1 {
			if _, err = target.Pool.Exec(ctx, "ALTER SEQUENCE events_sequence_seq RESTART WITH "+strconv.FormatInt(sequence.NextMinimum-1, 10)); err != nil {
				t.Fatal(err)
			}
			if _, err = review(); !errors.Is(err, ErrRecoveryHandoffReview) {
				t.Fatal("rewound sequence accepted", err)
			}
			if _, err = target.Pool.Exec(ctx, "ALTER SEQUENCE events_sequence_seq RESTART WITH "+strconv.FormatInt(sequence.NextMinimum, 10)); err != nil {
				t.Fatal("could not restore fixture sequence", err)
			}
			var completed domain.Task
			var request RecoveryWorkAdmissionRequest
			var completeAnother func(*testing.T) domain.Task
			if t.Run("first_ordinary_task", func(t *testing.T) {
				completed, request, completeAnother = verifyOwnedHandoffWork(t, ctx, target, spec, f, link, closed, peer, u, oldToken, lateReservation.OrganizationID, oidc)
			}) {
				target.Close()
				target = nil
				t.Run("accounting_after_new_work", func(t *testing.T) {
					// A new operator process must reconnect to the activated
					// target without relying on an earlier held session.
					if wrong, err := OpenRecoveryProvisionTarget(ctx, admin, u.String(), p, ob.DatabaseOID); !errors.Is(err, ErrRecoveryProvision) {
						if wrong != nil {
							wrong.Close()
						}
						t.Fatal("active database was accepted by held provisioning")
					}
					if wrong, err := OpenAdmittedRecoveryProvisionTarget(ctx, admin, u.String(), p, ob.DatabaseOID+1); !errors.Is(err, ErrRecoveryProvision) {
						if wrong != nil {
							wrong.Close()
						}
						t.Fatal("wrong database identity was accepted for active accounting")
					}
					operator, err := OpenAdmittedRecoveryProvisionTarget(ctx, admin, u.String(), p, ob.DatabaseOID)
					if err != nil {
						t.Fatal("could not rebind operator connection to the activated database", err)
					}
					defer operator.Close()
					inspection, err := operator.InspectRecoveryWorkAdmission(ctx, spec, request)
					if err != nil || inspection.State != "recorded" || inspection.Admission == nil || inspection.RequestSHA != request.SHA256() {
						t.Fatal("exact committed credentials were not inspectable", err)
					}
					tampered := request
					tampered.OperatorReference += " changed"
					if _, err = operator.InspectRecoveryWorkAdmission(ctx, spec, tampered); !errors.Is(err, ErrRecoveryWorkAdmission) {
						t.Fatal("changed activation request was accepted", err)
					}
					verifyOwnedHandoffInvoice(t, ctx, operator, spec, f.late, invoices.archive, invoices.link, invoices.initial, completed)
					ordinary := completeAnother(t)
					applyCorrection := func(selection RecoveryAccountingSelection, amount int64) RecoveryAccountingResult {
						t.Helper()
						plan, err := operator.PlanRecoveryAccounting(ctx, f.late, invoices.archive, invoices.link, spec, limits, selection)
						if err != nil {
							t.Fatal("could not plan the archived correction", err)
						}
						result, err := operator.ApplyRecoveryAccounting(ctx, f.late, invoices.archive, invoices.link, spec, limits, selection, plan.PlanSHA256)
						if err != nil || !result.NewlyRecorded || result.Outcome.Reservation.ActualMicroUSD == nil || *result.Outcome.Reservation.ActualMicroUSD != amount {
							t.Fatal("could not apply the exact archived correction", err)
						}
						return result
					}
					corrected := applyCorrection(invoices.correction, 9)
					var gap int64
					if err = operator.Pool.QueryRow(ctx, `SELECT (n.before_row->>'spent_microusd')::bigint-(p.after_row->>'spent_microusd')::bigint
 FROM recovery_accounting_mutations p JOIN recovery_accounting_mutations n USING(table_name,key_parts)
 WHERE p.batch_id=$1 AND n.batch_id=$2 AND p.table_name='organizations'`,
						invoices.initial.OperationID, corrected.Receipt.ID).Scan(&gap); err != nil || gap != ordinary.SpentMicroUSD || gap != domain.FixtureCost {
						t.Fatal("the interleaved task did not change the historical batch before-image", gap, err)
					}
					freeCapacity := func() { applyCorrection(invoices.offset, 0) }
					verifyOwnedColdUsage(t, ctx, operator, spec, f, link, verifiedStop, lateReservation, request.ActivationID, f.oldOrg, corrected.Receipt, freeCapacity)
					verifyOwnedHandoffOIDCRepeatRestore(t, ctx, operator, spec, f.late.Manifest().PlaintextSHA256, oidc, ordinary, invoices.correction.ReservationID)
				})
			}
			return
		}
	}
	t.Fatal("fixture event sequence did not advance")
}

func ownedHandoffPeerStopRecord(t *testing.T, l recoveryarchive.CaptureLink) recoveryprocess.SourceStopRecord {
	t.Helper()
	origin, err := time.Parse(time.RFC3339Nano, l.SelectedCutUTC)
	if err != nil {
		t.Fatal("invalid owned cut time")
	}
	p := recoveryprocess.Intent{Version: 1, OperationID: l.OperationID, DaemonID: "synthetic-owned-peer-daemon", Project: "owned-peer", CreatedAt: origin}
	b := recoveryprocess.SourceBinding{Version: 1, SourceID: l.SourceID, SnapshotSHA256: l.SelectedSnapshotSHA256,
		RuntimeImage: "sha256:" + strings.Repeat("e", 64), SystemID: l.SystemID, DatabaseOID: l.DatabaseOID, ObservedAt: origin}
	o := recoveryprocess.Observation{OperationID: p.OperationID, Project: p.Project, SourceID: b.SourceID,
		ProjectProcessesStopped: true, SourceInstallationBound: true, BoundSourceProcessesStopped: true}
	for i, service := range []string{"db", "api", "worker"} {
		id := strings.Repeat(strconv.Itoa(i+1), 64)
		stamp := origin.Add(-time.Minute).Format(time.RFC3339Nano)
		p.Containers = append(p.Containers, recoveryprocess.Container{ID: id, Name: "/owned-peer-" + service, Service: service, Created: stamp, Image: b.RuntimeImage, StartedAt: stamp})
		o.Containers = append(o.Containers, recoveryprocess.ContainerState{ID: id, Service: service, Status: "exited", RestartPolicy: "no", Stopped: true})
		if service == "db" {
			b.DatabaseContainer = id
		} else {
			b.Processes = append(b.Processes, recoveryprocess.SourceProcess{Container: id, BootID: strings.Repeat("a", 32)})
		}
	}
	b.PlanSHA256 = digestValue(p)
	return recoveryprocess.SourceStopRecord{Intent: p, Binding: b, Observation: o}
}

func verifyOwnedHandoffPeerWindow(t *testing.T, ctx context.Context, dir, keys string, target *Store, spec RecoveryTargetSpec, f *evidenceAuthorFixture,
	link *recoveryarchive.VerifiedCaptureLink, record recoveryprocess.SourceStopRecord, original RecoveryHandoffTargetReview, chargedOrg string) (RecoveryHandoffTargetReview, *recoveryarchive.VerifiedRecoveryPeerJournal, *recoveryarchive.VerifiedSourceStop) {
	t.Helper()
	// The databases and imports are real. The sealed Docker observation and
	// peer dispositions below are explicitly synthetic and use test-only keys.
	sealPath := filepath.Join(dir, "retirement-seal")
	if err := recoveryarchive.WriteHostSourceRetirementSeal(sealPath, keys, link, recoveryarchive.SourceRetirementBinding{
		RestoreID: spec.RestoreID, IntentSHA256: Hash([]byte("synthetic intent")),
		AdmissionReceiptSHA256: Hash([]byte("synthetic admission receipt")), CutProofSHA256: Hash([]byte("synthetic continuity")),
	}); err != nil {
		t.Fatal(err)
	}
	seal, err := recoveryarchive.VerifyHostSourceRetirementSeal(sealPath, keys, link)
	if err != nil {
		t.Fatal(err)
	}
	record.Observation.ObservedAt = time.Now().UTC()
	stopReceipt := recoveryarchive.SourceStopReceipt{Version: 2, Profile: "exclusive-local-compose-retired-stop-v2",
		CaptureLinkSHA256: link.SHA256(), Capture: link.Link(), Source: record, RetirementSealSHA256: seal.SHA256()}
	rawKey, err := recoveryarchive.ReadHostPrivateFile(keys)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(rawKey)
	var material struct {
		Seed []byte `json:"signing_seed"`
	}
	if json.Unmarshal(rawKey, &material) != nil || len(material.Seed) != ed25519.SeedSize {
		t.Fatal("synthetic archive key invalid")
	}
	defer clear(material.Seed)
	sourceKey := ed25519.NewKeyFromSeed(material.Seed)
	defer clear(sourceKey)
	stopFile := filepath.Join(dir, "source-stop")
	body, _ := json.Marshal(stopReceipt)
	if err = recoveryarchive.WriteHostJSON(stopFile, struct {
		Receipt   recoveryarchive.SourceStopReceipt `json:"receipt"`
		Signature []byte                            `json:"signature"`
	}{stopReceipt, ed25519.Sign(sourceKey, append([]byte("RunVerge retired source stop v2\x00"), body...))}); err != nil {
		t.Fatal(err)
	}
	stop, err := recoveryarchive.VerifyHostSourceStop(stopFile, keys, link, seal)
	if err != nil {
		t.Fatal(err)
	}
	scope := recoveryarchive.RecoveryPeerScope{SourceID: spec.SourceID, TargetID: spec.TargetInstallationID, RestoreID: spec.RestoreID,
		SourceStopSHA256: stop.SHA256(), RetirementSealSHA256: seal.SHA256(), CaptureLinkSHA256: link.SHA256(), OriginalCutUTC: link.Link().SelectedCutUTC}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	trust := filepath.Join(dir, "peer-trust")
	if err = recoveryarchive.WriteHostJSON(trust, struct {
		Version int                               `json:"version"`
		Scope   recoveryarchive.RecoveryPeerScope `json:"scope"`
		Key     []byte                            `json:"public_key"`
	}{1, scope, pub}); err != nil {
		t.Fatal(err)
	}
	evidence := Hash([]byte("synthetic independent observation"))
	j := recoveryarchive.RecoveryPeerJournal{Version: 1, Profile: recoveryarchive.RecoveryPeerJournalProfile, Scope: scope,
		FencedAtUTC: link.Link().EvidenceCutUTC, CoveredThroughUTC: record.Observation.ObservedAt.Format(time.RFC3339Nano),
		EgressProofSHA256: Hash([]byte("synthetic closed egress")), Records: []recoveryarchive.RecoveryPeerJournalRecord{}}
	for _, r := range original.ExecutionWindow.Records {
		if r.Captured != nil && r.Captured.DispatchRecorded {
			j.Records = append(j.Records, recoveryarchive.RecoveryPeerJournalRecord{Family: r.Family, OrganizationID: r.OrganizationID, RecordID: r.RecordID,
				DispatchSHA256: r.Captured.DispatchSHA256, EvidenceSHA256: evidence, Disposition: "no_effect_confirmed"})
		}
	}
	if len(j.Records) == 0 {
		t.Fatal("owned fixture lacks a sent action")
	}
	var closedReport RecoveryHandoffTargetReview
	var closedPeer *recoveryarchive.VerifiedRecoveryPeerJournal
	run := func(name string, journal recoveryarchive.RecoveryPeerJournal) RecoveryPeerWindowReview {
		t.Helper()
		path := filepath.Join(dir, name)
		raw, _ := json.Marshal(journal)
		if err := recoveryarchive.WriteHostJSON(path, struct {
			Journal   recoveryarchive.RecoveryPeerJournal `json:"journal"`
			Signature []byte                              `json:"signature"`
		}{journal, ed25519.Sign(key, append([]byte("RunVerge independent egress journal v1\x00"), raw...))}); err != nil {
			t.Fatal(err)
		}
		peer, err := recoveryarchive.VerifyHostRecoveryPeerJournal(path, trust, spec.TargetInstallationID, stop)
		if err != nil {
			t.Fatal(err)
		}
		report, err := target.ReviewRecoveryHandoffTarget(ctx, f.selected, f.late, link, spec, DefaultRecoverySnapshotLimits(), peer)
		if err != nil || report.PeerWindow == nil || report.ExecutionWindow.NewWorkAllowed || report.Target.State != "held" ||
			report.ExecutionWindow.CoverageProven != report.PeerWindow.CoverageProven || !reflect.DeepEqual(report.OpenReservations, original.OpenReservations) ||
			!reflect.DeepEqual(report.Organizations, original.Organizations) || report.AccountingHeadID != original.AccountingHeadID {
			t.Fatal("peer facts changed execution, accounting or existing liabilities", err)
		}
		if name == "closed-journal" {
			closedReport, closedPeer = report, peer
		}
		return *report.PeerWindow
	}
	ok := run("closed-journal", j)
	coveredLiability := false
	for _, org := range ok.Organizations {
		if org.OrganizationID == chargedOrg && org.BoundLiabilities == 1 && org.CoverageProven && org.FinancialHold {
			coveredLiability = true
		}
	}
	if !ok.CoverageProven || !coveredLiability {
		t.Fatal("exact closed peer journal failed to retain the paid action liability", ok.Blockers)
	}
	for _, org := range []string{f.newOrg, ""} {
		other := j
		other.Records = append(append([]recoveryarchive.RecoveryPeerJournalRecord{}, j.Records...), recoveryarchive.RecoveryPeerJournalRecord{
			Family: "unmatched", OrganizationID: org, RecordID: "unmatched-peer-effect", EvidenceSHA256: evidence, Disposition: "effect_observed"})
		blocked := run("unmatched-journal-"+org, other)
		if blocked.CoverageProven || len(blocked.Unmatched) != 1 || blocked.Unmatched[0].EvidenceSHA256 != evidence {
			t.Fatal("unmatched independent evidence disappeared")
		}
		for _, result := range blocked.Organizations {
			if result.CoverageProven != (org != "" && result.OrganizationID != org) {
				t.Fatal("unmatched evidence did not hold the correct scope")
			}
		}
	}
	mismatch := j
	mismatch.Records = append([]recoveryarchive.RecoveryPeerJournalRecord{}, j.Records...)
	mismatch.Records[0].DispatchSHA256 = Hash([]byte("another admission"))
	blocked := run("mismatching-dispatch", mismatch)
	if blocked.CoverageProven || len(blocked.Unmatched) != 1 {
		t.Fatal("peer disposition for another admission was accepted")
	}
	unknown := j
	unknown.Records = append([]recoveryarchive.RecoveryPeerJournalRecord{}, j.Records...)
	unknown.Records[0].Disposition = "unknown"
	if review := run("unknown-external-effect", unknown); review.CoverageProven || len(review.Unmatched) != 0 {
		t.Fatal("an exactly matched but unresolved effect qualified")
	}
	missing := j
	missing.Records = append([]recoveryarchive.RecoveryPeerJournalRecord{}, j.Records[1:]...)
	if review := run("missing-external-effect", missing); review.CoverageProven {
		t.Fatal("an omitted admitted action qualified")
	}
	return closedReport, closedPeer, stop
}

func verifyOwnedHandoffWork(t *testing.T, ctx context.Context, target *Store, spec RecoveryTargetSpec, f *evidenceAuthorFixture,
	link *recoveryarchive.VerifiedCaptureLink, closed RecoveryHandoffTargetReview, peer *recoveryarchive.VerifiedRecoveryPeerJournal, adminURL *url.URL, oldToken, chargedOrg string,
	oidc ownedHandoffOIDC) (domain.Task, RecoveryWorkAdmissionRequest, func(*testing.T) domain.Task) {
	t.Helper()
	limits := DefaultRecoverySnapshotLimits()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	defer clear(secret)
	password := base64.RawURLEncoding.EncodeToString(secret)
	verifier, err := RecoveryRuntimeVerifier(password)
	if err != nil || !RecoveryRuntimePasswordMatches(password, verifier) {
		t.Fatal("test password verifier unavailable", err)
	}
	token := domain.NewID("ap") + domain.NewID("key")
	bearer := RecoveryWorkOwner{OrganizationID: f.oldOrg, PrincipalID: domain.NewID("usr"), DisplayName: "Restored owner", TokenSHA256: Hash([]byte(token))}
	sso := RecoveryWorkOwner{PrincipalID: domain.NewID("usr"), DisplayName: "Restored SSO owner", OIDCIssuer: oidc.oldSession.Issuer, OIDCSubject: oidc.oldSession.Subject}
	owners := []RecoveryWorkOwner{bearer}
	for _, org := range oidc.orgs {
		sso.OrganizationID = org
		owners = append(owners, sso)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].OrganizationID < owners[j].OrganizationID })
	request := RecoveryWorkAdmissionRequest{
		ActivationID: domain.NewID("activate"), TargetReviewSHA256: closed.SHA256(), OperatorReference: "Owned test cutover", RuntimeVerifier: verifier,
		Owners: owners,
	}
	role := recoveryRuntimeRoleName(spec.RestoreID)
	var called bool
	_, err = target.AdmitRecoveryWork(ctx, f.selected, f.late, link, spec, limits, peer, request, func(context.Context) (recoveryarchive.CurrentRetiredSourceStop, error) {
		called = true
		return recoveryarchive.CurrentRetiredSourceStop{}, nil
	})
	if !errors.Is(err, ErrRecoveryWorkAdmission) || !called {
		t.Fatal("zero source observation was not rejected at the commit boundary", called, err)
	}
	if inspection, err := target.InspectRecoveryWorkAdmission(ctx, spec, request); err != nil || inspection.State != "not_recorded" || inspection.Admission != nil {
		t.Fatal("absence of the rolled back admission was not inspectable", err)
	}
	var residue bool
	if err = target.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recovery_work_admissions)
	 OR EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=$1)
	 OR EXISTS(SELECT 1 FROM principals WHERE id=$2)
	 OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE datname=current_database() AND datconnlimit=0)`,
		role, bearer.PrincipalID).Scan(&residue); err != nil || residue {
		t.Fatal("failed source observation left a partial activation", err)
	}

	// macOS has no real Linux Docker stop observation. The fixture commits
	// the production transaction preparation only after verifying the same
	// held archive and signed peer journal. The public entry point above
	// still refused to commit without the opaque live witness.
	original, baseline, err := target.bindRecoveryHandoffReview(ctx, f.selected, f.late, link, spec, limits, peer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	review, err := target.reviewRecoveryHandoffTargetTx(ctx, tx, f.selected, f.late, spec, limits, peer, original, baseline)
	if err != nil {
		t.Fatal("could not repeat exact review", err)
	}
	disallowed := request
	disallowed.Owners = []RecoveryWorkOwner{bearer}
	disallowed.Owners[0].OrganizationID = chargedOrg
	if _, err = target.prepareRecoveryWorkAdmissionTx(ctx, tx, spec, disallowed, review); !errors.Is(err, ErrRecoveryWorkAdmission) {
		t.Fatal("financially held scope received new work authority", err)
	}
	admission, err := target.prepareRecoveryWorkAdmissionTx(ctx, tx, spec, request, review)
	if err != nil || admission.RuntimeRole != role || admission.AdmittedAt.IsZero() || admission.ReviewSHA256 != request.TargetReviewSHA256 {
		t.Fatal("bound admission preparation failed", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("owned admission commit failed", err)
	}
	if !errors.Is(target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("administrative connection obtained ordinary runtime authority")
	}
	runtimeURL := *adminURL
	runtimeURL.User = url.UserPassword(role, password)
	runtimeStore, err := Open(ctx, runtimeURL.String())
	if err != nil {
		t.Fatal("restricted runtime connection unavailable")
	}
	defer runtimeStore.Close()
	if err = runtimeStore.CheckRuntimeAdmission(ctx); err != nil {
		t.Fatal("restricted runtime was not admitted", err)
	}
	for _, statement := range []string{
		"SELECT id FROM recovery_accounting_batches",
		"SELECT activation_id FROM recovery_work_admissions",
		"SELECT principal_id FROM recovery_oidc_rebindings",
		"TRUNCATE operations",
		"UPDATE recovery_historical_rows SET pk_sha256=pk_sha256",
	} {
		if _, err = runtimeStore.Pool.Exec(ctx, statement); err == nil {
			t.Fatal("restricted runtime obtained protected privileges")
		}
	}
	if _, err = runtimeStore.Pool.Exec(ctx, "UPDATE organizations SET budget_microusd=budget_microusd+1 WHERE id=$1", f.newOrg); err == nil {
		t.Fatal("write crossed the reviewed organization boundary")
	}
	if _, err = runtimeStore.Pool.Exec(ctx, "UPDATE operations SET status='queued' WHERE organization_id=$1 AND status='leased'", f.oldOrg); err == nil {
		t.Fatal("historical operation could be requeued")
	}
	if _, err = runtimeStore.Authenticate(ctx, oldToken, f.oldOrg); !errors.Is(err, domain.Unauthenticated) {
		t.Fatal("a formerly valid historical credential authenticated", err)
	}
	if _, err = runtimeStore.Authenticate(ctx, token, f.newOrg); !errors.Is(err, domain.Unauthenticated) {
		t.Fatal("a fresh credential crossed the reviewed scope", err)
	}
	p, err := runtimeStore.Authenticate(ctx, token, f.oldOrg)
	if err != nil || p.ID != bearer.PrincipalID || p.Role != "owner" {
		t.Fatal("the fresh owner could not authenticate", err)
	}
	p = verifyOwnedHandoffOIDC(t, ctx, runtimeStore, oidc, sso.PrincipalID, f.oldOrg)
	if lease, err := runtimeStore.ClaimFixture(ctx, "restored-test-worker", time.Minute); err != nil || lease != nil {
		t.Fatal("the first worker would pick up historical work", err)
	}
	got := completeOwnedHandoffTask(t, ctx, runtimeStore, p)
	if lease, err := runtimeStore.ClaimFixture(ctx, "restored-test-worker", time.Minute); err != nil || lease != nil {
		t.Fatal("historical work appeared after the new task completed", err)
	}
	for _, row := range closed.OpenReservations {
		var state string
		var amount int64
		var actual *int64
		if err = target.Pool.QueryRow(ctx, "SELECT status,amount_microusd,actual_microusd FROM reservations WHERE organization_id=$1 AND id=$2",
			row.OrganizationID, row.ReservationID).Scan(&state, &amount, &actual); err != nil || state != row.Position.Status || amount != row.Position.Amount ||
			!reflect.DeepEqual(actual, row.Position.Actual) {
			t.Fatal("an outstanding historical reservation changed", err)
		}
	}
	var hold bool
	if err = target.Pool.QueryRow(ctx, "SELECT financial_hold FROM organizations WHERE id=$1", chargedOrg).Scan(&hold); err != nil || !hold {
		t.Fatal("historical financial hold did not survive new work", err)
	}
	completeAnother := func(t *testing.T) domain.Task {
		t.Helper()
		runtime, err := Open(ctx, runtimeURL.String())
		if err != nil {
			t.Fatal("could not reopen the admitted runtime", err)
		}
		defer runtime.Close()
		owner, err := runtime.Authenticate(ctx, token, f.oldOrg)
		if err != nil || owner.ID != bearer.PrincipalID {
			t.Fatal("could not authenticate the owner for interleaved work", err)
		}
		return completeOwnedHandoffTask(t, ctx, runtime, owner)
	}
	return got, request, completeAnother
}

func completeOwnedHandoffTask(t *testing.T, ctx context.Context, runtime *Store, p domain.Principal) domain.Task {
	t.Helper()
	a := agent(t, runtime, p)
	task := task(t, runtime, p, a)
	lease, err := runtime.ClaimFixture(ctx, "restored-test-worker", time.Minute)
	if err != nil || lease == nil || lease.TaskID != task.ID {
		t.Fatal("the new task was not the only admitted job", err)
	}
	content := "Тестовый результат\n" + domain.ExecutionInput(*lease)
	if err = runtime.CompleteFixture(ctx, lease, content); err != nil {
		t.Fatal("the new task could not complete", err)
	}
	got, err := runtime.GetTask(ctx, p, task.ID)
	if err != nil || got.Status != "succeeded" || got.Result != content || got.SpentMicroUSD != domain.FixtureCost {
		t.Fatal("the new task result or synthetic accounting was lost", err)
	}
	return got
}

func verifyOwnedHandoffOIDC(t *testing.T, ctx context.Context, runtime *Store, oidc ownedHandoffOIDC, principal, deniedOrg string) domain.Principal {
	t.Helper()
	old := oidc.oldSession
	if _, err := runtime.GetSession(ctx, old.TokenHash); !errors.Is(err, oidcidentity.ErrSessionInvalid) {
		t.Fatal("historical browser session remained usable", err)
	}
	id, err := runtime.ResolveIdentity(ctx, old.Issuer, old.Subject)
	if err != nil || id != principal || id == old.PrincipalID {
		t.Fatal("same issuer/subject did not resolve to the approved new identity", err)
	}
	fresh := old
	fresh.ID, fresh.TokenHash, fresh.PrincipalID = domain.NewID("ses"), Hash([]byte(domain.NewID("key"))), id
	fresh.AuthenticatedAt = time.Now().UTC().Truncate(time.Microsecond)
	fresh.CreatedAt, fresh.ExpiresAt = fresh.AuthenticatedAt, fresh.AuthenticatedAt.Add(time.Hour)
	if err = runtime.CreateSession(ctx, fresh, old.TokenHash); err != nil {
		t.Fatal("SSO login with an old browser cookie failed", err)
	}
	items, _, err := runtime.BrowserOrganizations(ctx, fresh.TokenHash, "", 10)
	if err != nil || len(items) != 2 {
		t.Fatal("SSO session did not see exactly its reviewed organizations", err)
	}
	if _, err = runtime.AuthenticateBrowserSession(ctx, fresh.TokenHash, deniedOrg); !errors.Is(err, domain.Unauthenticated) {
		t.Fatal("SSO session reached an organization outside its reviewed memberships", err)
	}
	var p domain.Principal
	for _, org := range oidc.orgs {
		p, err = runtime.AuthenticateBrowserSession(ctx, fresh.TokenHash, org)
		if err != nil || p.ID != principal || p.Role != "owner" {
			t.Fatal("SSO owner could not use its reviewed membership", err)
		}
	}
	replacement := fresh
	replacement.ID, replacement.TokenHash = domain.NewID("ses"), Hash([]byte(domain.NewID("key")))
	if err = runtime.RotateSession(ctx, fresh.TokenHash, replacement); err != nil {
		t.Fatal("fresh SSO session could not rotate", err)
	}
	p, err = runtime.AuthenticateBrowserSession(ctx, replacement.TokenHash, oidc.orgs[0])
	if err != nil || p.ID != principal {
		t.Fatal("rotated SSO session lost approved access", err)
	}
	return p
}

func verifyOwnedHandoffOIDCRepeatRestore(t *testing.T, ctx context.Context, operator *Store, spec RecoveryTargetSpec, baselineSHA string,
	oidc ownedHandoffOIDC, ordinary domain.Task, correctedReservation string) {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	limits := DefaultRecoverySnapshotLimits()
	var data bytes.Buffer
	report, err := operator.ExportHeldRecoveryLineageSnapshot(ctx, &data, limits, spec, baselineSHA, parent)
	if err != nil || report.Lineage == nil {
		t.Fatal("activated SSO target could not be backed up", err)
	}
	archive := recoveryLineageTestArchive(t, data.Bytes(), report.Metadata, parent)
	target, _ := recoveryAuthorStore(t)
	nextSpec := recoveryTargetAuthorSpec(t, target)
	nextSpec.BackupID, nextSpec.SourceID = report.Metadata.BackupID, report.Metadata.SourceID
	nextSpec.CutUTC, nextSpec.SchemaSHA256 = report.Metadata.CutUTC, report.Metadata.SchemaSHA256
	if _, err = target.PrepareRecoveryTarget(ctx, nextSpec); err != nil {
		t.Fatal("second restore could not prepare its held target", err)
	}
	if _, err = target.ImportRecoverySnapshotLineage(ctx, archive, nextSpec, limits, parent); err != nil {
		t.Fatal("second restore could not import the current SSO history", err)
	}
	var previous string
	var freshHistorical, sessionsHistorical bool
	if err = target.Pool.QueryRow(ctx, `SELECT previous_principal_id,recovery_historical_principal(principal_id),
 (SELECT count(*)>=2 AND bool_and(recovery_is_historical('browser_sessions',jsonb_build_array(s.token_hash)))
  FROM browser_sessions s WHERE s.issuer=r.issuer AND s.subject=r.subject)
 FROM recovery_oidc_rebindings r WHERE r.issuer=$1 AND r.subject=$2 AND generation=1`, oidc.oldSession.Issuer,
		oidc.oldSession.Subject).Scan(&previous, &freshHistorical, &sessionsHistorical); err != nil ||
		previous != oidc.oldSession.PrincipalID || !freshHistorical || !sessionsHistorical || !errors.Is(target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("second restore did not fence both generations of SSO authority", err)
	}
	var result, status string
	var spent, invoice int64
	var admitted bool
	if err = target.Pool.QueryRow(ctx, `SELECT t.result,t.status,t.spent_microusd,r.actual_microusd,
 EXISTS(SELECT 1 FROM recovery_work_admissions)
 FROM tasks t JOIN reservations r USING(organization_id) WHERE t.organization_id=$1 AND t.id=$2 AND r.id=$3`,
		ordinary.OrganizationID, ordinary.ID, correctedReservation).Scan(&result, &status, &spent, &invoice, &admitted); err != nil ||
		result != ordinary.Result || status != ordinary.Status || spent != ordinary.SpentMicroUSD || invoice != 9 || admitted {
		t.Fatal("repeat restore changed interleaved work, its accounting, or carried old authority", err)
	}
}

func ownedHandoffInvoice(t *testing.T, ctx context.Context, source *Store, f *evidenceAuthorFixture, offset domain.Reservation) ownedHandoffInvoices {
	t.Helper()
	var reservation string
	if err := source.Pool.QueryRow(ctx, `SELECT id FROM reservations WHERE organization_id=$1 AND provider='author-owned-test'
	 AND status='unknown' AND amount_microusd=37`, f.oldOrg).Scan(&reservation); err != nil {
		t.Fatal("synthetic invoice reservation unavailable", err)
	}
	evidence := domain.CostEvidence{Source: "author-owned-test", Reference: "owned-handoff-invoice", PayloadDigest: Hash([]byte("owned fixture invoice")),
		ActualMicroUSD: 0, Final: true, Currency: "USD"}
	booked, err := source.AccountCost(ctx, f.oldOrg, reservation, evidence)
	if err != nil || booked.Status != "settled" {
		t.Fatal("synthetic source invoice failed", err)
	}
	evidence.Reference, evidence.PayloadDigest, evidence.Reason, evidence.ActualMicroUSD =
		"owned-handoff-invoice-correction", Hash([]byte("owned invoice correction")), "provider corrected invoice", 9
	invoiceCorrection, err := source.AccountAdjustment(ctx, f.oldOrg, reservation, domain.CostAdjustment{ExpectedRevision: booked.Reservation.Revision, Evidence: evidence})
	if err != nil || invoiceCorrection.Status != "settled" {
		t.Fatal("synthetic source correction failed", err)
	}
	corrected, err := source.AccountAdjustment(ctx, offset.OrganizationID, offset.ID, domain.CostAdjustment{ExpectedRevision: offset.Revision,
		Evidence: domain.CostEvidence{Source: offset.Provider, Reference: "owned-handoff-offset-correction", PayloadDigest: Hash([]byte("owned offset correction")),
			Reason: "provider corrected offset", ActualMicroUSD: 0, Final: true, Currency: "USD"}})
	if err != nil || corrected.Status != "settled" {
		t.Fatal("synthetic offset correction failed", err)
	}
	var snapshot bytes.Buffer
	if _, err = source.ExportRecoverySnapshot(ctx, &snapshot, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("synthetic source invoice archive failed", err)
	}
	archive := recoveryStageAuthorArchive(t, snapshot.Bytes())
	return ownedHandoffInvoices{
		archive: archive, link: evidenceAuthorPair(t, f.late, archive),
		initial:    recoveryAccountingAuthorSelection(f.oldOrg, reservation, booked.EvidenceID),
		correction: recoveryAccountingAuthorSelection(f.oldOrg, reservation, invoiceCorrection.EvidenceID),
		offset:     recoveryAccountingAuthorSelection(offset.OrganizationID, offset.ID, corrected.EvidenceID),
	}
}

func verifyOwnedHandoffInvoice(t *testing.T, ctx context.Context, target *Store, spec RecoveryTargetSpec,
	original, evidence *recoveryarchive.VerifiedArchive, link *recoveryarchive.VerifiedCaptureLink, invoice RecoveryAccountingSelection, completed domain.Task) {
	t.Helper()
	limits := DefaultRecoverySnapshotLimits()
	plan, err := target.PlanRecoveryAccounting(ctx, original, evidence, link, spec, limits, invoice)
	if err != nil {
		t.Fatal("ordinary work prevented planning historical accounting", err)
	}
	result, err := target.ApplyRecoveryAccounting(ctx, original, evidence, link, spec, limits, invoice, plan.PlanSHA256)
	if err != nil || !result.NewlyRecorded || result.Receipt.Ordinal != 1 || result.Outcome.Reservation.ActualMicroUSD == nil ||
		*result.Outcome.Reservation.ActualMicroUSD != 0 {
		t.Fatal("ordinary work prevented exact historical settlement", err)
	}
	inspection, err := target.InspectRecoveryAccounting(ctx, original, link, spec, limits, invoice.OperationID, plan.RequestSHA256)
	if err != nil || inspection.State != "recorded" || inspection.HeadID != result.Receipt.ID || inspection.HeadOrdinal != 1 {
		t.Fatal("committed historical settlement was not inspectable", err)
	}
	replay, err := target.ApplyRecoveryAccounting(ctx, original, evidence, link, spec, limits, invoice, plan.PlanSHA256)
	if err != nil || replay.NewlyRecorded || replay.State != "already_recorded" || replay.Receipt.ID != result.Receipt.ID {
		t.Fatal("historical settlement could not be safely inspected through the original plan", err)
	}
	var spent int64
	var status, content string
	if err = target.Pool.QueryRow(ctx, "SELECT status,result,spent_microusd FROM tasks WHERE organization_id=$1 AND id=$2", completed.OrganizationID, completed.ID).
		Scan(&status, &content, &spent); err != nil || status != completed.Status || content != completed.Result || spent != completed.SpentMicroUSD {
		t.Fatal("historical settlement rewrote the fresh result or its cost", err)
	}
}

// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/toolruntime"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Author lifecycle tests reuse the repository's independent model fixture only
// to obtain a real unverified candidate. They are not independent review evidence.
type authorToolWire struct {
	ExecutionID string          `json:"execution_id"`
	ActionID    string          `json:"action_id"`
	IntentHash  string          `json:"intent_hash"`
	Arguments   json.RawMessage `json:"arguments"`
}

func TestAuthorToolLateConflictIsRetainedDeduplicatedAndBounded(t *testing.T) {
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { conn, _, _ := w.(http.Hijacker).Hijack(); conn.Close() }))
	defer peer.Close()
	f, l, c := authorToolReady(t, peer, 60)
	ctx := context.Background()
	if _, err := authorToolRuntime(t, f.s, c).Execute(ctx, l); err == nil {
		t.Fatal("expected unknown transport")
	}
	v, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, l.Execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.ReconcileToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), domain.ReconcileToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, IntentHash: v.Execution.IntentHash, Outcome: "rejected", EffectReference: "operator-evidence", Result: "Operator observed no effect", EvidenceDigest: Hash([]byte("operator")), Reason: "Explicit operator resolution"})
	if err != nil {
		t.Fatal(err)
	}
	v = independentToolView(t, raw, nil)
	if v.Receipt == nil {
		t.Fatal("missing operator receipt")
	}
	receipt := v.Receipt.ID
	in := domain.ToolOutcomeEvidence{OrganizationID: f.p.OrganizationID, ExecutionID: l.Execution.ID, IntentHash: l.Execution.IntentHash, FencingToken: l.FencingToken, Outcome: "succeeded", Result: "Late provider evidence", EffectReference: "late-provider-reference", EvidenceDigest: Hash([]byte("late"))}
	for range 2 {
		if _, err = f.s.RecordToolOutcome(ctx, in); err == nil {
			t.Fatal("contradiction silently overwrote receipt")
		}
	}
	var count int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM tool_outcome_conflicts WHERE organization_id=$1 AND execution_id=$2", f.p.OrganizationID, l.Execution.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("late conflict not deduplicated", count, err)
	}
	for i := range 12 {
		in.Result = fmt.Sprintf("late observation %d", i)
		in.EvidenceDigest = Hash([]byte(in.Result))
		if _, err = f.s.RecordToolOutcome(ctx, in); err == nil {
			t.Fatal("late conflict unexpectedly accepted")
		}
	}
	var overflow int
	if err = f.s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM tool_outcome_conflicts WHERE organization_id=$1 AND execution_id=$2),(SELECT count(*) FROM events WHERE organization_id=$1 AND task_id=$3 AND type='tool.outcome_conflict_overflow')", f.p.OrganizationID, l.Execution.ID, f.task.ID).Scan(&count, &overflow); err != nil || count != 8 || overflow != 1 {
		t.Fatal("conflict history was not bounded", count, overflow, err)
	}
	final, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, l.Execution.ID)
	if err != nil || final.Execution.Status != "rejected" || final.Receipt.ID != receipt || final.Task.Status != "waiting_input" {
		t.Fatal("late conflict changed accepted observation", err)
	}
}

func authorToolResponse(w http.ResponseWriter, in authorToolWire) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": in.ExecutionID, "action_id": in.ActionID, "intent_hash": in.IntentHash, "outcome": "succeeded", "effect_reference": "author-fixture-effect", "result": "Local external result; not a real provider"})
}
func authorToolReady(t *testing.T, peer *httptest.Server, expires int) (*independentToolFixture, *domain.ToolLease, domain.ToolConnection) {
	t.Helper()
	f := independentToolSetup(t, false)
	c := f.tool
	c.Tool.Version++
	c.Endpoint = peer.URL
	c.CredentialEnvironment = ""
	c.TrustedCAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))
	var err error
	f.tool, err = f.s.InstallToolConnection(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	in := f.prepareInput()
	in.ExpiresInSeconds = expires
	raw, err := f.s.PrepareToolExecution(context.Background(), f.p, f.task.ID, domain.NewID("key"), in)
	v := independentToolView(t, raw, err)
	v = f.decide(t, v, f.p, "approve")
	f.dispatch(t, v)
	l, c, err := f.s.ClaimToolExecution(context.Background(), "author-worker", 30*time.Second)
	if err != nil || l == nil {
		t.Fatalf("tool lease: %v", err)
	}
	return f, l, c
}
func authorToolRuntime(t *testing.T, s toolruntime.Store, c domain.ToolConnection) *toolruntime.Runtime {
	t.Helper()
	r, err := toolruntime.New(s, toolruntime.Config{Connection: c, PersistenceTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAuthorToolRealTLSReceiptPreservesCandidateAndCostHold(t *testing.T) {
	var calls atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var in authorToolWire
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.Header.Get("Idempotency-Key") != in.ActionID {
			t.Error("action idempotency binding")
		}
		authorToolResponse(w, in)
	}))
	defer peer.Close()
	f, l, c := authorToolReady(t, peer, 60)
	out, err := authorToolRuntime(t, f.s, c).Execute(context.Background(), l)
	if err != nil || out.Status != "succeeded" || calls.Load() != 1 {
		t.Fatal(out, err)
	}
	task, err := f.s.GetTask(context.Background(), f.p, f.task.ID)
	if err != nil || task.Status != "waiting_input" || task.CandidateID != l.Execution.CandidateID || task.CurrentRunID != l.Execution.RunID {
		t.Fatal("effect receipt changed source candidate/run", err)
	}
	var reservation, action string
	var held int64
	if err = f.s.Pool.QueryRow(context.Background(), "SELECT r.status,a.status,r.amount_microusd FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.id=$2", f.p.OrganizationID, l.Execution.ReservationID).Scan(&reservation, &action, &held); err != nil {
		t.Fatal(err)
	}
	if reservation != "unknown" || action != "unknown" || held != 120 {
		t.Fatal("effect response settled money")
	}
	if _, err = authorToolRuntime(t, f.s, c).Execute(context.Background(), l); err == nil || calls.Load() != 1 {
		t.Fatal("same execution resent")
	}
}
func TestAuthorToolLostResponseRetainsHoldAndCannotReclaim(t *testing.T) {
	var effects atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer peer.Close()
	f, l, c := authorToolReady(t, peer, 60)
	out, err := authorToolRuntime(t, f.s, c).Execute(context.Background(), l)
	if err == nil || out.Status != "unknown" || effects.Load() != 1 {
		t.Fatal(out, err)
	}
	task, err := f.s.GetTask(context.Background(), f.p, f.task.ID)
	if err != nil || task.Status != "blocked" || task.WaitingReason != "external_action_unknown" {
		t.Fatal("unknown effect not blocked", err)
	}
	next, _, err := f.s.ClaimToolExecution(context.Background(), "restarted-worker", 30*time.Second)
	if err != nil || next != nil {
		t.Fatal("unknown job reclaimed", err)
	}
	if _, err = f.s.ReconcileToolUnsent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if effects.Load() != 1 {
		t.Fatal("reconciliation resent")
	}
}
func TestAuthorToolLateResponseAfterCancelIsFactWithoutResurrection(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in authorToolWire
		_ = json.NewDecoder(r.Body).Decode(&in)
		close(entered)
		<-release
		authorToolResponse(w, in)
	}))
	defer peer.Close()
	f, l, c := authorToolReady(t, peer, 60)
	done := make(chan error, 1)
	go func() { _, err := authorToolRuntime(t, f.s, c).Execute(context.Background(), l); done <- err }()
	<-entered
	if _, err := f.s.CancelTask(context.Background(), f.p, f.task.ID, domain.NewID("key")); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	task, err := f.s.GetTask(context.Background(), f.p, f.task.ID)
	if err != nil || task.Status != "cancelled" {
		t.Fatal("late result resurrected cancelled task", err)
	}
	view, err := f.s.GetToolExecution(context.Background(), f.p, f.task.ID, l.Execution.ID)
	if err != nil || view.Receipt == nil || view.Execution.Status != "succeeded" {
		t.Fatal("late effect observation lost", err)
	}
	var status string
	if err = f.s.Pool.QueryRow(context.Background(), "SELECT status FROM reservations WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, l.Execution.ReservationID).Scan(&status); err != nil || status != "unknown" {
		t.Fatal("late cancellation released money", err)
	}
}
func TestAuthorToolProfileWaitCannotExtendDeadline(t *testing.T) {
	var calls atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer peer.Close()
	f, l, c := authorToolReady(t, peer, 1)
	tx, err := f.s.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), "SELECT id FROM tool_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR UPDATE", c.Tool.OrganizationID, c.Tool.ID, c.Tool.Version); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	runtime := authorToolRuntime(t, f.s, c)
	go func() { _, err := runtime.Execute(context.Background(), l); done <- err }()
	time.Sleep(1100 * time.Millisecond)
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil || calls.Load() != 0 {
		t.Fatal("expired action sent after profile wait")
	}
	if _, err = f.s.ReconcileToolUnsent(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = f.s.Pool.QueryRow(context.Background(), "SELECT status FROM reservations WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, l.Execution.ReservationID).Scan(&status); err != nil || status != "released" {
		t.Fatal("proven unsent failed to release", err)
	}
}
func TestAuthorToolTrustProfileRejectsPrivateKeyAndGarbage(t *testing.T) {
	peer := httptest.NewTLSServer(http.NotFoundHandler())
	defer peer.Close()
	f := independentToolSetup(t, false)
	for _, raw := range []string{"not a certificate", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}))} {
		c := f.tool
		c.Tool.ID = domain.NewID("tool")
		c.TrustedCAPEM = raw
		if _, err := f.s.InstallToolConnection(context.Background(), c); err == nil {
			t.Fatal("invalid private trust profile accepted")
		}
	}
	if _, err := x509.ParseCertificate(peer.Certificate().Raw); err != nil {
		t.Fatal(err)
	}
}

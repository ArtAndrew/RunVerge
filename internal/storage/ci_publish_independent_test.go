// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

// The receiver is an owned TLS peer that records a synthetic comment before
// dropping its response. It has no repository credentials or outbound client.
func TestIndependentCIPublishLostReceipt(t *testing.T) {
	const schema = `{"type":"object","properties":{"destination":{"type":"string"},"body":{"type":"string"}},"required":["destination","body"],"additionalProperties":false}`
	const arguments = `{"destination":"https://ci.example.invalid/runs/4102","body":"The runner ran out of disk before tests started."}`
	var mu sync.Mutex
	var received []string
	f := independentToolSetupWithPeerSchema(t, true, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Arguments json.RawMessage `json:"arguments"`
			ActionID  string          `json:"action_id"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&request) != nil || request.ActionID == "" ||
			request.ActionID != r.Header.Get("Idempotency-Key") || string(request.Arguments) != arguments {
			t.Error("TLS peer received a different publication")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, string(request.Arguments))
		mu.Unlock()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error("cannot inject lost receipt")
			return
		}
		_ = conn.Close()
	}, schema)
	checkCount := func(want int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(received) != want {
			t.Fatalf("physical publications: got %d, want %d", len(received), want)
		}
	}
	ctx := context.Background()
	in := f.prepareInput()
	in.ArgumentsJSON = arguments
	raw, err := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), in)
	view := independentToolView(t, raw, err)
	admin := independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
	bad := independentToolDecision(view, "approve")
	bad.ExpectedTaskVersion--
	_, err = f.s.DecideToolExecution(ctx, admin, f.task.ID, view.Execution.ID, domain.NewID("key"), bad)
	independentToolDeny(t, err)
	view = f.decide(t, view, admin, "approve")
	if job, _, err := f.s.ClaimToolExecution(ctx, "ci-comment-worker", 30*time.Second); err != nil || job != nil {
		t.Fatal("approval created an executable job", err)
	}
	checkCount(0)
	view = f.dispatch(t, view)
	job, _, err := f.s.ClaimToolExecution(ctx, "ci-comment-worker", 30*time.Second)
	if err != nil || job == nil || job.Execution.ID != view.Execution.ID {
		t.Fatal("explicit dispatch did not create the expected job", err)
	}
	if result, err := f.toolRuntime(t, f.s).Execute(ctx, job); err == nil || result.Status != "unknown" {
		t.Fatal("lost receipt did not leave the effect unknown", err)
	}
	checkCount(1)
	reopened, err := Open(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	unknown, err := reopened.GetToolExecution(ctx, f.p, f.task.ID, view.Execution.ID)
	if err != nil || unknown.Execution.Status != "unknown" || unknown.Receipt != nil ||
		unknown.Task.WaitingReason != "external_action_unknown" || unknown.Task.CandidateID != f.candidate.ID {
		t.Fatal("reopened store lost the unknown effect or changed the candidate", err)
	}
	_, err = reopened.DispatchToolExecution(ctx, f.p, f.task.ID, view.Execution.ID, domain.NewID("key"), independentToolDispatch(unknown))
	independentToolDeny(t, err)
	_, _ = f.toolRuntime(t, reopened).Execute(ctx, job)
	checkCount(1)
	f.balance(t, 270, 0)

	raw, err = reopened.ReconcileToolExecution(ctx, f.p, f.task.ID, view.Execution.ID, domain.NewID("key"), domain.ReconcileToolExecution{
		ExpectedTaskVersion: unknown.Task.Version, ExpectedExecutionVersion: unknown.Execution.Version,
		IntentHash: unknown.Execution.IntentHash, Outcome: "succeeded", EffectReference: "local-comment-observation-4102",
		Result: "One comment in the local receiver journal", EvidenceDigest: Hash([]byte(arguments)),
		Reason: "The operator checked the exact receiver record after the reply was lost.",
	})
	reconciled := independentToolView(t, raw, err)
	if reconciled.Receipt == nil || reconciled.Receipt.Method != "operator_assertion" ||
		reconciled.Task.Status != "waiting_input" || reconciled.Task.CandidateID != f.candidate.ID || reconciled.Task.Result != "" {
		t.Fatal("reconciliation lost the original draft or accepted it automatically")
	}
	checkCount(1)
	f.balance(t, 270, 0)
}

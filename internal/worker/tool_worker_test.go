// SPDX-License-Identifier: Elastic-2.0
package worker_test

import (
	"agentplatform.local/platform/internal/domain"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerExplicitToolDispatchSharesBoundedPool(t *testing.T) {
	f := newModelFixture(t)
	tasks := []domain.Task{f.task(t), f.task(t)}
	f.unblock()
	stop := f.run(t, 2)
	defer stop()
	ctx := context.Background()
	await(t, func() bool {
		for i := range tasks {
			current, e := f.store.GetTask(ctx, f.principal, tasks[i].ID)
			if e != nil || current.Status != "waiting_input" {
				return false
			}
			tasks[i] = current
		}
		return true
	})
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls, active, peak atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		a := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); a > old && !peak.CompareAndSwap(old, a); old = peak.Load() {
		}
		var in map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&in)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": in["execution_id"], "action_id": in["action_id"], "intent_hash": in["intent_hash"], "outcome": "succeeded", "effect_reference": "local-worker-fixture", "result": "Synthetic external fixture result"})
	}))
	defer peer.Close()
	schema := `{"type":"object","additionalProperties":false}`
	c, err := f.store.InstallToolConnection(ctx, domain.ToolConnection{Tool: domain.ToolInfo{ID: "worker-tool", OrganizationID: f.principal.OrganizationID, Version: 1, Name: "Worker action fixture", Effect: "write", Destination: "Local TLS fixture", Provider: "worker-fixture", PriceVersion: "v1", MaxChargeMicroUSD: 120, InputSchemaJSON: schema, TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true}, Transport: "http_json", ResultContract: "agent_platform_action_v1", Endpoint: peer.URL, AllowPrivateNetwork: true, TrustedCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))})
	if err != nil {
		t.Fatal(err)
	}
	views := make([]domain.ToolExecutionView, 2)
	for i, task := range tasks {
		candidates, _, err := f.store.Candidates(ctx, f.principal, task.ID, "", 10)
		if err != nil || len(candidates) != 1 {
			t.Fatal("candidate unavailable", err)
		}
		candidate := candidates[0]
		raw, err := f.store.PrepareToolExecution(ctx, f.principal, task.ID, domain.NewID("key"), domain.PrepareToolExecution{OccurrenceID: domain.NewID("occ"), ExpectedTaskVersion: task.Version, CandidateID: candidate.ID, ArtifactID: candidate.ArtifactID, ArtifactVersion: candidate.ArtifactVersion, ContentDigest: candidate.ContentDigest, ToolID: c.Tool.ID, ToolVersion: c.Tool.Version, ArgumentsJSON: `{}`, ExpiresInSeconds: 60})
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &views[i]); err != nil {
			t.Fatal(err)
		}
		v := views[i]
		raw, err = f.store.DecideToolExecution(ctx, f.principal, task.ID, v.Execution.ID, domain.NewID("key"), domain.DecideToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, Decision: "approve", Reason: "Explicit local worker test"})
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &views[i]); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(250 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("approval implicitly dispatched")
	}
	for i, v := range views {
		_, err = f.store.DispatchToolExecution(ctx, f.principal, tasks[i].ID, v.Execution.ID, domain.NewID("key"), domain.DispatchToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, AcknowledgeCost: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(6 * time.Second):
			unblock()
			t.Fatal("bounded pool did not dispatch two tools")
		}
	}
	if peak.Load() != 2 {
		t.Fatal("tool execution did not share configured concurrency")
	}
	unblock()
	await(t, func() bool {
		for _, v := range views {
			got, e := f.store.GetToolExecution(ctx, f.principal, v.Task.ID, v.Execution.ID)
			if e != nil || got.Execution.Status != "succeeded" || got.Task.Status != "waiting_input" || got.Task.CandidateID != v.Execution.CandidateID || got.Task.ReservedMicroUSD != 1120 {
				return false
			}
		}
		return true
	})
	stop()
	if calls.Load() != 2 {
		t.Fatal("unexpected repeated external action")
	}
}

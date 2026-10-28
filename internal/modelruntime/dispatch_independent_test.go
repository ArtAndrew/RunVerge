// SPDX-License-Identifier: Elastic-2.0
package modelruntime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
)

// The barrier is after the fake store has committed pending->sent, but before
// Execute resumes. HTTP is real; the durable-store/lease model is a fixture.
// This reproduces the scheduling gap, not a PostgreSQL or process SIGSTOP test.
type independentDelayedDispatchStore struct {
	*faultStore
	prepared chan struct{}
	resume   chan struct{}
}

func (s *independentDelayedDispatchStore) PrepareModelDispatch(ctx context.Context, l *domain.Lease, id string) (domain.DispatchPermit, error) {
	permit, e := s.faultStore.PrepareModelDispatch(ctx, l, id)
	if e != nil {
		return domain.DispatchPermit{}, e
	}
	// Capture BEFORE delaying: returning must not manufacture a new permit.
	close(s.prepared)
	<-s.resume
	return permit, nil
}
func TestIndependentDelayedDispatchMustNotStartAfterExpiredAdmission(t *testing.T) {
	store := &independentDelayedDispatchStore{faultStore: newStore(), prepared: make(chan struct{}), resume: make(chan struct{})}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); success(w, "late candidate", 3, 2) }))
	defer server.Close()
	cfg := testConfig(server.URL)
	cfg.Gateway.Timeout = 100 * time.Millisecond
	runtime, e := modelruntime.New(store, cfg)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := runtime.Execute(context.Background(), testLease()); done <- e }()
	<-store.prepared
	// More than the two-second dispatch-start permit, and thirty times
	// the HTTP timeout. The request timeout alone used to restart after this pause.
	time.Sleep(3 * time.Second)
	store.mu.Lock()
	store.fence++
	store.authorized = false
	store.mu.Unlock()
	close(store.resume)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not finish")
	}
	if calls.Load() != 0 {
		t.Fatalf("provider received %d HTTP request(s) after a 3s post-admission pause and lost authority; no bounded dispatch-start permit was enforced", calls.Load())
	}
}

type independentMutatedPermitStore struct {
	*faultStore
	change func(*domain.DispatchPermit)
}

func (s *independentMutatedPermitStore) PrepareModelDispatch(ctx context.Context, l *domain.Lease, id string) (domain.DispatchPermit, error) {
	permit, e := s.faultStore.PrepareModelDispatch(ctx, l, id)
	if e == nil {
		s.change(&permit)
	}
	return permit, e
}
func TestIndependentInvalidDispatchPermitNeverReachesProvider(t *testing.T) {
	cases := map[string]func(*domain.DispatchPermit){
		"organization":            func(p *domain.DispatchPermit) { p.OrganizationID = "foreign" },
		"task":                    func(p *domain.DispatchPermit) { p.TaskID = "foreign" },
		"operation":               func(p *domain.DispatchPermit) { p.OperationID = "foreign" },
		"action":                  func(p *domain.DispatchPermit) { p.ActionID = "foreign" },
		"reservation":             func(p *domain.DispatchPermit) { p.ReservationID = "foreign" },
		"intent":                  func(p *domain.DispatchPermit) { p.IntentHash = "foreign" },
		"fence":                   func(p *domain.DispatchPermit) { p.FencingToken++ },
		"missing_issue_time":      func(p *domain.DispatchPermit) { p.IssuedAt = time.Time{} },
		"zero_duration":           func(p *domain.DispatchPermit) { p.ValidFor = 0 },
		"negative_duration":       func(p *domain.DispatchPermit) { p.ValidFor = -time.Second },
		"overlong_duration":       func(p *domain.DispatchPermit) { p.ValidFor = 3 * time.Second; p.NotAfter = p.IssuedAt.Add(p.ValidFor) },
		"inconsistent_expiration": func(p *domain.DispatchPermit) { p.NotAfter = p.NotAfter.Add(time.Second) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			store := &independentMutatedPermitStore{faultStore: newStore(), change: change}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); success(w, "invalid admission", 3, 2) }))
			defer server.Close()
			runtime, e := modelruntime.New(store, testConfig(server.URL))
			if e != nil {
				t.Fatal(e)
			}
			out, e := runtime.Execute(context.Background(), testLease())
			checkCode(t, e, "dispatch_permit_rejected", true)
			if calls.Load() != 0 || out.CostStatus != "unknown" || store.reservation.Status != "unknown" || store.artifact.ID != "" {
				t.Fatal("invalid permit dispatched or lost held uncertainty")
			}
			again, e := runtime.Execute(context.Background(), testLease())
			if e != nil || again.Status != modelruntime.AlreadyAttempted || calls.Load() != 0 {
				t.Fatal("invalid prior admission was reissued on replay")
			}
		})
	}
}

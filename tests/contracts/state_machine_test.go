// SPDX-License-Identifier: Elastic-2.0
package contracts_test

import (
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestIndependentDomainLifecycleTransitions(t *testing.T) {
	// Expected lifecycle rules are fixed independently of the generated files.
	// Contract 0.6 adds guarded external-action waiting and reconciliation edges.
	// A permitted edge does not bypass storage authorization/evidence guards.
	profiles := map[string]struct {
		states []string
		edges  [][2]string
	}{
		"task": {
			states: []string{"queued", "running", "blocked", "waiting_input", "waiting_approval", "succeeded", "failed", "cancelled"},
			edges:  [][2]string{{"queued", "running"}, {"queued", "cancelled"}, {"queued", "blocked"}, {"queued", "failed"}, {"running", "succeeded"}, {"running", "failed"}, {"running", "waiting_input"}, {"running", "waiting_approval"}, {"running", "blocked"}, {"running", "cancelled"}, {"waiting_input", "succeeded"}, {"waiting_input", "failed"}, {"waiting_input", "blocked"}, {"waiting_input", "queued"}, {"waiting_input", "cancelled"}, {"waiting_input", "waiting_approval"}, {"waiting_approval", "waiting_input"}, {"waiting_approval", "blocked"}, {"blocked", "waiting_input"}, {"waiting_approval", "queued"}, {"waiting_approval", "cancelled"}, {"blocked", "queued"}, {"blocked", "cancelled"}},
		},
		"run": {
			states: []string{"queued", "running", "blocked", "waiting_input", "waiting_approval", "succeeded", "failed", "cancelled"},
			edges:  [][2]string{{"queued", "running"}, {"queued", "cancelled"}, {"queued", "blocked"}, {"running", "succeeded"}, {"running", "failed"}, {"running", "cancelled"}, {"running", "blocked"}, {"running", "waiting_input"}, {"running", "waiting_approval"}, {"blocked", "queued"}, {"blocked", "cancelled"}, {"waiting_input", "succeeded"}, {"waiting_input", "failed"}, {"waiting_input", "queued"}, {"waiting_input", "cancelled"}, {"waiting_input", "waiting_approval"}, {"waiting_approval", "waiting_input"}, {"waiting_approval", "blocked"}, {"blocked", "waiting_input"}, {"waiting_approval", "queued"}, {"waiting_approval", "cancelled"}},
		},
		"operation": {
			states: []string{"queued", "leased", "blocked", "succeeded", "failed", "cancelled"},
			edges:  [][2]string{{"queued", "leased"}, {"queued", "cancelled"}, {"queued", "blocked"}, {"leased", "leased"}, {"leased", "succeeded"}, {"leased", "failed"}, {"leased", "cancelled"}, {"leased", "blocked"}, {"blocked", "succeeded"}, {"blocked", "failed"}, {"blocked", "queued"}, {"blocked", "cancelled"}},
		},
		"tool_execution": {
			states: []string{"awaiting_approval", "approved", "queued", "leased", "sent", "unknown", "succeeded", "rejected", "denied", "expired", "cancelled"},
			edges:  [][2]string{{"awaiting_approval", "approved"}, {"awaiting_approval", "denied"}, {"awaiting_approval", "expired"}, {"awaiting_approval", "cancelled"}, {"approved", "queued"}, {"approved", "expired"}, {"approved", "cancelled"}, {"queued", "leased"}, {"queued", "expired"}, {"queued", "cancelled"}, {"queued", "rejected"}, {"leased", "leased"}, {"leased", "sent"}, {"leased", "expired"}, {"leased", "cancelled"}, {"leased", "rejected"}, {"sent", "unknown"}, {"sent", "succeeded"}, {"sent", "rejected"}, {"unknown", "succeeded"}, {"unknown", "rejected"}},
		},
		"action": {
			states: []string{"pending", "sent", "unknown", "rejected", "confirmed"},
			edges:  [][2]string{{"pending", "sent"}, {"pending", "rejected"}, {"pending", "confirmed"}, {"sent", "confirmed"}, {"sent", "unknown"}, {"sent", "rejected"}, {"unknown", "confirmed"}, {"unknown", "rejected"}, {"rejected", "confirmed"}},
		},
		"reservation": {
			states: []string{"reserved", "unknown", "settled", "released"},
			edges:  [][2]string{{"reserved", "unknown"}, {"reserved", "settled"}, {"reserved", "released"}, {"unknown", "settled"}, {"unknown", "released"}, {"settled", "settled"}, {"released", "settled"}},
		},
		"approval": {
			states: []string{"pending", "approved", "denied", "expired"},
			edges:  [][2]string{{"pending", "approved"}, {"pending", "denied"}, {"pending", "expired"}},
		},
	}
	for kind, profile := range profiles {
		t.Run(kind, func(t *testing.T) {
			allowed := map[[2]string]bool{}
			for _, edge := range profile.edges {
				allowed[edge] = true
			}
			states := append(profile.states, "unrecognized")
			for _, before := range states {
				for _, after := range states {
					if got := domain.ValidTransition(kind, before, after); got != allowed[[2]string{before, after}] {
						t.Errorf("%s transition %s -> %s: got %v", kind, before, after, got)
					}
				}
			}
		})
	}
	if domain.ValidTransition("unrecognized", "queued", "running") {
		t.Fatal("unknown machine permits execution")
	}
}

func TestIndependentDomainPermissionDenials(t *testing.T) {
	for _, check := range [][2]string{{"viewer", "tasks.create"}, {"viewer", "agents.write"}, {"member", "tasks.cancel_any"}, {"admin", "organization.manage"}, {"owner", "costs.settle"}, {"owner", "invented.permission"}, {"invented_role", "tasks.read"}} {
		if domain.HasPermission(check[0], check[1]) {
			t.Errorf("unexpected permission %s for %s", check[1], check[0])
		}
	}
	if !domain.HasPermission("owner", "organization.manage") || !domain.HasPermission("viewer", "tasks.read") {
		t.Fatal("documented read/owner authority missing")
	}
}

// SPDX-License-Identifier: Elastic-2.0
package domain

import "testing"

func TestAllTaskTransitions(t *testing.T) {
	// These added edges are guarded by exact tool approval/observation commands;
	// the integration suite rejects generic resumes, stale snapshots and revoked authority.
	allowed := map[string]bool{"waiting_input/waiting_approval": true, "waiting_approval/waiting_input": true, "waiting_approval/blocked": true, "blocked/waiting_input": true, "queued/running": true, "queued/cancelled": true, "queued/blocked": true, "queued/failed": true, "running/succeeded": true, "running/failed": true, "running/waiting_input": true, "running/waiting_approval": true, "running/blocked": true, "running/cancelled": true, "waiting_input/queued": true, "waiting_input/cancelled": true, "waiting_input/succeeded": true, "waiting_input/failed": true, "waiting_input/blocked": true, "waiting_approval/queued": true, "waiting_approval/cancelled": true, "blocked/queued": true, "blocked/cancelled": true}
	states := []string{"queued", "running", "succeeded", "failed", "cancelled", "waiting_input", "waiting_approval", "blocked", "invented"}
	for _, a := range states {
		for _, b := range states {
			if CanTransition(a, b) != allowed[a+"/"+b] {
				t.Errorf("transition %s/%s", a, b)
			}
		}
	}
}
func TestIDsOpaqueAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID("task")
		if len(id) != 37 || seen[id] {
			t.Fatalf("bad id: %s", id)
		}
		seen[id] = true
	}
}

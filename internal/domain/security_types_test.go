// SPDX-License-Identifier: Elastic-2.0
package domain

import "testing"

func TestIdentityKindCannotGrantHumanAuthorityToServices(t *testing.T) {
	for _, kind := range []string{"", "robot", "HUMAN", PrincipalService} {
		for _, role := range []string{"owner", "admin", "unknown"} {
			if got := EffectivePermissions(Principal{Kind: kind, Role: role}); len(got) != 0 {
				t.Fatalf("unrecognized or invalid kind/role gained permissions: %q %q", kind, role)
			}
		}
	}
	service := Principal{Kind: PrincipalService, Role: "member"}
	for _, permission := range []string{"tasks.review_own", "tasks.review_any", "identity.manage", "organization.manage", "actions.approve_any", "actions.reconcile_any", "tasks.automate_any", "not.a.permission"} {
		if PrincipalHasPermission(service, permission) {
			t.Fatalf("service gained human/operator authority %s", permission)
		}
	}
	for _, permission := range []string{"tasks.read", "tasks.create", "tasks.continue_own", "tasks.cancel_own", "actions.prepare_own", "actions.dispatch_own", "materials.write_own", "agents.write", "costs.read"} {
		if !PrincipalHasPermission(service, permission) {
			t.Fatalf("documented service workflow unavailable: %s", permission)
		}
	}
	viewer := Principal{Kind: PrincipalService, Role: "viewer"}
	for _, permission := range []string{"tasks.create", "agents.write", "actions.dispatch_own", "tasks.review_own"} {
		if PrincipalHasPermission(viewer, permission) {
			t.Fatalf("viewer service gained write authority: %s", permission)
		}
	}
	if !PrincipalHasPermission(Principal{Kind: PrincipalHuman, Role: "member"}, "tasks.review_own") {
		t.Fatal("human review authority was removed")
	}
	copy := EffectivePermissions(service)
	copy[0] = "identity.manage"
	if PrincipalHasPermission(service, "identity.manage") {
		t.Fatal("caller could mutate canonical permissions")
	}
}

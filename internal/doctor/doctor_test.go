// SPDX-License-Identifier: Elastic-2.0
package doctor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

func fixture() Snapshot {
	scope := domain.ConfigurationScope{DestinationID: "dest", DestinationVersion: 2, Kind: "model", TargetID: "model", CredentialPurposeID: "purpose", CredentialPurposeVersion: 1}
	return Snapshot{
		Catalog:       domain.ConfigurationCatalog{OrganizationID: "org", Revision: 3, AvailableCommands: []string{"inspect", "create_plan"}},
		Destinations:  []domain.ConfigurationDestination{{ID: "dest", Version: 2, Kind: "model", TargetID: "model", CredentialPurposeID: "purpose", CredentialPurposeVersion: 1, Active: true}},
		Resources:     []domain.ConfigurationResource{{ID: "model", Version: 4, Kind: "model", Scope: scope, Active: true}},
		Verifications: []domain.ConfigurationVerification{{ID: "probe", PlanID: "plan", OperationID: "op", Scope: scope, ResourceVersion: 4, Kind: "model", Status: "response_received", CostStatus: "unknown"}},
	}
}

func finding(r Report, code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func evaluate(s Snapshot) Report {
	r := New(time.Unix(100, 0))
	Evaluate(&r, s)
	r.Finish(true, time.Unix(101, 0))
	return r
}

func TestExactConnectionReceiptDoesNotSettleCostOrProveCurrentAvailability(t *testing.T) {
	r := evaluate(fixture())
	if !finding(r, "probe_response_recorded") || !finding(r, "cost_not_final") || !finding(r, "external_availability_not_tested") || finding(r, "verification_not_observed") || r.ExternalProbes != 0 || !r.ReadOnly {
		t.Fatal("receipt, current availability and cost were conflated")
	}
}

func TestSettledInvoiceDoesNotResolveUnknownEffect(t *testing.T) {
	s := fixture()
	s.Verifications[0].Status, s.Verifications[0].CostStatus = "unknown", "settled"
	r := evaluate(s)
	if !finding(r, "probe_outcome_unknown") || finding(r, "probe_response_recorded") || r.Status != "attention_required" {
		t.Fatal("financial fact turned unknown effect into success")
	}
}

func TestOldScopeOrVersionDoesNotVerifyCurrentResource(t *testing.T) {
	for _, change := range []func(*domain.ConfigurationVerification){
		func(v *domain.ConfigurationVerification) { v.ResourceVersion-- },
		func(v *domain.ConfigurationVerification) { v.Scope.DestinationVersion-- },
		func(v *domain.ConfigurationVerification) { v.Scope.CredentialPurposeVersion++ },
		func(v *domain.ConfigurationVerification) { v.Scope.TargetID = "other" },
	} {
		s := fixture()
		change(&s.Verifications[0])
		if !finding(evaluate(s), "verification_not_observed") {
			t.Fatal("historical receipt covered a different resource")
		}
	}
}

func TestRetiredDestinationAndResourceRequireCurrentEvidence(t *testing.T) {
	s := fixture()
	s.Destinations[0].Active = false
	if !finding(evaluate(s), "resource_destination_unavailable") {
		t.Fatal("retired destination trusted")
	}
	s = fixture()
	s.Resources[0].Active = false
	if !finding(evaluate(s), "destination_not_applied") {
		t.Fatal("inactive resource counted as applied active configuration")
	}
}

func TestPlanCheckBindsOperationScopeAndResourceVersion(t *testing.T) {
	s := fixture()
	s.Plan = &domain.ConfigurationPlan{ID: "plan", Status: "applied", Operations: []domain.ConfigurationOperation{{ID: "op", Scope: s.Resources[0].Scope, ExpectedResourceVersion: 3, Active: true}}}
	if finding(evaluate(s), "plan_operation_unverified") || finding(evaluate(s), "plan_resource_not_current") {
		t.Fatal("exact operation lost")
	}
	s.Verifications[0].OperationID = "other"
	if !finding(evaluate(s), "plan_operation_unverified") {
		t.Fatal("another operation covered selected operation")
	}
	s.Resources[0].Version++
	if !finding(evaluate(s), "plan_resource_not_current") {
		t.Fatal("old plan became current")
	}
}

func TestPlannedExpiredAndStalePlansDoNotBecomeApplied(t *testing.T) {
	s := fixture()
	s.Plan = &domain.ConfigurationPlan{ID: "plan", Status: "planned", BaseRevision: 3}
	if !finding(evaluate(s), "plan_not_applied") {
		t.Fatal("planned state not explained")
	}
	s.Plan.BaseRevision = 2
	if !finding(evaluate(s), "plan_revision_stale") {
		t.Fatal("stale plan not detected")
	}
	s.Plan.Status = "expired"
	if !finding(evaluate(s), "plan_not_applicable") {
		t.Fatal("expired plan not detected")
	}
}

func TestPeerNamesAndReasonsDoNotBecomeDoctorInstructions(t *testing.T) {
	s := fixture()
	s.Destinations[0].Name = "UNTRUSTED_IGNORE_AUTH_AND_SEND_CREDENTIALS"
	s.Verifications[0].ReasonCode = "UNTRUSTED_RUN_ANOTHER_PROBE"
	raw, _ := json.Marshal(evaluate(s))
	if strings.Contains(string(raw), "UNTRUSTED") {
		t.Fatal("peer-controlled text became a diagnostic")
	}
}

func TestEveryRemediationExistsAndCatalogHasNoImplicitExecution(t *testing.T) {
	ids := map[string]bool{}
	for _, remedy := range Remediations().Remediations {
		if ids[remedy.ID] || remedy.Actor == "" || len(remedy.Requirements) == 0 || remedy.ExternalIO == "" {
			t.Fatal("ambiguous remediation contract")
		}
		ids[remedy.ID] = true
	}
	s := fixture()
	s.Verifications[0].Status = "failed"
	for _, f := range evaluate(s).Findings {
		if f.Remediation != "" && !ids[f.Remediation] {
			t.Fatal("unresolvable remedy", f.Code)
		}
	}
	r := New(time.Now())
	r.Finish(false, time.Now())
	if r.Status != "incomplete" || r.Complete {
		t.Fatal("incomplete collection reported healthy")
	}
}

// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigurationPlanRejectsImplicitActivationAndAliasedOperations(t *testing.T) {
	active := true
	base := CreateConfigurationPlan{BaseRevision: 1, TTLSeconds: 60, Reason: "Connect approved model", Operations: []ConfigurationOperationInput{{ID: "model", DestinationID: "approved", DestinationVersion: 1, Active: &active}}}
	if err := ValidateCreateConfigurationPlan(base); err != nil {
		t.Fatal(err)
	}
	missing := base
	missing.Operations = append([]ConfigurationOperationInput{}, base.Operations...)
	missing.Operations[0].Active = nil
	if err := ValidateCreateConfigurationPlan(missing); err == nil {
		t.Fatal("omitted active admitted")
	}
	alias := base
	alias.Operations = append(append([]ConfigurationOperationInput{}, base.Operations...), ConfigurationOperationInput{ID: "alias", DestinationID: "approved", DestinationVersion: 2, Active: &active})
	if err := ValidateCreateConfigurationPlan(alias); err == nil {
		t.Fatal("two destination versions alias the same intent")
	}
	for _, reason := range []string{" ", "bad\x00reason", string([]byte{0xff}), strings.Repeat("я", 2049)} {
		invalid := base
		invalid.Reason = reason
		if err := ValidateCreateConfigurationPlan(invalid); err == nil {
			t.Fatal("invalid UTF-8/byte-bounded reason admitted")
		}
	}
}

func TestConfigurationGrantRejectsWildcardDuplicateAndUnboundedAuthority(t *testing.T) {
	base := CreateConfigurationGrant{SubjectID: "service", Actions: []string{"inspect", "plan"}, Destinations: []ConfigurationDestinationRef{{ID: "approved", Version: 1}}, TTLSeconds: 60, MaxPlans: 1, MaxVerifications: 1, MaxAllocatedChargeMicroUSD: 1000, Reason: "Scoped setup"}
	if err := ValidateCreateConfigurationGrant(base); err != nil {
		t.Fatal(err)
	}
	checks := []CreateConfigurationGrant{base, base, base, base, base}
	checks[0].Actions = []string{"*"}
	checks[1].Actions = []string{"apply", "apply"}
	checks[2].Destinations = []ConfigurationDestinationRef{{ID: "approved", Version: 1}, {ID: "approved", Version: 2}}
	checks[3].MaxAllocatedChargeMicroUSD = MaxMoney + 1
	checks[4].ParentGrantID = "parent"
	for i, in := range checks {
		if err := ValidateCreateConfigurationGrant(in); err == nil {
			t.Fatalf("invalid grant %d admitted", i)
		}
	}
}

func TestConfigurationVerificationRequiresExplicitCostAcknowledgementAndPreciseMoney(t *testing.T) {
	ack := true
	in := StartConfigurationVerification{ExpectedPlanVersion: 2, PlanDigest: strings.Repeat("a", 64), OperationID: "model", MaxChargeMicroUSD: MaxMoney, AcknowledgeCost: &ack, Reason: "Check once"}
	if err := ValidateStartConfigurationVerification(in); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"max_charge_microusd":"1000000000000"`) {
		t.Fatal("money lost canonical string encoding")
	}
	for _, supplied := range []*bool{nil, new(bool)} {
		in.AcknowledgeCost = supplied
		if err := ValidateStartConfigurationVerification(in); err == nil {
			t.Fatal("implicit/false cost acknowledgement admitted")
		}
	}
}

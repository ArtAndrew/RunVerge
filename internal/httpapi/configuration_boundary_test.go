// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestConfigurationDecodeRejectsPrivateFieldsImplicitDefaultsAndImpreciseMoney(t *testing.T) {
	decode := func(body string, out any) bool {
		r := httptest.NewRequest("POST", "/api/v1/configuration/plans", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		return (&Server{}).decode(httptest.NewRecorder(), r, out)
	}
	valid := `{"base_revision":"1","ttl_seconds":60,"operations":[{"id":"one","destination_id":"approved","destination_version":1,"expected_resource_version":0,"active":true}],"reason":"Approved connection"}`
	if !decode(valid, &domain.CreateConfigurationPlan{}) {
		t.Fatal("exact explicit plan rejected")
	}
	for _, invalid := range []string{
		strings.Replace(valid, `"base_revision":"1"`, `"base_revision":"01"`, 1),
		strings.Replace(valid, `"base_revision":"1"`, `"base_revision":"+1"`, 1),
		strings.Replace(valid, `"expected_resource_version":0,`, "", 1),
		strings.Replace(valid, `"active":true`, `"active":true,"endpoint":"https://attacker.example"`, 1),
		strings.Replace(valid, `"active":true`, `"active":true,"credential_environment":"PLATFORM_MODEL_KEY_OTHER"`, 1),
	} {
		if decode(invalid, &domain.CreateConfigurationPlan{}) {
			t.Fatal("unsafe/ambiguous plan decoded", invalid)
		}
	}
	verify := `{"expected_plan_version":2,"plan_digest":"` + strings.Repeat("a", 64) + `","operation_id":"one","max_charge_microusd":"0","acknowledge_cost":true,"reason":"Explicit check"}`
	if !decode(verify, &domain.StartConfigurationVerification{}) {
		t.Fatal("zero-cost explicit MCP check rejected")
	}
	for _, invalid := range []string{
		strings.Replace(verify, `"max_charge_microusd":"0",`, "", 1),
		strings.Replace(verify, `"max_charge_microusd":"0"`, `"max_charge_microusd":"-0"`, 1),
		strings.Replace(verify, `"acknowledge_cost":true,`, "", 1),
	} {
		if decode(invalid, &domain.StartConfigurationVerification{}) {
			t.Fatal("implicit cost command decoded")
		}
	}
}

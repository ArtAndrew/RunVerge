// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestAutomationWireKeepsExactMoneyAndRejectsAmbiguousAuthority(t *testing.T) {
	s := Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	valid := `{"expected_task_version":2,"mode":"read_only","tool_pins":[{"tool_id":"lookup","tool_version":1,"tool_profile_digest":"` + strings.Repeat("a", 64) + `"}],"max_model_turns":2,"max_tool_executions":3,"max_charge_microusd":"1000000000000","expires_in_seconds":60,"acknowledge_cost":true,"reason":"Explicit bounded delegation"}`
	decode := func(raw string) (bool, domain.CreateTaskAutomation) {
		r := httptest.NewRequest("POST", "/", strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		var v domain.CreateTaskAutomation
		ok := s.decode(w, r, &v)
		if !ok && w.Code != 400 {
			t.Fatalf("wronginvalid status:%d", w.Code)
		}
		return ok, v
	}
	if ok, v := decode(valid); !ok || v.MaxChargeMicroUSD != 1_000_000_000_000 {
		t.Fatal("valid exact maximum rejected")
	}
	for _, raw := range []string{`1000`, `"01"`, `"+1"`, `"1e3"`, `"1.0"`, `" 1"`, `"1 "`, `"-0"`, `"9223372036854775808"`, `true`, `null`} {
		t.Run("money_"+raw, func(t *testing.T) {
			if ok, _ := decode(strings.Replace(valid, `"1000000000000"`, raw, 1)); ok {
				t.Fatal("noncanonical money accepted")
			}
		})
	}
	for _, raw := range []string{
		strings.Replace(valid, `"mode":"read_only"`, `"mode":"read_only","mode":"prepare_and_continue"`, 1),
		strings.Replace(valid, `"tool_version":1`, `"Tool_version":1`, 1),
		strings.Replace(valid, `"tool_version":1`, `"tool_version":1,"grant_all":true`, 1),
		strings.Replace(valid, `"expected_task_version":2`, `"principal_id":"someone","expected_task_version":2`, 1),
		strings.Replace(valid, `"expected_task_version":2`, `"expected_task_version":2,"automation_grant_id":"forged"`, 1),
	} {
		if ok, _ := decode(raw); ok {
			t.Fatal("ambiguous or forged authority accepted")
		}
	}
	// The approved ceiling retains decimal-string encoding on responses too.
	body, err := json.Marshal(domain.AutomationUsage{AllocatedMicroUSD: 1_000_000_000_000})
	if err != nil || !strings.Contains(string(body), `"allocated_microusd":"1000000000000"`) {
		t.Fatal("money response lostexactencoding")
	}
}

// SPDX-License-Identifier: Elastic-2.0
package modelruntime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
)

func authorLocalConfig(endpoint string) modelruntime.Config {
	c := testConfig(endpoint)
	c.Price.AccountingMode = domain.ModelAccountingLocalUnmetered
	c.Price.MaxChargeMicroUSD = 0
	c.Price.InputMicroUSDPerMillion = 0
	c.Price.OutputMicroUSDPerMillion = 0
	c.Price.FixedMicroUSD = 0
	return c
}

func TestAuthorLocalUnmeteredRequiresExplicitZeroRatesAndCanonicalLoopback(t *testing.T) {
	base := "http://127.0.0.1:12345/v1/chat/completions"
	if _, err := modelruntime.New(newStore(), authorLocalConfig(base)); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"https://api.example.com/v1/chat/completions", "http://localhost:12345/v1/chat/completions", "http://127.0.0.1:012345/v1/chat/completions", base + "?", base + "#", "http://user@127.0.0.1:12345/v1/chat/completions", "http://127.0.0.1:12345/other"} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := modelruntime.New(newStore(), authorLocalConfig(endpoint)); err == nil {
				t.Fatal("noncanonical/cloud endpoint admitted")
			}
		})
	}
	for name, mutate := range map[string]func(*modelruntime.Config){
		"legacy_zero":       func(c *modelruntime.Config) { c.Price.AccountingMode = "" },
		"metered_zero":      func(c *modelruntime.Config) { c.Price.AccountingMode = domain.ModelAccountingMetered },
		"unknown_mode":      func(c *modelruntime.Config) { c.Price.AccountingMode = "free" },
		"charge":            func(c *modelruntime.Config) { c.Price.MaxChargeMicroUSD = 1 },
		"input_rate":        func(c *modelruntime.Config) { c.Price.InputMicroUSDPerMillion = 1 },
		"output_rate":       func(c *modelruntime.Config) { c.Price.OutputMicroUSDPerMillion = 1 },
		"fixed_rate":        func(c *modelruntime.Config) { c.Price.FixedMicroUSD = 1 },
		"loopback_disabled": func(c *modelruntime.Config) { c.Gateway.AllowInsecureLoopback = false },
	} {
		t.Run(name, func(t *testing.T) {
			c := authorLocalConfig(base)
			mutate(&c)
			if _, err := modelruntime.New(newStore(), c); err == nil {
				t.Fatal("invalid unmetered profile admitted")
			}
		})
	}
}

func TestAuthorLocalZeroAttemptRetainsUnknownIdentityAndCannotResend(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "response", true: "lost_reply"}[lost], func(t *testing.T) {
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if lost {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Unverified local draft"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
			}))
			defer peer.Close()
			store := newStore()
			runtime := newRuntime(t, store, authorLocalConfig(peer.URL+"/v1/chat/completions"))
			out, err := runtime.Execute(context.Background(), testLease())
			if lost && err == nil || !lost && err != nil || calls.Load() != 1 || store.intent.AmountMicroUSD != 0 || store.reservation.Status != "unknown" || store.action != "unknown" {
				t.Fatal("zero tariff lost ordinary dispatch/accounting guards", out, err)
			}
			if again, retryErr := runtime.Execute(context.Background(), testLease()); retryErr != nil || again.Status != modelruntime.AlreadyAttempted || calls.Load() != 1 {
				t.Fatal("zero tariff resent old occurrence")
			}
		})
	}
}

func TestAuthorLegacyPriceJSONDoesNotAcquireAccountingMode(t *testing.T) {
	profile := testConfig("http://127.0.0.1:12345/v1/chat/completions").Price
	raw, err := json.Marshal(profile)
	if err != nil || strings.Contains(string(raw), "accounting_mode") {
		t.Fatal("legacy immutable intent acquired new field", err)
	}
	if domain.EffectiveModelAccountingMode(profile.AccountingMode) != domain.ModelAccountingMetered || profile.AccountingMode != "" {
		t.Fatal("display normalization mutated legacy mode")
	}
}

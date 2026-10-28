// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestOpenRouterLimitsDoNotCopyPrivateProviderMessages(t *testing.T) {
	private := `{"error":{"message":"private-key-and-body-sentinel","metadata":{"raw":"private-key-and-body-sentinel","limit_source":"upstream_provider_shared_pool"}}}`
	if code := openRouterLimitCode(strings.NewReader(private)); code != "provider_shared_pool_limited" {
		t.Fatal("shared provider pool was not distinguished")
	}
	for _, body := range []string{
		strings.Replace(private, "upstream_provider_shared_pool", "private-key-and-body-sentinel", 1),
		strings.Replace(private, `"limit_source":`, `"limit_source":"unexpected","limit_source":`, 1),
		private + `{}`, strings.Repeat("x", (16<<10)+1),
	} {
		if code := openRouterLimitCode(strings.NewReader(body)); code != "" {
			t.Fatal("ambiguous or private diagnostic escaped")
		}
	}
}

func TestOpenRouterEmptyLengthCompletionKeepsUncertainOutcome(t *testing.T) {
	body := []byte(`{"id":"gen-test","model":"example/fixed-model:free","choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","reasoning":"private-reasoning-sentinel"}}],"usage":{"prompt_tokens":10,"completion_tokens":256,"total_tokens":266}}`)
	_, err := decodeProfileResult(body, 256, nil, nil, "example/fixed-model:free")
	assertOutcome(t, err, "output_budget_exhausted", true)
}

func routerConfig() Config {
	c := fixtureConfig(OpenRouterEndpoint)
	c.WireProfile, c.Model, c.AllowInsecureLoopback = domain.ModelWireOpenRouterTextV1, "example/fixed-model", false
	c.OpenRouterPrice = OpenRouterPrice{InputMicroUSDPerMillion: 123456, OutputMicroUSDPerMillion: 2345678, FixedMicroUSD: 9}
	return c
}

func TestOpenRouterWirePinsRoutingAndExactPrices(t *testing.T) {
	g, err := New(routerConfig())
	if err != nil {
		t.Fatal(err)
	}
	body, err := g.BuildRequest(fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var wire map[string]any
	if err := decoder.Decode(&wire); err != nil {
		t.Fatal(err)
	}
	provider := wire["provider"].(map[string]any)
	price := provider["max_price"].(map[string]any)
	if price["prompt"] != json.Number("0.123456") || price["completion"] != json.Number("2.345678") || price["request"] != json.Number("0.000009") {
		t.Fatal("provider rate caps changed units or precision")
	}
	if provider["allow_fallbacks"] != false || provider["require_parameters"] != true || provider["data_collection"] != "deny" || wire["stream"] != false || wire["model"] != "example/fixed-model" {
		t.Fatal("routing policy was not frozen")
	}
	if _, exists := wire["n"]; exists {
		t.Fatal("unsupported n parameter sent")
	}
	if len(wire["plugins"].([]any)) != 0 || strings.Contains(string(body), testKey) {
		t.Fatal("unexpected plugin or credential")
	}
}

func TestOpenRouterProfileRejectsEndpointRouterAndTools(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Endpoint = "https://other.invalid/api/v1/chat/completions" },
		func(c *Config) { c.Model = "openrouter/auto" },
		func(c *Config) { c.Model = "example/model:online" },
		func(c *Config) { c.AllowInsecureLoopback = true },
		func(c *Config) { c.WireProfile = "unknown" },
		func(c *Config) { c.OpenRouterPrice.FixedMicroUSD = -1 },
		func(c *Config) { c.Model += ":free" },
		func(c *Config) { c.OpenRouterDataCollection = "unknown" },
	} {
		c := routerConfig()
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Fatal("unsafe provider profile accepted")
		}
	}
	g, _ := New(routerConfig())
	input := fixtureRequest()
	input.Messages = []domain.ModelMessage{{Role: "system", Content: input.Instructions}, {Role: "user", Content: input.Goal}}
	if _, err := g.BuildRequest(input); err == nil {
		t.Fatal("conversation accepted by text-only profile")
	}
}

func TestOpenRouterReplyRequiresExactModelAndGenerationIdentity(t *testing.T) {
	base := `{"id":"gen-test","model":"example/fixed-model:free","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"draft"}}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6,"cost":0}}`
	result, err := decodeProfileResult([]byte(base), 16, nil, nil, "example/fixed-model:free")
	if err != nil || result.ProviderRequestID != "gen-test" || result.Text != "draft" {
		t.Fatal("valid fixed-model response rejected", err)
	}
	for _, body := range []string{
		strings.Replace(base, "example/fixed-model:free", "other/model", 1),
		strings.Replace(base, `"id":"gen-test",`, "", 1),
		strings.Replace(base, `"cost":0`, `"cost":0,"completion_tokens":3`, 1),
		strings.Replace(base, `"choices":`, `"error":{"code":500},"choices":`, 1),
	} {
		_, err := decodeProfileResult([]byte(body), 16, nil, nil, "example/fixed-model:free")
		var outcome *OutcomeError
		if !errors.As(err, &outcome) || !outcome.Unknown || outcome.DefinitiveRejection {
			t.Fatal("ambiguous provider result became safe to retry")
		}
	}
}

func TestOpenRouterFreeProfileUsesZeroCapsAndExplicitDataPolicy(t *testing.T) {
	c := routerConfig()
	c.Model += ":free"
	c.OpenRouterPrice = OpenRouterPrice{}
	c.OpenRouterDataCollection = "allow"
	g, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	body, err := g.BuildRequest(fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	provider := wire["provider"].(map[string]any)
	if provider["data_collection"] != "allow" {
		t.Fatal("explicit data policy lost")
	}
	for _, value := range provider["max_price"].(map[string]any) {
		if value != float64(0) {
			t.Fatal("free profile has nonzero price cap")
		}
	}
}

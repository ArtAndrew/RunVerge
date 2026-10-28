// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"agentplatform.local/platform/internal/domain"
)

const OpenRouterEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// These are trusted rate caps, not a provider invoice. Runtime derives them
// from the same immutable price profile used by reservation and intent hashing.
type OpenRouterPrice struct {
	InputMicroUSDPerMillion  int64
	OutputMicroUSDPerMillion int64
	FixedMicroUSD            int64
}

var fixedRouterModel = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*(?::free)?$`)

func validateWireProfile(config Config) error {
	switch config.WireProfile {
	case "":
		if config.OpenRouterPrice != (OpenRouterPrice{}) || config.OpenRouterDataCollection != "" {
			return rejected("unexpected_provider_prices")
		}
		return nil
	case domain.ModelWireOpenRouterTextV1:
		if config.OpenRouterDataCollection != "" && config.OpenRouterDataCollection != "deny" && config.OpenRouterDataCollection != "allow" {
			return rejected("invalid_openrouter_data_policy")
		}
		if config.Endpoint != OpenRouterEndpoint || config.AllowInsecureLoopback || config.TokenLimitField != "max_tokens" ||
			!fixedRouterModel.MatchString(config.Model) || strings.HasPrefix(config.Model, "openrouter/") {
			return rejected("invalid_openrouter_profile")
		}
		for _, value := range []int64{config.OpenRouterPrice.InputMicroUSDPerMillion, config.OpenRouterPrice.OutputMicroUSDPerMillion, config.OpenRouterPrice.FixedMicroUSD} {
			if value < 0 || value > domain.MaxMoney {
				return rejected("invalid_openrouter_price")
			}
		}
		if strings.HasSuffix(config.Model, ":free") && config.OpenRouterPrice != (OpenRouterPrice{}) {
			return rejected("invalid_free_model_prices")
		}
		return nil
	default:
		return rejected("unsupported_wire_profile")
	}
}

// A micro-USD rate per million tokens becomes USD per million tokens on the
// provider wire. Fixed charges use USD per request. No float conversion occurs.
func exactUSD(microUSD int64) json.Number {
	return json.Number(fmt.Sprintf("%d.%06d", microUSD/1_000_000, microUSD%1_000_000))
}

func (g *Gateway) applyWireProfile(payload map[string]any, input Request) error {
	if g.config.WireProfile == "" {
		return nil
	}
	if len(input.Tools) != 0 || len(input.Messages) != 0 {
		return rejected("openrouter_text_only")
	}
	delete(payload, "n") // OpenRouter's public request schema has no n parameter.
	payload["plugins"] = []any{}
	dataPolicy := g.config.OpenRouterDataCollection
	if dataPolicy == "" {
		dataPolicy = "deny"
	}
	payload["provider"] = map[string]any{
		"allow_fallbacks":    false,
		"require_parameters": true,
		"data_collection":    dataPolicy,
		"max_price": map[string]any{
			"prompt":     exactUSD(g.config.OpenRouterPrice.InputMicroUSDPerMillion),
			"completion": exactUSD(g.config.OpenRouterPrice.OutputMicroUSDPerMillion),
			"request":    exactUSD(g.config.OpenRouterPrice.FixedMicroUSD),
		},
	}
	return nil
}

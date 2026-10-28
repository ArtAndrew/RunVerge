// SPDX-License-Identifier: Elastic-2.0
package modelruntime

import (
	"math/big"
	"strings"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
)

func validText(s string, maximum int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= maximum && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func (p PriceProfile) validate(model string, outputLimit int) error {
	if !validText(p.ProviderAccount, 128) || !validText(p.Version, 128) || !validText(p.Model, 256) || p.Model != model ||
		!validText(p.BoundAssumptions, 4096) || p.MaxChargeMicroUSD < 0 || p.MaxChargeMicroUSD > domain.MaxMoney ||
		p.MaxInputTokens <= 0 || p.MaxInputTokens > 1_000_000_000 ||
		p.InputMicroUSDPerMillion < 0 || p.InputMicroUSDPerMillion > domain.MaxMoney ||
		p.OutputMicroUSDPerMillion < 0 || p.OutputMicroUSDPerMillion > domain.MaxMoney ||
		p.FixedMicroUSD < 0 || p.FixedMicroUSD > domain.MaxMoney {
		return runtimeError("invalid_price_profile", false)
	}
	switch domain.EffectiveModelAccountingMode(p.AccountingMode) {
	case domain.ModelAccountingMetered:
		if p.MaxChargeMicroUSD == 0 {
			return runtimeError("invalid_price_profile", false)
		}
	case domain.ModelAccountingLocalUnmetered:
		if p.MaxChargeMicroUSD != 0 || p.InputMicroUSDPerMillion != 0 || p.OutputMicroUSDPerMillion != 0 || p.FixedMicroUSD != 0 {
			return runtimeError("invalid_price_profile", false)
		}
	default:
		return runtimeError("invalid_price_profile", false)
	}
	maximum, err := p.Estimate(p.MaxInputTokens, int64(outputLimit))
	if err != nil || maximum > p.MaxChargeMicroUSD {
		return runtimeError("price_ceiling_too_small", false)
	}
	return nil
}

// Estimate returns ceil(input*inputRate/1e6) + ceil(output*outputRate/1e6)
// + fixed fee. Each component rounds up separately. Exact big-integer arithmetic
// avoids intermediate overflow; only a representable int64 result is returned.
// This estimate cannot settle a reservation, even when a response is successful.
func (p PriceProfile) Estimate(inputTokens, outputTokens int64) (int64, error) {
	if inputTokens < 0 || outputTokens < 0 || p.InputMicroUSDPerMillion < 0 || p.OutputMicroUSDPerMillion < 0 || p.FixedMicroUSD < 0 {
		return 0, runtimeError("invalid_price_observation", false)
	}
	component := func(tokens, rate int64) *big.Int {
		product := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(rate))
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(product, big.NewInt(1_000_000), rem)
		if rem.Sign() != 0 {
			q.Add(q, big.NewInt(1))
		}
		return q
	}
	total := component(inputTokens, p.InputMicroUSDPerMillion)
	total.Add(total, component(outputTokens, p.OutputMicroUSDPerMillion))
	total.Add(total, big.NewInt(p.FixedMicroUSD))
	if !total.IsInt64() {
		return 0, runtimeError("price_observation_overflow", false)
	}
	return total.Int64(), nil
}

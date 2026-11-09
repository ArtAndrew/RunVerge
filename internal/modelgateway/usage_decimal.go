// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"errors"
	"math/big"
	"regexp"
	"strconv"
)

var usageDecimal = regexp.MustCompile(`^(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

// usageExactMicroUSD interprets the documented USD-based credit unit exactly.
// It does not round sub-micro charges or treat overflow as zero. Decimal lexemes
// stay in the observation even when the integer ledger cannot represent them.
// Exact representation still does NOT establish settlement or invoice finality.
func usageExactMicroUSD(raw string) (string, string, error) {
	invalid := errors.New("unsupported cost number")
	if len(raw) > 128 {
		return "", "", invalid
	}
	parts := usageDecimal.FindStringSubmatch(raw)
	if parts == nil {
		return "", "", invalid
	}
	exponent := 0
	if parts[3] != "" {
		var err error
		exponent, err = strconv.Atoi(parts[3])
		if err != nil || exponent < -1000 || exponent > 1000 {
			return "", "", invalid
		}
	}
	coefficient, ok := new(big.Int).SetString(parts[1]+parts[2], 10)
	if !ok {
		return "", "", invalid
	}
	scale := exponent - len(parts[2]) + 6
	if scale >= 0 {
		coefficient.Mul(coefficient, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	} else {
		denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-scale)), nil)
		remainder := new(big.Int)
		coefficient.QuoRem(coefficient, denominator, remainder)
		if remainder.Sign() != 0 {
			return "", "fractional", nil
		}
	}
	if !coefficient.IsInt64() {
		return "", "out_of_range", nil
	}
	return coefficient.String(), "exact", nil
}

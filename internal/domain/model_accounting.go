// SPDX-License-Identifier: Elastic-2.0
package domain

const (
	ModelAccountingMetered        = "metered"
	ModelAccountingLocalUnmetered = "operator_unmetered_local"
)

// Empty is the historical metered profile. Never rewrite it in stored profiles
// or intent hashes: old immutable versions must retain their original identity.
func EffectiveModelAccountingMode(mode string) string {
	if mode == "" {
		return ModelAccountingMetered
	}
	return mode
}

// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

func recoveryImportReceiptCommitment(receipt RecoveryImportReceipt) string {
	receipt.ImportedAt = receipt.ImportedAt.UTC()
	return digestValue(receipt)
}

func recoveryImportReceiptMatches(receipt RecoveryImportReceipt, expected string) bool {
	_, ok := recoveryImportReceiptProofTime(receipt, expected)
	return ok
}

// New receipts are committed in UTC. An older admission hashed the operator
// host's JSON representation of the same PostgreSQL instant. Find that exact
// representation without trusting the current host's timezone.
func recoveryImportReceiptProofTime(receipt RecoveryImportReceipt, expected string) (time.Time, bool) {
	stamp := receipt.ImportedAt.UTC()
	if !validDigest(expected) || stamp.IsZero() {
		return time.Time{}, false
	}
	if recoveryImportReceiptCommitment(receipt) == expected {
		return stamp, true
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		return time.Time{}, false
	}
	if Hash(body) == expected {
		return receipt.ImportedAt, true
	}
	suffix, err := receipt.ImportedAt.MarshalJSON()
	if err != nil || !bytes.HasSuffix(body, append(suffix, '}')) {
		return time.Time{}, false
	}
	prefix := make([]byte, len(body)-len(suffix)-1, len(body)+6)
	copy(prefix, body)
	var want [sha256.Size]byte
	if _, err = hex.Decode(want[:], []byte(expected)); err != nil {
		return time.Time{}, false
	}
	// Replace only the trailing timestamp. RFC3339 serializes offsets in
	// minutes, so the fallback is bounded and reuses the encoded receipt.
	for minutes := -1439; minutes <= 1439; minutes++ {
		candidate := stamp.In(time.FixedZone("", minutes*60))
		encoded := candidate.AppendFormat(append(prefix, '"'), time.RFC3339Nano)
		encoded = append(encoded, '"', '}')
		if sha256.Sum256(encoded) == want {
			return candidate, true
		}
	}
	return time.Time{}, false
}

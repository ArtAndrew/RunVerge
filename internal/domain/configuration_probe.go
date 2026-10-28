// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// ConfigurationProbeDigest binds all private network, credential-purpose and
// protocol limits without exposing them. Liveness/timestamps are checked
// separately; they cannot change immutable profile identity. No raw key enters
// ConfigurationProbeConfig or this digest. Copy pointers before normalization.
func ConfigurationProbeDigest(config ConfigurationProbeConfig, resourceVersion int) string {
	if resourceVersion < 1 {
		return ""
	}
	config.Destination.Destination.Active = false
	config.Destination.Destination.CreatedAt = time.Time{}
	config.Purpose.Active = false
	config.Purpose.CreatedAt = time.Time{}
	if config.Destination.Model != nil {
		model := *config.Destination.Model
		model.Active, model.CreatedAt = false, time.Time{}
		config.Destination.Model = &model
	}
	raw, err := json.Marshal(struct {
		ResourceVersion int
		Profile         ConfigurationProbeConfig
	}{resourceVersion, config})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

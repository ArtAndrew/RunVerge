// SPDX-License-Identifier: Elastic-2.0
package webhookruntime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"time"

	"agentplatform.local/platform/internal/domain"
)

const signatureDomain = "agent-platform-webhook-v1\n"

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var signaturePattern = regexp.MustCompile(`^v1=[0-9a-f]{64}$`)

type SignatureHeaders struct {
	KeyID      string
	Timestamp  string
	DeliveryID string
	AttemptID  string
	Signature  string
}

// DecodeSigningKey accepts only canonical unpadded base64url of32..64 bytes.
// The error never includes a key, environment reference, body or signature.
func DecodeSigningKey(encoded string) ([]byte, error) {
	if len(encoded) < 43 || len(encoded) > 86 {
		return nil, errors.New("invalid webhook signing material")
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) < 32 || len(key) > 64 || base64.RawURLEncoding.EncodeToString(key) != encoded {
		return nil, errors.New("invalid webhook signing material")
	}
	return key, nil
}

// Sign binds the exact bytes, algorithm domain, public key identity, fresh
// timestamp, stable logical delivery ID and distinct physical attempt ID.
// A signature provides no platform read capability or receiver deduplication.
func Sign(key []byte, keyID, deliveryID, attemptID string, body []byte, at time.Time) (SignatureHeaders, error) {
	h := SignatureHeaders{KeyID: keyID, DeliveryID: deliveryID, AttemptID: attemptID, Timestamp: strconv.FormatInt(at.Unix(), 10)}
	if len(key) < 32 || len(key) > 64 || !validInput(h, body) || at.Unix() < 0 {
		return SignatureHeaders{}, errors.New("invalid webhook signing input")
	}
	h.Signature = "v1=" + hex.EncodeToString(mac(key, h, body))
	return h, nil
}

func validInput(h SignatureHeaders, body []byte) bool {
	if !safeID.MatchString(h.KeyID) || !safeID.MatchString(h.DeliveryID) || !safeID.MatchString(h.AttemptID) || len(body) == 0 || len(body) > domain.MaxWebhookEnvelopeBytes {
		return false
	}
	seconds, err := strconv.ParseInt(h.Timestamp, 10, 64)
	return err == nil && seconds >= 0 && strconv.FormatInt(seconds, 10) == h.Timestamp
}

func mac(key []byte, h SignatureHeaders, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(signatureDomain + h.KeyID + "\n" + h.Timestamp + "\n" + h.DeliveryID + "\n" + h.AttemptID + "\n"))
	_, _ = m.Write(body)
	return m.Sum(nil)
}

// Verify checks raw bytes before decoding or processing them. Callers must
// reject duplicate/ambiguous HTTP headers and durably deduplicate DeliveryID
// separately. In-window replay remains cryptographically valid by design.
func Verify(key []byte, h SignatureHeaders, body []byte, now time.Time, tolerance time.Duration) error {
	invalid := errors.New("invalid webhook signature")
	if len(key) < 32 || len(key) > 64 || !validInput(h, body) || !signaturePattern.MatchString(h.Signature) || tolerance < time.Second || tolerance > time.Hour || tolerance%time.Second != 0 || now.Unix() < 0 {
		return invalid
	}
	seconds, _ := strconv.ParseInt(h.Timestamp, 10, 64)
	current := now.Unix()
	window := int64(tolerance / time.Second)
	if seconds > current {
		if seconds-current > window {
			return invalid
		}
	} else if current-seconds > window {
		return invalid
	}
	supplied, err := hex.DecodeString(h.Signature[3:])
	if err != nil || !hmac.Equal(supplied, mac(key, h, body)) {
		return invalid
	}
	return nil
}

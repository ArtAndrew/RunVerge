// SPDX-License-Identifier: Elastic-2.0
package webhookruntime

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

func syntheticKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func TestWebhookSignatureKnownIndependentPythonHMACVector(t *testing.T) {
	body := []byte(`{"delivery_id":"d_1","event":{"sequence":"9007199254740993"}}`)
	at := time.Unix(1700000000, 0)
	h, err := Sign(syntheticKey(), "key_1", "d_1", "a_1", body, at)
	if err != nil || h.Signature != "v1=31988404c02da2fcbf618afbf371a213912fffef59512bf2acadaa69e9145a59" {
		t.Fatal("signature differs from independent standard-library vector")
	}
	if Verify(syntheticKey(), h, body, at, 5*time.Minute) != nil {
		t.Fatal("valid raw-byte signature rejected")
	}
	// Authentication intentionally does not claim exactly-once processing: a
	// valid in-window replay must be handled by durable receiver deduplication.
	if Verify(syntheticKey(), h, body, at.Add(time.Minute), 5*time.Minute) != nil {
		t.Fatal("signature verification must not pretend to keep replay state")
	}
}

func TestWebhookSignatureBindsEveryHeaderAndExactBody(t *testing.T) {
	body := []byte(`{"delivery_id":"d_1"}`)
	at := time.Unix(1700000000, 0)
	h, err := Sign(syntheticKey(), "key_1", "d_1", "a_1", body, at)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SignatureHeaders){
		"key":       func(h *SignatureHeaders) { h.KeyID = "key_2" },
		"delivery":  func(h *SignatureHeaders) { h.DeliveryID = "d_2" },
		"attempt":   func(h *SignatureHeaders) { h.AttemptID = "a_2" },
		"timestamp": func(h *SignatureHeaders) { h.Timestamp = "1700000001" },
		"algorithm": func(h *SignatureHeaders) { h.Signature = "v2=" + h.Signature[3:] },
		"signature": func(h *SignatureHeaders) { h.Signature = "v1=" + strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := h
			change(&changed)
			if Verify(syntheticKey(), changed, body, at, 5*time.Minute) == nil {
				t.Fatal("altered binding accepted")
			}
		})
	}
	for _, changed := range [][]byte{append(append([]byte{}, body...), '\n'), bytes.Replace(body, []byte(":"), []byte(": "), 1), []byte(`{"delivery_id":"d_2"}`)} {
		if Verify(syntheticKey(), h, changed, at, 5*time.Minute) == nil {
			t.Fatal("modified raw bytes accepted")
		}
	}
	other := bytes.Repeat([]byte{42}, 32)
	if Verify(other, h, body, at, 5*time.Minute) == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestWebhookSignatureCanonicalHeadersTimeAndBounds(t *testing.T) {
	body := []byte(`{}`)
	at := time.Unix(1700000000, 0)
	h, _ := Sign(syntheticKey(), "key_1", "d_1", "a_1", body, at)
	for _, offset := range []time.Duration{-301 * time.Second, 301 * time.Second} {
		if Verify(syntheticKey(), h, body, at.Add(offset), 5*time.Minute) == nil {
			t.Fatal("expired or future signature accepted")
		}
	}
	for _, offset := range []time.Duration{-300 * time.Second, 300 * time.Second} {
		if Verify(syntheticKey(), h, body, at.Add(offset), 5*time.Minute) != nil {
			t.Fatal("inclusive timestamp boundary rejected")
		}
	}
	for _, timestamp := range []string{"01700000000", "+1700000000", "1700000000 ", "1700000000,1700000000", "-1", strconv.FormatInt(9223372036854775807, 10)} {
		changed := h
		changed.Timestamp = timestamp
		if Verify(syntheticKey(), changed, body, at, 5*time.Minute) == nil {
			t.Fatal("noncanonical, ambiguous or far-future timestamp accepted")
		}
	}
	for _, id := range []string{"", strings.Repeat("a", 129), "d_1\na_2", "d_1,d_2", "d 1"} {
		if _, err := Sign(syntheticKey(), "key_1", id, "a_1", body, at); err == nil {
			t.Fatal("ambiguous delivery identity accepted")
		}
	}
	for _, signature := range []string{h.Signature + "," + h.Signature, strings.ToUpper(h.Signature), h.Signature + "\n"} {
		changed := h
		changed.Signature = signature
		if Verify(syntheticKey(), changed, body, at, 5*time.Minute) == nil {
			t.Fatal("ambiguous signature accepted")
		}
	}
	for _, b := range [][]byte{nil, bytes.Repeat([]byte{'x'}, 4097)} {
		if _, err := Sign(syntheticKey(), "key_1", "d_1", "a_1", b, at); err == nil {
			t.Fatal("body bound not enforced")
		}
	}
	for _, window := range []time.Duration{0, time.Millisecond, 1500 * time.Millisecond, time.Hour + time.Second} {
		if Verify(syntheticKey(), h, body, at, window) == nil {
			t.Fatal("invalid replay window accepted")
		}
	}
}

func TestWebhookSigningKeyEncodingIsCanonicalBoundedAndSecretSafe(t *testing.T) {
	for _, size := range []int{32, 64} {
		key := bytes.Repeat([]byte{42}, size)
		raw := base64.RawURLEncoding.EncodeToString(key)
		decoded, err := DecodeSigningKey(raw)
		if err != nil || !bytes.Equal(key, decoded) {
			t.Fatal("valid dedicated key rejected")
		}
	}
	valid := base64.RawURLEncoding.EncodeToString(syntheticKey())
	for _, raw := range []string{"", valid + "=", valid + "\n", valid[:10] + "\n" + valid[10:], strings.Repeat("a", 10000), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{42}, 31)), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{42}, 65))} {
		_, err := DecodeSigningKey(raw)
		if err == nil {
			t.Fatal("invalid encoded key accepted")
		}
		if len(raw) > 10 && strings.Contains(err.Error(), raw) {
			t.Fatal("key exposed in error")
		}
	}
}

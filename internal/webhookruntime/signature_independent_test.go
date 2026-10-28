// SPDX-License-Identifier: Elastic-2.0
package webhookruntime_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/webhookruntime"
)

// These expected digests were independently calculated with Python's standard
// hmac/hashlib from the published wire description, without calling Go Sign.
func TestIndependentWebhookPublishedWireVectors(t *testing.T) {
	wideKey := make([]byte, 64)
	for i := range wideKey {
		wideKey[i] = byte(i)
	}
	for _, tc := range []struct {
		name, keyID, delivery, attempt, bodyHex, digest string
		key                                             []byte
		second                                          int64
	}{
		{"unicode_epoch_64byte", "key-Z_9", "whd_fixed", "wha_2", "7b2278223a22d0b6f09f9880222c226e223a2239303037313939323534373430393933227d", "7c72f65716df8b45dc9ff710b315689af19745b03fb65e4562c87ee22a932861", wideKey, 0},
		{"int64_timestamp_zero_key", "K", "D", "A", "7b7d", "c97f6f5b47f245c924782a2712d4c859d6dc96f539d7b2a800f68d26367c91e3", make([]byte, 32), math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := hex.DecodeString(tc.bodyHex)
			if err != nil {
				t.Fatal(err)
			}
			h, err := webhookruntime.Sign(tc.key, tc.keyID, tc.delivery, tc.attempt, body, time.Unix(tc.second, 0))
			if err != nil || h.Signature != "v1="+tc.digest || h.Timestamp != strconv.FormatInt(tc.second, 10) {
				t.Fatalf("published wire vector failed: %v", err)
			}
			if err := webhookruntime.Verify(tc.key, h, body, time.Unix(tc.second, 0), time.Second); err != nil {
				t.Fatal("independent vector not verified", err)
			}
		})
	}
}

func independentWebhookHeaders(key, body []byte, stamp string) webhookruntime.SignatureHeaders {
	h := webhookruntime.SignatureHeaders{KeyID: "key", DeliveryID: "delivery", AttemptID: "attempt", Timestamp: stamp}
	// Deliberately does not call the implementation's Sign/mac helpers.
	message := append([]byte("agent-platform-webhook-v1\nkey\n"+stamp+"\ndelivery\nattempt\n"), body...)
	computed := hmac.New(sha256.New, key)
	_, _ = computed.Write(message)
	h.Signature = "v1=" + hex.EncodeToString(computed.Sum(nil))
	return h
}

func TestIndependentWebhookCanonicalTimeAndOverflowBoundaries(t *testing.T) {
	key, body := bytes.Repeat([]byte{0x76}, 32), []byte(`{}`)
	for _, stamp := range []string{"0", "1", "9223372036854775807"} {
		h := independentWebhookHeaders(key, body, stamp)
		seconds, _ := strconv.ParseInt(stamp, 10, 64)
		if err := webhookruntime.Verify(key, h, body, time.Unix(seconds, 999999999), time.Second); err != nil {
			t.Fatal("valid canonical int64 timestamp rejected", stamp, err)
		}
	}
	for _, stamp := range []string{"00", "-0", "+0", " 0", "0 ", "0\r\n", "0,0", "0.0", "0e0", "9223372036854775808", "18446744073709551615", "-9223372036854775808"} {
		h := independentWebhookHeaders(key, body, stamp)
		if webhookruntime.Verify(key, h, body, time.Unix(0, 0), time.Hour) == nil {
			t.Fatal("independently MACed noncanonical timestamp accepted", stamp)
		}
	}
	// Both subtraction branches must reject extreme distance without overflow.
	for _, clocks := range [][2]int64{{0, math.MaxInt64}, {math.MaxInt64, 0}} {
		h := independentWebhookHeaders(key, body, strconv.FormatInt(clocks[0], 10))
		if webhookruntime.Verify(key, h, body, time.Unix(clocks[1], 0), time.Hour) == nil {
			t.Fatal("overflow-distance timestamp admitted")
		}
	}
	h := independentWebhookHeaders(key, body, "10000")
	for _, window := range []time.Duration{time.Second, time.Hour} {
		for _, offset := range []int64{-int64(window / time.Second), int64(window / time.Second)} {
			if err := webhookruntime.Verify(key, h, body, time.Unix(10000+offset, 999999999), window); err != nil {
				t.Fatal("whole-second inclusive boundary rejected", err)
			}
			outside := offset + 1
			if offset < 0 {
				outside = offset - 1
			}
			if webhookruntime.Verify(key, h, body, time.Unix(10000+outside, 0), window) == nil {
				t.Fatal("outside whole-second boundary accepted")
			}
		}
	}
}

func TestIndependentWebhookAllKeyLengthsAndCanonicalBase64(t *testing.T) {
	for size := 0; size <= 96; size++ {
		key := bytes.Repeat([]byte{0xff}, size)
		raw := base64.RawURLEncoding.EncodeToString(key)
		decoded, err := webhookruntime.DecodeSigningKey(raw)
		want := size >= 32 && size <= 64
		if want != (err == nil) || (want && !bytes.Equal(key, decoded)) {
			t.Fatalf("key bound or canonical decode failed at %d bytes", size)
		}
		if !want {
			continue
		}
		for _, malformed := range []string{raw + "=", raw + "\n", raw[:10] + "\r\n" + raw[10:], strings.ReplaceAll(raw, "_", "/")} {
			if malformed == raw {
				continue
			}
			if _, err := webhookruntime.DecodeSigningKey(malformed); err == nil {
				t.Fatal("noncanonical base64url key accepted")
			}
		}
	}
	// Alternate final alphabet indices with nonzero unused pad bits decode to the
	// same bytes in permissive decoders; the published canonical profile rejects.
	canonical := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, last := range []byte{'B', 'C', 'D'} {
		if _, err := webhookruntime.DecodeSigningKey(canonical[:len(canonical)-1] + string(last)); err == nil {
			t.Fatal("nonzero unused base64 pad bits accepted")
		}
	}
}

func TestIndependentWebhookEveryRawByteAndMACByteBound(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, 32)
	body := []byte("{\"text\":\"ж😀\",\"sequence\":\"9007199254740993\"}")
	at := time.Unix(1770000000, 0)
	h, err := webhookruntime.Sign(key, "kid", "delivery", "attempt", body, at)
	if err != nil {
		t.Fatal(err)
	}
	for i := range body {
		mutated := bytes.Clone(body)
		mutated[i] ^= 1
		if webhookruntime.Verify(key, h, mutated, at, time.Second) == nil {
			t.Fatal("raw-byte modification accepted", i)
		}
	}
	for i := 3; i < len(h.Signature); i++ {
		mutated := []byte(h.Signature)
		mutated[i] = '0'
		if h.Signature[i] == '0' {
			mutated[i] = '1'
		}
		changed := h
		changed.Signature = string(mutated)
		if webhookruntime.Verify(key, changed, body, at, time.Second) == nil {
			t.Fatal("MAC modification accepted", i)
		}
	}
	maxID := strings.Repeat("z", 128)
	maxBody := bytes.Repeat([]byte{'x'}, 4096)
	if _, err := webhookruntime.Sign(key, maxID, maxID, maxID, maxBody, at); err != nil {
		t.Fatal("exact valid size boundary rejected", err)
	}
	for _, id := range []string{"x\r", "x\n", "x\t", "x x", "x,x", "x:x", "x.x", "ж", strings.Repeat("x", 129)} {
		for field := 0; field < 3; field++ {
			ids := []string{"key", "delivery", "attempt"}
			ids[field] = id
			if _, err := webhookruntime.Sign(key, ids[0], ids[1], ids[2], body, at); err == nil {
				t.Fatal("non-profile or ambiguous identifier accepted", field)
			}
		}
	}
}

func TestIndependentWebhookPrimitiveDoesNotClaimEntropyReplayOrSchemaValidation(t *testing.T) {
	// Length is not entropy proof. The trusted installer must generate random
	// material; accepting this structurally valid key is not a security finding.
	key := make([]byte, 32)
	body := []byte(`{"delivery_id":"DIFFERENT","unexpected":"not an envelope"}`)
	at := time.Unix(1770000000, 0)
	h, err := webhookruntime.Sign(key, "key", "delivery", "attempt", body, at)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := webhookruntime.Verify(key, h, body, at, time.Second); err != nil {
			t.Fatal("raw-byte verifier must be separate from receiver schema/dedup", err)
		}
	}
	// Header.Get discards multiplicity. A receiver must enforce exactly one
	// case-insensitive value for all five headers before constructing this struct.
	headers := http.Header{}
	headers.Add("Webhook-Signature", h.Signature)
	headers.Add("webhook-signature", h.Signature)
	if len(headers.Values("Webhook-Signature")) != 2 || headers.Get("Webhook-Signature") != h.Signature {
		t.Fatal("duplicate-header fixture does not reproduce extraction hazard")
	}
	fromFirstValue := h
	fromFirstValue.Signature = headers.Get("Webhook-Signature")
	if err := webhookruntime.Verify(key, fromFirstValue, body, at, time.Second); err != nil {
		t.Fatal("fixture must demonstrate that multiplicity has already been lost")
	}
}

func TestIndependentWebhookConcurrentUsePreservesCallerBuffers(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 64)
	body := []byte(`{"id":"unchanged"}`)
	beforeKey, beforeBody := bytes.Clone(key), bytes.Clone(body)
	at := time.Unix(1770000000, 0)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := webhookruntime.Sign(key, "key", "delivery", "attempt", body, at)
			if err != nil || webhookruntime.Verify(key, h, body, at, time.Second) != nil {
				t.Error("independent concurrent signing/verification failed")
			}
		}()
	}
	wg.Wait()
	if !bytes.Equal(key, beforeKey) || !bytes.Equal(body, beforeBody) {
		t.Fatal("primitive mutated caller key or body")
	}
}

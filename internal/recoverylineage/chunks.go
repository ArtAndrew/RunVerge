// SPDX-License-Identifier: Elastic-2.0
// Package recoverylineage implements bounded historical payload framing.
// Framing integrity does not authenticate a backup or grant execution authority.
package recoverylineage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"strconv"
)

const ChunkBytes = 1 << 20
const MaxPayloadBytes int64 = 384 * ChunkBytes
const chunkDomain = "RunVerge lineage chunks v1\n"

var ErrChunks = errors.New("invalid recovery lineage chunks")

// ChunkCommitment hashes ordered ordinal:length:SHA256 lines, matching the
// SQL lineage completeness guard. It is NOT the SHA256 of concatenated payload.
// Payload buffers are consumed immediately and are never retained.
// Instances are single-stream and not safe for concurrent use.
type ChunkCommitment struct {
	total  int64
	next   int
	count  int
	digest hash.Hash
	failed bool
}

func NewChunkCommitment(totalBytes int64) (*ChunkCommitment, error) {
	if totalBytes < 1 || totalBytes > MaxPayloadBytes {
		return nil, ErrChunks
	}
	h := sha256.New()
	_, _ = h.Write([]byte(chunkDomain))
	return &ChunkCommitment{total: totalBytes, count: int((totalBytes + ChunkBytes - 1) / ChunkBytes), digest: h}, nil
}

// Add requires each declared slot exactly once, in order, with its exact size.
// A rejected chunk permanently invalidates this accumulator.
func (c *ChunkCommitment) Add(ordinal int, payload []byte) error {
	if c == nil {
		return ErrChunks
	}
	if c.failed || c.digest == nil || c.next >= c.count || ordinal != c.next {
		c.failed = true
		return ErrChunks
	}
	expected := min(int64(ChunkBytes), c.total-int64(ordinal)*ChunkBytes)
	if int64(len(payload)) != expected {
		c.failed = true
		return ErrChunks
	}
	if ordinal > 0 {
		_, _ = c.digest.Write([]byte{'\n'})
	}
	sum := sha256.Sum256(payload)
	line := strconv.Itoa(ordinal) + ":" + strconv.Itoa(len(payload)) + ":" + hex.EncodeToString(sum[:])
	_, _ = c.digest.Write([]byte(line))
	c.next++
	return nil
}

// Digest succeeds only for a complete stream. Inspecting an incomplete stream
// does not consume it. Calling Add after completion invalidates the instance.
func (c *ChunkCommitment) Digest() (string, error) {
	if c == nil || c.failed || c.digest == nil || c.next != c.count {
		return "", ErrChunks
	}
	return hex.EncodeToString(c.digest.Sum(nil)), nil
}

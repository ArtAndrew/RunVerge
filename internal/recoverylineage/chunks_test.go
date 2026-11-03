// SPDX-License-Identifier: Elastic-2.0
package recoverylineage

import (
	"errors"
	"testing"
)

func TestChunkCommitmentWireVectors(t *testing.T) {
	// Independent Python hashlib vectors for the documented SQL commitment.
	for _, tc := range []struct {
		total  int64
		chunks [][]byte
		want   string
	}{
		{3, [][]byte{[]byte("abc")}, "6ae408386a61f69681602a20e7ea6f4916caffd320353b0477e11293fdc7438c"},
		{ChunkBytes + 1, [][]byte{make([]byte, ChunkBytes), []byte("z")}, "e7b4407cd39ab26b77d5811c12f59130ac73fd92d4a58bdff4f68ed1164f01f3"},
	} {
		c, err := NewChunkCommitment(tc.total)
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range tc.chunks {
			if err = c.Add(i, p); err != nil {
				t.Fatal(err)
			}
			p[0] ^= 0xff
		}
		got, err := c.Digest()
		if err != nil || got != tc.want {
			t.Fatal("wire commitment mismatch", got, err)
		}
		again, err := c.Digest()
		if err != nil || again != got {
			t.Fatal("digest inspection changed stream")
		}
	}
}
func TestChunkCommitmentRejectsIncompleteReorderedAndChangedLengths(t *testing.T) {
	for _, n := range []int64{-1, 0, MaxPayloadBytes + 1} {
		if _, err := NewChunkCommitment(n); !errors.Is(err, ErrChunks) {
			t.Fatal("invalid bound accepted")
		}
	}
	for _, tc := range []struct {
		name    string
		ordinal int
		size    int
	}{
		{"negative", -1, ChunkBytes}, {"missing_first", 1, 1}, {"short", 0, ChunkBytes - 1}, {"oversize", 0, ChunkBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := NewChunkCommitment(ChunkBytes + 1)
			if err := c.Add(tc.ordinal, make([]byte, tc.size)); !errors.Is(err, ErrChunks) {
				t.Fatal("bad chunk accepted")
			}
			if err := c.Add(0, make([]byte, ChunkBytes)); !errors.Is(err, ErrChunks) {
				t.Fatal("failed stream revived")
			}
		})
	}
	c, _ := NewChunkCommitment(ChunkBytes + 1)
	if _, err := c.Digest(); !errors.Is(err, ErrChunks) {
		t.Fatal("missing stream accepted")
	}
	if err := c.Add(0, make([]byte, ChunkBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Digest(); !errors.Is(err, ErrChunks) {
		t.Fatal("truncated stream accepted")
	}
	if err := c.Add(0, make([]byte, ChunkBytes)); !errors.Is(err, ErrChunks) {
		t.Fatal("duplicate accepted")
	}
	c, _ = NewChunkCommitment(1)
	_ = c.Add(0, []byte("x"))
	if err := c.Add(1, []byte("y")); !errors.Is(err, ErrChunks) {
		t.Fatal("trailing chunk accepted")
	}
	if _, err := c.Digest(); !errors.Is(err, ErrChunks) {
		t.Fatal("trailing data still has valid commitment")
	}
	var zero ChunkCommitment
	if err := zero.Add(0, []byte("x")); !errors.Is(err, ErrChunks) {
		t.Fatal("zero accumulator accepted")
	}
}

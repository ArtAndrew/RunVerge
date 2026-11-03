// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestRecoveryLineageBinaryMetadataTypes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		oid   uint32
		raw   []byte
		valid bool
	}{
		{"name", 19, []byte("operator"), true},
		{"name_max", 19, bytes.Repeat([]byte("a"), 63), true},
		{"name_truncation", 19, bytes.Repeat([]byte("a"), 64), false},
		{"name_nul", 19, []byte{'a', 0}, false},
		{"name_invalid_utf8", 19, []byte{255}, false},
		{"xid8", 5069, []byte{255, 255, 255, 255, 255, 255, 255, 255}, true},
		{"xid8_short", 5069, make([]byte, 7), false},
		{"xid8_long", 5069, make([]byte, 9), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := RecoverySnapshotTable{Columns: []RecoverySnapshotColumn{{TypeOID: tc.oid, NotNull: true}}}
			raw := independentRecoveryCopy([][]byte{tc.raw})
			reader := newRecoveryStageTypedReader(context.Background(), bytes.NewReader(raw), table, DefaultRecoverySnapshotLimits())
			result, err := io.ReadAll(reader)
			stop := reader.Stop()
			if tc.valid {
				if err != nil || stop != nil || !bytes.Equal(result, raw) {
					t.Fatal("metadata bytes changed or rejected", err, stop)
				}
			} else if err == nil && stop == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

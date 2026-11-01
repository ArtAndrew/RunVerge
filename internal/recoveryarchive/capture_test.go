// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureLinkRejectsSplicingAndWrongKey(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(d, "key")
	other := filepath.Join(d, "other")
	path := filepath.Join(d, "link")
	if _, err := GenerateHostKeys(key); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateHostKeys(other); err != nil {
		t.Fatal(err)
	}
	h := strings.Repeat("a", 64)
	l := CaptureLink{Version: 1, OperationID: strings.Repeat("b", 32), SourceID: "inst_" + strings.Repeat("c", 32), SelectedReceiptSHA256: h, SelectedSnapshotSHA256: h, SelectedCutUTC: "2026-11-01T00:00:00Z", BindingSHA256: h, StoppedExecutorsSHA256: h, EvidenceReceiptSHA256: h, EvidenceSnapshotSHA256: h, EvidenceCutUTC: "2026-11-01T00:00:01Z", CapturedAt: time.Now().UTC()}
	if err := WriteHostCaptureLink(path, key, l); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHostCaptureLink(path, key)
	if err != nil || got != l {
		t.Fatal("link round trip", err)
	}
	if err = WriteHostCaptureLink(path, key, l); err == nil {
		t.Fatal("link overwritten")
	}
	if _, err = ReadHostCaptureLink(path, other); err == nil {
		t.Fatal("wrong signer accepted")
	}
	raw, _ := os.ReadFile(path)
	var signed signedCapture
	if err = json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	signed.Link.SelectedReceiptSHA256 = strings.Repeat("d", 64)
	raw, _ = json.Marshal(signed)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadHostCaptureLink(path, key); err == nil {
		t.Fatal("old archive splice accepted")
	}
}

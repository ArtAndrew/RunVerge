// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
)

func TestRecoveryProcessHostRequiresExactConsentBeforeDocker(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "intent.json")
	if err := recoveryarchive.WriteHostJSON(path, recoveryProcessFile{Socket: "/unavailable/socket", Intent: recoveryprocess.Intent{Project: "synthetic", Containers: []recoveryprocess.Container{}}}); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{nil, {"--confirm-stop-project"}, {"--expected-project", "synthetic"}, {"--confirm-stop-project", "--expected-project", "different"}} {
		args := append([]string{"container-stop-apply", "--intent-file", path}, flags...)
		if err := runRecoveryHost(context.Background(), args, io.Discard); err == nil || err.Error() != "usage: platform recovery container-stop-plan|container-stop-inspect|container-stop-apply [explicit private file options]" {
			t.Fatalf("consent not refused before connection: %v", err)
		}
	}
}

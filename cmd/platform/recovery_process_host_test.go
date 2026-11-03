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
		if err := runRecoveryHost(context.Background(), args, io.Discard); err == nil || err.Error() != "usage: platform recovery container-stop-plan|container-stop-inspect|container-stop-apply|source-stop-plan|source-stop-inspect|source-stop-apply [explicit private file options]" {
			t.Fatalf("consent not refused before connection: %v", err)
		}
	}
}

func TestSourceStopProofPathsExcludeBothArchives(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	selected, evidence := filepath.Join(parent, "selected"), filepath.Join(parent, "evidence")
	for _, dir := range []string{selected, evidence} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	saved := recoveryProcessFile{Socket: "/unavailable/socket", Intent: recoveryprocess.Intent{Containers: []recoveryprocess.Container{}}, Binding: &recoveryprocess.SourceBinding{Processes: []recoveryprocess.SourceProcess{}}, Capture: &recoverySourceCapture{SelectedDirectory: selected, Directory: evidence}}
	outside := filepath.Join(parent, "fresh")
	if err := validateSourceStopPath(saved, outside); err != nil {
		t.Fatal(err)
	}
	intent := filepath.Join(parent, "intent")
	if err := recoveryarchive.WriteHostJSON(intent, saved); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(selected, "proof"), filepath.Join(evidence, "proof")} {
		if err := validateSourceStopPath(saved, path); err == nil {
			t.Fatal("archive accepted as authority")
		}
		if err := runRecoveryProcessHost(context.Background(), []string{"source-stop-inspect", "--intent-file", intent, "--source-stop-out", path}, io.Discard); err == nil || err.Error() != "source stop proof requires capture and a private path outside both archives" {
			t.Fatal("path not refused before Docker", err)
		}
	}
	saved.Capture = nil
	if err := validateSourceStopPath(saved, outside); err == nil {
		t.Fatal("proof without capture accepted")
	}
}

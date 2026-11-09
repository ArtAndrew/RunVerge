// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryProcessHostCapturePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(root, "selected")
	stage := filepath.Join(root, "stage")
	for _, p := range []string{selected, stage} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	intent := filepath.Join(root, "intent")
	c := recoverySourceCapture{Directory: filepath.Join(root, "evidence"), Keys: filepath.Join(root, "keys"), Stage: stage}
	if err := validateSourceCapturePaths(selected, intent, c); err != nil {
		t.Fatal("new sibling refused", err)
	}
	for _, fault := range []string{"inside_selected", "existing", "symlink", "shared_key", "inside_intent"} {
		t.Run(fault, func(t *testing.T) {
			bad := c
			path := intent
			switch fault {
			case "inside_selected":
				bad.Directory = filepath.Join(selected, "evidence")
			case "existing":
				bad.Directory = stage
			case "symlink":
				bad.Directory = filepath.Join(root, "link")
				if err := os.Symlink(stage, bad.Directory); err != nil {
					t.Fatal(err)
				}
			case "shared_key":
				bad.Keys = bad.Directory
			case "inside_intent":
				path = filepath.Join(bad.Directory, "intent")
			}
			if err := validateSourceCapturePaths(selected, path, bad); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
}

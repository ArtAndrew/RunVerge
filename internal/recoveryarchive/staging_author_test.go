//go:build darwin || linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuthorStagingCleanupFailureIsStickyAndNeverDeletesUnexpectedFiles(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	stage, err := newStaging(parent)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := stage.anonymousFile("plaintext")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = plain.WriteString("private test material"); err != nil {
		t.Fatal(err)
	}
	// A foreign entry prevents cleanup; the module must not recursively remove
	// unexpected content or turn a second Close into a false cleanup success.
	sentinel := filepath.Join(parent, stage.name, "unexpected")
	if err = os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = stage.Close(); err != ErrStaging {
		t.Fatalf("close: %v", err)
	}
	if err = stage.Close(); err != ErrStaging {
		t.Fatalf("repeated close: %v", err)
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "preserve" {
		t.Fatal("cleanup removed unexpected content")
	}
	if _, err = plain.Read(make([]byte, 1)); err == nil {
		t.Fatal("cleanup left plaintext descriptor open")
	}
}

func TestAuthorVerifiedCloseReportsDescriptorFailureAndZeroValueIsClosed(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	stage, err := newStaging(parent)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := stage.anonymousFile("plaintext")
	if err != nil {
		t.Fatal(err)
	}
	archive := &VerifiedArchive{file: plain, stage: stage}
	if err = plain.Close(); err != nil {
		t.Fatal(err)
	}
	if err = archive.Close(); err != ErrStaging {
		t.Fatalf("close error lost: %v", err)
	}
	if err = archive.Close(); err != ErrStaging {
		t.Fatalf("close failure not sticky: %v", err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatal("empty owned directory not removed")
	}
	var zero VerifiedArchive
	if _, err = zero.Read(make([]byte, 1)); err != ErrClosed {
		t.Fatal("zero reader is not closed")
	}
	if _, err = zero.Seek(0, 0); err != ErrClosed {
		t.Fatal("zero seeker is not closed")
	}
	if err = zero.Close(); err != ErrClosed {
		t.Fatal("zero close result")
	}
}

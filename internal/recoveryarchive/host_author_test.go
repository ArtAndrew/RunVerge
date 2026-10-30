//go:build darwin || linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
)

func TestHostAuthorPublicationSelectionAndPrivateKeys(t *testing.T) {
	parent, stage := privateDirectory(t), privateDirectory(t)
	ctx := context.Background()
	key := filepath.Join(parent, "keys.json")
	summary, err := recoveryarchive.GenerateHostKeys(key)
	if err != nil || summary.Recipient == "" || summary.SigningKeySHA256 == "" {
		t.Fatal("key creation failed", err)
	}
	keyBytes, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = recoveryarchive.GenerateHostKeys(key); err == nil {
		t.Fatal("keys overwritten")
	}
	after, _ := os.ReadFile(key)
	if !bytes.Equal(keyBytes, after) {
		t.Fatal("key bytes changed")
	}
	meta := newFixture(t).metadata
	create := func(name, content string) recoveryarchive.HostBackupReceipt {
		t.Helper()
		r, e := recoveryarchive.CreateHostBackup(ctx, filepath.Join(parent, name), key, stage, func(_ context.Context, w io.Writer) (recoveryarchive.Metadata, error) {
			_, e := io.WriteString(w, content)
			return meta, e
		})
		if e != nil {
			t.Fatal("backup create failed", e)
		}
		return r
	}
	old := create("old", "synthetic old private content")
	meta.BackupID = "backup_" + strings.Repeat("b", 32)
	current := create("current", "synthetic current private content")
	dir := filepath.Join(parent, "current")
	archive, err := recoveryarchive.VerifyHostBackup(ctx, dir, key, current.ReceiptSHA256, stage)
	if err != nil {
		t.Fatal("current selection rejected", err)
	}
	plain, err := io.ReadAll(archive)
	if err != nil || string(plain) != "synthetic current private content" {
		t.Fatal("content changed")
	}
	if archive.Close() != nil {
		t.Fatal("plaintext cleanup failed")
	}
	assertEmpty(t, stage)
	if _, err = recoveryarchive.VerifyHostBackup(ctx, filepath.Join(parent, "old"), key, current.ReceiptSHA256, stage); err == nil {
		t.Fatal("authentic stale archive and sidecar replaced current selection")
	}
	if old.ReceiptSHA256 == current.ReceiptSHA256 {
		t.Fatal("fixture receipts not distinct")
	}
	if _, err = recoveryarchive.CreateHostBackup(ctx, dir, key, stage, func(context.Context, io.Writer) (recoveryarchive.Metadata, error) {
		t.Fatal("existing backup reached exporter")
		return meta, nil
	}); err == nil {
		t.Fatal("backup adopted existing output")
	}
	other := filepath.Join(parent, "other-keys.json")
	if _, err = recoveryarchive.GenerateHostKeys(other); err != nil {
		t.Fatal(err)
	}
	if _, err = recoveryarchive.VerifyHostBackup(ctx, dir, other, current.ReceiptSHA256, stage); err == nil {
		t.Fatal("wrong independent keys accepted")
	}
	inside := filepath.Join(dir, "keys.json")
	if err = os.WriteFile(inside, keyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = recoveryarchive.VerifyHostBackup(ctx, dir, inside, current.ReceiptSHA256, stage); err == nil {
		t.Fatal("transported keys trusted")
	}
	if err = os.Remove(inside); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 {
		t.Fatal("unexpected backup output")
	}
	for _, entry := range entries {
		data, e := os.ReadFile(filepath.Join(dir, entry.Name()))
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(data, keyBytes) || bytes.Contains(data, plain) {
			t.Fatal("plaintext or key exposed in bundle")
		}
	}
	// Secret-bearing target intents cannot be added through canonical aliases.
	alias := filepath.Join(privateDirectory(t), "alias")
	if err = os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "target.json"), filepath.Join(alias, "current", "target.json")} {
		if recoveryarchive.WriteHostJSONOutside(dir, path, map[string]string{"database_url": "synthetic-private-password"}) == nil {
			t.Fatal("private target intent written into backup")
		}
		if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("rejected intent created a file")
		}
	}
}

func TestHostAuthorIncompletePublicationAndLock(t *testing.T) {
	parent, stage := privateDirectory(t), privateDirectory(t)
	key := filepath.Join(parent, "keys.json")
	if _, err := recoveryarchive.GenerateHostKeys(key); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "partial")
	meta := newFixture(t).metadata
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := recoveryarchive.CreateHostBackup(context.Background(), dir, key, stage, func(_ context.Context, w io.Writer) (recoveryarchive.Metadata, error) {
			close(entered)
			<-release
			_, _ = io.WriteString(w, "private partial export")
			return meta, errors.New("synthetic source failure")
		})
		done <- err
	}()
	<-entered
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := recoveryarchive.VerifyHostBackup(short, dir, key, strings.Repeat("a", 64), stage)
	cancel()
	if err == nil {
		close(release)
		t.Fatal("verification passed active incomplete writer")
	}
	close(release)
	if <-done == nil {
		t.Fatal("failed source published success")
	}
	if _, err = os.Stat(filepath.Join(dir, "receipt.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed export published receipt")
	}
	if _, err = recoveryarchive.VerifyHostBackup(context.Background(), dir, key, strings.Repeat("a", 64), stage); err == nil {
		t.Fatal("partial backup accepted")
	}
	assertEmpty(t, stage)
	if _, err = recoveryarchive.CreateHostBackup(context.Background(), dir, key, stage, func(context.Context, io.Writer) (recoveryarchive.Metadata, error) {
		t.Fatal("partial output silently adopted")
		return meta, nil
	}); err == nil {
		t.Fatal("partial output overwritten")
	}
}

func TestHostAuthorTruncationAndKeyErrors(t *testing.T) {
	parent, stage := privateDirectory(t), privateDirectory(t)
	key := filepath.Join(parent, "keys.json")
	_, err := recoveryarchive.GenerateHostKeys(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "backup")
	meta := newFixture(t).metadata
	receipt, err := recoveryarchive.CreateHostBackup(context.Background(), dir, key, stage, func(_ context.Context, w io.Writer) (recoveryarchive.Metadata, error) {
		_, e := io.WriteString(w, "test payload")
		return meta, e
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(filepath.Join(dir, "archive.rvg"), 20); err != nil {
		t.Fatal(err)
	}
	if _, err = recoveryarchive.VerifyHostBackup(context.Background(), dir, key, receipt.ReceiptSHA256, stage); err == nil {
		t.Fatal("truncated archive accepted")
	}
	const secret = "do-not-log-private-key-material"
	bad := filepath.Join(parent, "bad.json")
	if err = os.WriteFile(bad, []byte(`{"version":1,"identity":"`+secret+`","signing_seed":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = recoveryarchive.VerifyHostBackup(context.Background(), dir, bad, receipt.ReceiptSHA256, stage)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("malformed key accepted or leaked")
	}
}

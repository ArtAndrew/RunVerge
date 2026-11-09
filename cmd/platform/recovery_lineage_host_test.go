// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
)

func recoveryHostLineageRoundTrip(t *testing.T, ctx context.Context, parent, stage, keys, sourceFile string, source recoveryHostTarget, org, candidate, content string) {
	t.Helper()
	bundle := filepath.Join(parent, "lineage-backup")
	backup := []string{"backup-create", "--keys-file", keys, "--staging-parent", stage, "--out-dir", bundle, "--target-config"}
	wrong := source
	wrong.SnapshotSHA256 = strings.Repeat("0", 64)
	wrongFile := recoveryHostPrivateJSON(t, filepath.Join(parent, "wrong-held.json"), wrong)
	wrongBundle := filepath.Join(parent, "rejected-backup")
	wrongBackup := append([]string{}, backup...)
	wrongBackup[len(wrongBackup)-2] = wrongBundle
	if raw, err := recoveryHostCommand(t, ctx, append(wrongBackup, wrongFile)...); err == nil || len(raw) != 0 {
		t.Fatal("held backup accepted the wrong import identity")
	}
	for _, file := range []string{"archive.rvg", "receipt.json"} {
		if _, err := os.Lstat(filepath.Join(wrongBundle, file)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed backup published output")
		}
	}
	raw, err := recoveryHostCommand(t, ctx, append(backup, sourceFile)...)
	var receipt recoveryarchive.HostBackupReceipt
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.ReceiptSHA256 == "" {
		t.Fatal("held backup", err)
	}
	archive := []string{"--backup-dir", bundle, "--keys-file", keys, "--staging-parent", stage}
	selection := append(append([]string{}, archive...), "--expected-receipt-sha256", receipt.ReceiptSHA256)
	if _, err := recoveryHostCommand(t, ctx, append([]string{"archive-verify"}, selection...)...); err != nil {
		t.Fatal("lineage archive verification", err)
	}
	target, _, targetFile := recoveryHostProvisionedTarget(t, ctx, parent, selection)
	inspect := []string{"import-inspect", "--target-config", targetFile}
	assertInspection := func(state string) {
		t.Helper()
		raw, err := recoveryHostCommand(t, ctx, append(append([]string{}, inspect...), archive...)...)
		var result struct{ State string }
		if err != nil || json.Unmarshal(raw, &result) != nil || result.State != state {
			t.Fatal("lineage import inspection", state, err)
		}
	}
	assertInspection("empty_held")
	importArgs := append([]string{"archive-import", "--target-config", targetFile}, archive...)
	if err := runRecoveryHost(ctx, importArgs, recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
		t.Fatal("lineage import output loss", err)
	}
	assertInspection("committed_held")
	if raw, err := recoveryHostCommand(t, ctx, inspect...); err == nil || len(raw) != 0 {
		t.Fatal("business-only inspection accepted a lineage import")
	}
	if raw, err := recoveryHostCommand(t, ctx, importArgs...); err == nil || len(raw) != 0 {
		t.Fatal("repeated lineage import accepted")
	}
	scopes := recoveryHostPrivateJSON(t, filepath.Join(parent, "lineage-scopes.json"), recoveryHostScopes{Scopes: []struct {
		Subject        string `json:"subject"`
		OrganizationID string `json:"organization_id"`
	}{{Subject: "lineage-reader", OrganizationID: org}}})
	authority := filepath.Join(parent, "lineage-authority")
	initArgs := []string{"access-init", "--authority-dir", authority, "--target-config", targetFile, "--scope-file", scopes, "--operator-reference", "independent:test", "--confirm-current-authority"}
	if raw, err := recoveryHostCommand(t, ctx, initArgs...); err == nil || len(raw) != 0 {
		t.Fatal("reader initialized without lineage verification")
	}
	alias := filepath.Join(parent, "backup-alias")
	if err = os.Symlink(bundle, alias); err != nil {
		t.Fatal(err)
	}
	for _, inside := range []string{bundle, alias} {
		bad := append([]string{}, initArgs...)
		bad[2] = filepath.Join(inside, "reader-authority")
		if raw, err := recoveryHostCommand(t, ctx, append(bad, archive...)...); err == nil || len(raw) != 0 {
			t.Fatal("reader initialized authority inside backup")
		}
		if _, err := os.Lstat(bad[2]); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("private authority entered backup")
		}
	}
	insideScopes := filepath.Join(bundle, "transported-scopes.json")
	scopeData, err := os.ReadFile(scopes)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(insideScopes, scopeData, 0600); err != nil {
		t.Fatal(err)
	}
	for _, inside := range []string{bundle, alias} {
		bad := append([]string{}, initArgs...)
		bad[6] = filepath.Join(inside, filepath.Base(insideScopes))
		if raw, err := recoveryHostCommand(t, ctx, append(bad, archive...)...); err == nil || len(raw) != 0 {
			t.Fatal("reader accepted transported scopes")
		}
		if _, err := os.Lstat(authority); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("transported scopes created current authority")
		}
	}
	if err = os.Remove(insideScopes); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryHostCommand(t, ctx, append(initArgs, archive...)...); err != nil {
		t.Fatal("lineage reader initialization", err)
	}
	token := filepath.Join(parent, "lineage-read.token")
	if _, err := recoveryHostCommand(t, ctx, "issue-read-token", "--authority-dir", authority, "--subject", "lineage-reader", "--organization-id", org, "--out", token); err != nil {
		t.Fatal("lineage reader token", err)
	}
	raw, err = recoveryHostCommand(t, ctx, "read-result", "--authority-dir", authority, "--target-config", targetFile, "--token-file", token, "--organization-id", org, "--candidate-id", candidate)
	var result storage.RecoveryResult
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Content != content || !result.Notice.ExecutionHeld || !errors.Is(target.CheckRuntimeAdmission(ctx), storage.RecoveryHeld) {
		t.Fatal("lineage reader result or execution hold", err)
	}
	if entries, err := os.ReadDir(stage); err != nil || len(entries) != 0 {
		t.Fatal("lineage staging remains", err)
	}
}

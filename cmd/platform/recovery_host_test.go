// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
	"filippo.io/age"
	"github.com/jackc/pgx/v5"
)

func recoveryHostPrivateJSON(t *testing.T, path string, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func recoveryHostCommand(t *testing.T, ctx context.Context, args ...string) ([]byte, error) {
	t.Helper()
	cmd := independentCLICommand(t, ctx, "invalid-implicit-database-must-not-be-used", append([]string{"recovery"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if strings.Contains(stderr.String(), "invalid-implicit") {
		t.Fatal("implicit database configuration leaked")
	}
	return stdout.Bytes(), err
}

func TestRecoveryHostAuthorPrivateTokenAndClosedDispatch(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "authority")
	binding := recoveryaccess.Binding{TargetID: "inst_" + strings.Repeat("1", 32), RestoreID: "restore_" + strings.Repeat("2", 32), SnapshotSHA256: strings.Repeat("3", 64)}
	if _, err := recoveryaccess.InitHost(context.Background(), path, binding, []recoveryaccess.Scope{{Subject: "current-owner", OrganizationID: "org-a"}}, "manual:test"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	file := filepath.Join(parent, "read.token")
	args := []string{"issue-read-token", "--authority-dir", path, "--subject", "current-owner", "--organization-id", "org-a", "--out", file}
	out, err := recoveryHostCommand(t, ctx, args...)
	if err != nil {
		t.Fatal("token command failed", err)
	}
	token, err := recoveryaccess.LoadHostToken(file)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(token)) || bytes.Contains(out, []byte(file)) {
		t.Fatal("token or private path leaked")
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = recoveryHostCommand(t, ctx, args...); err == nil {
		t.Fatal("token overwritten")
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing token changed")
	}
	alias := filepath.Join(t.TempDir(), "parent-alias")
	if err = os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(path, "child")
	if err = os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{filepath.Join(path, "pending.json"), filepath.Join(alias, "authority", "pending.json"), filepath.Join(child, "read.token")} {
		changed := append([]string(nil), args...)
		changed[len(changed)-1] = forbidden
		if out, e := recoveryHostCommand(t, ctx, changed...); e == nil || len(out) != 0 {
			t.Fatal("token could overwrite authority namespace")
		}
		if _, e := os.Lstat(forbidden); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("forbidden output created")
		}
	}
	for _, command := range [][]string{{"bootstrap"}, {"worker"}, {"migrate"}, {"list-tasks", "--authority-dir", path}, {"access-replace", "--authority-dir", path, "--expected-epoch", "1"}} {
		out, err = recoveryHostCommand(t, ctx, command...)
		if err == nil || len(out) != 0 {
			t.Fatal("unapproved command/config produced output")
		}
	}
}

func TestRecoveryHostAuthorExplicitDatabaseSelection(t *testing.T) {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
		}
	}
	valid := "postgres://test:synthetic@127.0.0.1:5432/target?sslmode=disable&search_path=public"
	if _, err := explicitRecoveryDSN(valid); err != nil {
		t.Fatal("complete URI refused", err)
	}
	for _, value := range []string{"", "postgres:///target", "host=localhost", "postgres://test@127.0.0.1:5432/target?sslmode=disable", valid + "&service=old", valid + "&host=elsewhere", valid + "&sslmode=prefer"} {
		if _, err := explicitRecoveryDSN(value); err == nil {
			t.Fatal("incomplete or overriding config accepted")
		}
	}
	t.Setenv("PGSERVICE", "do-not-read-saved-service")
	if _, err := explicitRecoveryDSN(valid); err == nil {
		t.Fatal("ambient service allowed")
	}
}

func TestRecoveryHostAuthorImportedResultAcrossProcesses(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" || os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" {
		t.Skip("requires explicit disposable recovery cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	source, _, bootstrap := independentCLIDB(t)
	principal, err := source.Authenticate(ctx, bootstrap.Token, bootstrap.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(10000)
	raw, err := source.CreateAgent(ctx, principal, domain.NewID("key"), domain.CreateAgent{Name: "Recovery CLI fixture", Instructions: "Echo saved input", BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var agent domain.Agent
	if json.Unmarshal(raw, &agent) != nil {
		t.Fatal("agent decode")
	}
	raw, err = source.PublishAgent(ctx, principal, agent.ID, domain.NewID("key"), agent.Version)
	if err != nil || json.Unmarshal(raw, &agent) != nil {
		t.Fatal("publish fixture", err)
	}
	if _, err = source.CreateTask(ctx, principal, domain.NewID("key"), domain.CreateTask{Goal: "Read my restored result", AgentID: agent.ID, BudgetMicroUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	lease, err := source.ClaimFixture(ctx, "recovery-host-cli", time.Minute)
	if err != nil || lease == nil {
		t.Fatal("fixture lease", err)
	}
	content := "Тестовый результат\n" + domain.ExecutionInput(*lease)
	if err = source.CompleteFixture(ctx, lease, content); err != nil {
		t.Fatal(err)
	}
	var candidate string
	if err = source.Pool.QueryRow(ctx, "SELECT id FROM task_candidates WHERE organization_id=$1", principal.OrganizationID).Scan(&candidate); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	report, err := source.ExportRecoverySnapshot(ctx, &snapshot, storage.DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	base, err := storage.Open(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("owned base unavailable")
	}
	defer base.Close()
	schema := domain.NewID("test")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = base.Pool.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := base.Pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); e != nil {
			t.Error("target cleanup failed")
		}
	}()
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("owned DSN invalid")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	target, err := storage.Open(ctx, u.String())
	if err != nil {
		t.Fatal("fresh target unavailable")
	}
	defer target.Close()
	if err = target.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := target.InspectRecoveryTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config := recoveryHostTarget{DatabaseURL: u.String(), TargetInstallationID: state.InstallationID, RestoreID: domain.NewID("restore"), BackupID: report.Metadata.BackupID, SourceID: report.Metadata.SourceID, CutUTC: report.Metadata.CutUTC, SchemaSHA256: report.Metadata.SchemaSHA256, SnapshotSHA256: report.SHA256}
	if _, err = target.PrepareRecoveryTarget(ctx, config.spec()); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err = os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	metadata := recoveryarchive.Metadata{BackupID: config.BackupID, SourceID: config.SourceID, CutUTC: config.CutUTC, SchemaSHA256: config.SchemaSHA256}
	limits := recoveryarchive.Limits{MaxPlaintextBytes: 8 << 20, MaxCiphertextBytes: 9 << 20}
	var sealed bytes.Buffer
	if _, err = recoveryarchive.Seal(ctx, &sealed, bytes.NewReader(snapshot.Bytes()), recoveryarchive.SealOptions{Recipient: identity.Recipient(), SigningKey: key, Metadata: metadata, Limits: limits, StagingParent: parent}); err != nil {
		t.Fatal(err)
	}
	archive, err := recoveryarchive.Open(ctx, bytes.NewReader(sealed.Bytes()), recoveryarchive.OpenOptions{Identity: identity, TrustedSigner: pub, Expected: metadata, Limits: limits, StagingParent: parent})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if _, err = target.ImportRecoverySnapshot(ctx, archive, config.spec(), storage.DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	configFile := recoveryHostPrivateJSON(t, filepath.Join(parent, "target.json"), config)
	scopesFile := filepath.Join(parent, "scopes.json")
	if err = os.WriteFile(scopesFile, []byte(`{"scopes":[{"subject":"current-owner","organization_id":"`+principal.OrganizationID+`"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, "authority")
	initArgs := []string{"access-init", "--authority-dir", directory, "--target-config", configFile, "--scope-file", scopesFile, "--operator-reference", "independent-of-backup:test", "--confirm-current-authority"}
	if out, e := recoveryHostCommand(t, ctx, initArgs...); e != nil || bytes.Contains(out, []byte(config.DatabaseURL)) {
		t.Fatal("explicit init failed or leaked configuration", e)
	}
	tokenFile := filepath.Join(parent, "read.token")
	if _, err = recoveryHostCommand(t, ctx, "issue-read-token", "--authority-dir", directory, "--subject", "current-owner", "--organization-id", principal.OrganizationID, "--out", tokenFile); err != nil {
		t.Fatal("issue command", err)
	}
	listArgs := []string{"list-tasks", "--authority-dir", directory, "--target-config", configFile, "--token-file", tokenFile, "--organization-id", principal.OrganizationID}
	out, err := recoveryHostCommand(t, ctx, listArgs...)
	var page storage.RecoveryTaskPage
	if err != nil || json.Unmarshal(out, &page) != nil || len(page.Tasks) != 1 || page.Tasks[0].CandidateID != candidate {
		t.Fatal("list command did not read imported task", err)
	}
	readArgs := []string{"read-result", "--authority-dir", directory, "--target-config", configFile, "--token-file", tokenFile, "--organization-id", principal.OrganizationID, "--candidate-id", candidate}
	out, err = recoveryHostCommand(t, ctx, readArgs...)
	var result storage.RecoveryResult
	if err != nil || json.Unmarshal(out, &result) != nil || result.Content != content || !result.Notice.ExecutionHeld {
		t.Fatal("result command failed", err)
	}
	if err = os.WriteFile(scopesFile, []byte(`{"scopes":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = recoveryHostCommand(t, ctx, "access-replace", "--authority-dir", directory, "--expected-epoch", "1", "--scope-file", scopesFile, "--operator-reference", "revocation:test", "--confirm-current-authority"); err != nil {
		t.Fatal("revoke command failed", err)
	}
	if out, err = recoveryHostCommand(t, ctx, readArgs...); err == nil || len(out) != 0 {
		t.Fatal("new process read after revocation")
	}
	if !errors.Is(target.CheckRuntimeAdmission(ctx), storage.RecoveryHeld) {
		t.Fatal("CLI cleared hold")
	}
}

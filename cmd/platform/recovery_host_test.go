// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
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
	for _, lostOutput := range []bool{false, true} {
		name := "reported_success"
		if lostOutput {
			name = "lost_command_output"
		}
		t.Run(name, func(t *testing.T) { recoveryHostArchiveRoundTrip(t, lostOutput) })
	}
}

// An output failure after a database operation is distinct from loss of the
// database COMMIT acknowledgment, which is covered by the storage import tests.
type recoveryHostFailedOutput struct{}

func (recoveryHostFailedOutput) Write([]byte) (int, error) { return 0, errRecoveryHostOutput }

var errRecoveryHostOutput = errors.New("synthetic output unavailable")

func recoveryHostArchiveRoundTrip(t *testing.T, lostOutput bool) {
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" || os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" {
		t.Skip("requires explicit disposable recovery cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	source, sourceDSN, bootstrap := independentCLIDB(t)
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
	parent := t.TempDir()
	if err = os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(parent, "stage")
	if err = os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(parent, "archive-keys.json")
	bundle := filepath.Join(parent, "backup")
	sourceFile := recoveryHostPrivateJSON(t, filepath.Join(parent, "source.json"), recoveryDatabaseFile{sourceDSN})
	databaseFile := recoveryHostPrivateJSON(t, filepath.Join(parent, "database.json"), recoveryDatabaseFile{u.String()})
	configFile := filepath.Join(parent, "target.json")
	if _, err = recoveryHostCommand(t, ctx, "backup-keygen", "--out", keys); err != nil {
		t.Fatal("key generation", err)
	}
	out, err := recoveryHostCommand(t, ctx, "backup-create", "--database-config", sourceFile, "--keys-file", keys, "--staging-parent", stage, "--out-dir", bundle)
	var receipt recoveryarchive.HostBackupReceipt
	if err != nil || json.Unmarshal(out, &receipt) != nil || receipt.ReceiptSHA256 == "" {
		t.Fatal("backup command", err)
	}
	selection := []string{"--backup-dir", bundle, "--keys-file", keys, "--staging-parent", stage, "--expected-receipt-sha256", receipt.ReceiptSHA256}
	if _, err = recoveryHostCommand(t, ctx, append([]string{"archive-verify"}, selection...)...); err != nil {
		t.Fatal("verify command", err)
	}
	wrongPin := append([]string(nil), selection...)
	wrongPin[len(wrongPin)-1] = strings.Repeat("0", 64)
	if out, err = recoveryHostCommand(t, ctx, append([]string{"archive-verify"}, wrongPin...)...); err == nil || len(out) != 0 {
		t.Fatal("wrong receipt accepted")
	}
	out, err = recoveryHostCommand(t, ctx, "target-inspect", "--database-config", databaseFile)
	var inspected storage.RecoveryTargetState
	if err != nil || json.Unmarshal(out, &inspected) != nil || inspected.InstallationID != state.InstallationID || inspected.State != "normal" {
		t.Fatal("target selection inspection", err)
	}
	prepareArgs := append([]string{"target-prepare"}, selection...)
	prepareArgs = append(prepareArgs, "--database-config", databaseFile, "--expected-installation-id", state.InstallationID, "--confirm-new-target", "--intent-out", configFile)
	// Rejections must happen before preparation or creation of credentials inside
	// a backup, including when its parent is reached through an alias.
	alias := filepath.Join(t.TempDir(), "parent-alias")
	if err = os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{filepath.Join(bundle, "intent.json"), filepath.Join(alias, "backup", "intent.json")} {
		bad := append([]string(nil), prepareArgs...)
		bad[len(bad)-1] = forbidden
		if out, err = recoveryHostCommand(t, ctx, bad...); err == nil || len(out) != 0 {
			t.Fatal("credential output inside bundle accepted")
		}
		if _, err = os.Lstat(forbidden); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("forbidden intent created")
		}
		if current, e := target.InspectRecoveryTarget(ctx); e != nil || current.State != "normal" {
			t.Fatal("rejected intent changed target", e)
		}
	}
	// A pre-existing output must never be overwritten or trigger preparation.
	existing := recoveryHostPrivateJSON(t, filepath.Join(parent, "existing.json"), map[string]string{"sentinel": "preserve"})
	bad := append([]string(nil), prepareArgs...)
	bad[len(bad)-1] = existing
	if _, err = recoveryHostCommand(t, ctx, bad...); err == nil {
		t.Fatal("existing intent accepted")
	}
	if current, e := target.InspectRecoveryTarget(ctx); e != nil || current.State != "normal" {
		t.Fatal("existing intent prepared target", e)
	}
	if lostOutput {
		if err = runRecoveryHost(ctx, prepareArgs, recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
			t.Fatal("prepare output failure not observed", err)
		}
	} else if _, err = recoveryHostCommand(t, ctx, prepareArgs...); err != nil {
		t.Fatal("prepare command", err)
	}
	var config recoveryHostTarget
	if err = readPrivateJSON(configFile, &config); err != nil || config.ReceiptSHA256 != receipt.ReceiptSHA256 || config.TargetInstallationID != state.InstallationID {
		t.Fatal("durable intent missing or incorrect", err)
	}
	assertImportState := func(want string) {
		t.Helper()
		out, e := recoveryHostCommand(t, ctx, "import-inspect", "--target-config", configFile)
		var status struct {
			State          string `json:"state"`
			TargetID       string `json:"target_id"`
			RestoreID      string `json:"restore_id"`
			SnapshotSHA256 string `json:"snapshot_sha256"`
		}
		if e != nil || json.Unmarshal(out, &status) != nil || status.State != want || status.TargetID != config.TargetInstallationID || status.RestoreID != config.RestoreID || status.SnapshotSHA256 != config.SnapshotSHA256 {
			t.Fatal("exact import inspection", want, e)
		}
	}
	assertImportState("empty_held")
	importArgs := []string{"archive-import", "--backup-dir", bundle, "--keys-file", keys, "--staging-parent", stage, "--target-config", configFile}
	if lostOutput {
		if err = runRecoveryHost(ctx, importArgs, recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
			t.Fatal("import output failure not observed", err)
		}
	} else if _, err = recoveryHostCommand(t, ctx, importArgs...); err != nil {
		t.Fatal("import command", err)
	}
	assertImportState("committed_held")
	if out, err = recoveryHostCommand(t, ctx, importArgs...); err == nil || len(out) != 0 {
		t.Fatal("repeated import accepted")
	}
	assertImportState("committed_held")
	if entries, e := os.ReadDir(stage); e != nil || len(entries) != 0 {
		t.Fatal("plaintext staging remains", e)
	}
	var after bytes.Buffer
	afterReport, err := source.ExportRecoverySnapshot(ctx, &after, storage.DefaultRecoverySnapshotLimits())
	if err != nil || !reflect.DeepEqual(report.Tables, afterReport.Tables) || !reflect.DeepEqual(report.Sequences, afterReport.Sequences) {
		t.Fatal("source business data changed", err)
	}
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
	out, err = recoveryHostCommand(t, ctx, listArgs...)
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

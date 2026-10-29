// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryaccess"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRecoveryReaderAuthorScopedResultsAndRevocation(t *testing.T) {
	recoveryInvariantOwned(t)
	source, _ := testStore(t)
	ctx := context.Background()
	p, _, content := recoveryAuthorPopulate(t, source)
	pOther, _ := identity(t, source, 1_000_000)
	aOther := agent(t, source, pOther)
	_ = task(t, source, pOther, aOther)
	lease, err := source.ClaimFixture(ctx, "recovery-reader-other-org", time.Minute)
	if err != nil || lease == nil || lease.Task.OrganizationID != pOther.OrganizationID {
		t.Fatal("second org fixture failed", err)
	}
	otherContent := "private-other-organization-content"
	if err = source.CompleteFixture(ctx, lease, otherContent); err != nil {
		t.Fatal(err)
	}
	var ownCandidate, otherCandidate string
	if err = source.Pool.QueryRow(ctx, "SELECT id FROM task_candidates WHERE organization_id=$1", p.OrganizationID).Scan(&ownCandidate); err != nil {
		t.Fatal(err)
	}
	if err = source.Pool.QueryRow(ctx, "SELECT id FROM task_candidates WHERE organization_id=$1", pOther.OrganizationID).Scan(&otherCandidate); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if _, err = source.ExportRecoverySnapshot(ctx, &wire, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	archive := recoveryStageAuthorArchive(t, wire.Bytes())
	target, spec, _ := recoveryStageAuthorTarget(t, wire.Bytes())
	receipt, err := target.ImportRecoverySnapshot(ctx, archive, spec, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binding := recoveryaccess.Binding{TargetID: spec.TargetInstallationID, RestoreID: spec.RestoreID, SnapshotSHA256: receipt.SnapshotSHA256}
	policy := recoveryaccess.Policy{Binding: binding, Epoch: 1, Issuers: map[string]ed25519.PublicKey{"current-host": pub}, Scopes: []recoveryaccess.Scope{{Subject: "fresh-operator", OrganizationID: p.OrganizationID}}}
	authority, err := recoveryaccess.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewRecoveryReader(target, authority)
	if err != nil {
		t.Fatal(err)
	}
	claims := recoveryaccess.Claims{Version: 1, Binding: binding, Issuer: "current-host", Subject: "fresh-operator", OrganizationID: p.OrganizationID, Epoch: 1, IssuedAt: time.Now().Unix() - 1, ExpiresAt: time.Now().Unix() + 600}
	token, err := recoveryaccess.Sign(claims, key)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := target.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := recoverySnapshotInventory(ctx, inspection, target.runtimeSchema, true)
	rollbackErr := inspection.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
	before := independentImportRows(t, target, inventory, false)
	seqBefore := independentImportSequenceStates(t, target, receipt.Sequences)
	page, err := reader.ListTasks(ctx, token, p.OrganizationID, "", 1)
	if err != nil || len(page.Tasks) != 1 || page.NextAfterID == "" {
		t.Fatal("scoped pagination first page failed", err)
	}
	second, err := reader.ListTasks(ctx, token, p.OrganizationID, page.NextAfterID, 1)
	if err != nil || len(second.Tasks) != 1 || second.NextAfterID != "" || second.Tasks[0].ID == page.Tasks[0].ID {
		t.Fatal("scoped pagination second page failed", err)
	}
	result, err := reader.Result(ctx, token, p.OrganizationID, ownCandidate)
	if err != nil || result.Content != content || result.ContentDigest != Hash([]byte(content)) || result.ArtifactVersion < 1 {
		t.Fatal("exact historical result unavailable", err)
	}
	if result.Notice.CutUTC != spec.CutUTC || result.Notice.PostCutInterval != "unresolved_open_ended" || !result.Notice.UntrustedHistoricalContent || !result.Notice.ExecutionHeld || page.Notice != result.Notice || second.Notice != result.Notice {
		t.Fatal("recovery meaning not preserved")
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), pOther.OrganizationID) || strings.Contains(string(raw), otherContent) || strings.Contains(string(raw), "cost_history") {
		t.Fatal("other organization/operator report leaked")
	}
	denied, err := reader.Result(ctx, token, p.OrganizationID, otherCandidate)
	if err == nil || !reflect.DeepEqual(denied, RecoveryResult{}) {
		t.Fatal("cross-org candidate leaked")
	}
	deniedPage, err := reader.ListTasks(ctx, token, pOther.OrganizationID, "", 50)
	if !errors.Is(err, recoveryaccess.ErrDenied) || !reflect.DeepEqual(deniedPage, RecoveryTaskPage{}) {
		t.Fatal("cross-org page leaked")
	}
	for _, bad := range []string{"old-imported-bearer", token + "x"} {
		denied, err = reader.Result(ctx, bad, p.OrganizationID, ownCandidate)
		if err == nil || !reflect.DeepEqual(denied, RecoveryResult{}) {
			t.Fatal("invalid credential leaked result")
		}
	}
	for name, mutate := range map[string]func(*recoveryaccess.Binding){
		"target":   func(b *recoveryaccess.Binding) { b.TargetID = "inst_" + strings.Repeat("9", 32) },
		"restore":  func(b *recoveryaccess.Binding) { b.RestoreID = "restore_" + strings.Repeat("9", 32) },
		"snapshot": func(b *recoveryaccess.Binding) { b.SnapshotSHA256 = strings.Repeat("9", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			wrongPolicy := policy
			mutate(&wrongPolicy.Binding)
			wrongAuthority, e := recoveryaccess.New(wrongPolicy)
			if e != nil {
				t.Fatal(e)
			}
			wrongReader, _ := NewRecoveryReader(target, wrongAuthority)
			wrongClaims := claims
			wrongClaims.Binding = wrongPolicy.Binding
			signed, e := recoveryaccess.Sign(wrongClaims, key)
			if e != nil {
				t.Fatal(e)
			}
			value, e := wrongReader.Result(ctx, signed, p.OrganizationID, ownCandidate)
			if e == nil || !reflect.DeepEqual(value, RecoveryResult{}) {
				t.Fatal("valid permit for other target accepted by storage")
			}
		})
	}
	t.Run("untrusted_pool_search_path", func(t *testing.T) {
		outside := domain.NewID("test")
		quoted := pgx.Identifier{outside}.Sanitize()
		if _, e := target.Pool.Exec(ctx, "CREATE SCHEMA "+quoted); e != nil {
			t.Fatal(e)
		}
		defer target.Pool.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		if _, e := target.Pool.Exec(ctx, `CREATE FUNCTION `+quoted+`."left"(text,integer) RETURNS text LANGUAGE sql IMMUTABLE AS $$SELECT 'outside-schema-substitution'::text$$`); e != nil {
			t.Fatal(e)
		}
		cfg := target.Pool.Config()
		cfg.ConnConfig.RuntimeParams["search_path"] = target.runtimeSchema + "," + outside + ",pg_catalog"
		pool, e := pgxpool.NewWithConfig(ctx, cfg)
		if e != nil {
			t.Fatal(e)
		}
		defer pool.Close()
		var poison string
		if e = pool.QueryRow(ctx, "SELECT left('original',240)").Scan(&poison); e != nil || poison != "outside-schema-substitution" {
			t.Fatal("overload fixture not active", e)
		}
		otherStore := &Store{Pool: pool, runtimeSchema: target.runtimeSchema}
		safeReader, e := NewRecoveryReader(otherStore, authority)
		if e != nil {
			t.Fatal(e)
		}
		got, e := safeReader.ListTasks(ctx, token, p.OrganizationID, "", 1)
		if e != nil || !reflect.DeepEqual(got, page) {
			t.Fatal("pool search path changed scoped result", e)
		}
	})
	policy.Epoch++
	policy.Scopes = nil
	if err = authority.Replace(policy); err != nil {
		t.Fatal(err)
	}
	if value, e := reader.Result(ctx, token, p.OrganizationID, ownCandidate); e == nil || !reflect.DeepEqual(value, RecoveryResult{}) {
		t.Fatal("revocation did not prevent read")
	}
	if !errors.Is(target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("reader cleared execution hold")
	}
	independentImportAssertRows(t, before, independentImportRows(t, target, inventory, false))
	if !reflect.DeepEqual(seqBefore, independentImportSequenceStates(t, target, receipt.Sequences)) {
		t.Fatal("read changed sequences")
	}
}

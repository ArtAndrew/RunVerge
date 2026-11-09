// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Existing normal storage helpers create the history; only the owned TEMP copy
// is corrupted. These are author checks, not independent acceptance evidence.
func recoveryAllocationValid(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, q := range recoveryAllocationQueries("pg_temp") {
		var valid bool
		if err := tx.QueryRow(ctx, q.sql).Scan(&valid); err != nil {
			t.Fatalf("allocation query failed: %s", q.name)
		}
		if !valid {
			t.Fatalf("normal history rejected: %s", q.name)
		}
	}
	if err := validateRecoveryAllocations(ctx, tx, "pg_temp"); err != nil {
		t.Fatal("normal allocation history rejected")
	}
}

type recoveryAllocationMutation struct {
	name       string
	statements []string
	args       []any
}

func recoveryAllocationReject(t *testing.T, tx pgx.Tx, cases []recoveryAllocationMutation) {
	t.Helper()
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT allocation_case"); err != nil {
				t.Fatal("allocation savepoint failed")
			}
			defer func() {
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT allocation_case"); err != nil {
					t.Fatal("allocation rollback failed")
				}
			}()
			for _, statement := range tc.statements {
				if tag, err := tx.Exec(ctx, statement, tc.args...); err != nil || tag.RowsAffected() == 0 {
					t.Fatal("negative allocation fixture mutation failed")
				}
			}
			if err := validateRecoveryAllocations(ctx, tx, "pg_temp"); !errors.Is(err, ErrRecoveryAllocation) {
				t.Fatal("corrupt allocation history accepted")
			}
			var one int
			if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Fatal("validator aborted transaction instead of a safe rejection")
			}
		})
	}
}

func TestRecoveryAllocationsAuthorBoundary(t *testing.T) {
	if !errors.Is(validateRecoveryAllocations(nil, nil, "pg_temp"), ErrRecoveryAllocation) {
		t.Fatal("nil API input did not fail closed")
	}
	for _, q := range recoveryAllocationQueries("pg_temp") {
		if regexp.MustCompile(`\{[a-z_]+\}`).MatchString(q.sql) || strings.Contains(q.sql, ";") || !strings.Contains(q.sql, `"pg_temp".`) {
			t.Fatal("unsafe identifier replacement or multiple statements")
		}
		for _, verb := range []string{"INSERT ", "UPDATE ", "DELETE ", "ALTER ", "DROP ", "SET "} {
			if strings.Contains(q.sql, verb) {
				t.Fatal("allocation validator mutates data")
			}
		}
	}
}

func TestRecoveryAllocationsAuthorMaterials(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	base := f.candidate.ID
	m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "Первый UTF-8 черновик\n", base)
	m = independentMaterialRevise(t, f.s, f.p, f.task.ID, m, "Вторая редакция 🌳\n")
	_ = independentMaterialPublish(t, f, m)
	f.refresh(t)
	first := f.candidate.ID
	m = independentMaterialRevise(t, f.s, f.p, f.task.ID, m, "Третья редакция — опубликована дважды\n")
	_ = independentMaterialPublish(t, f, m)
	f.refresh(t)
	second := f.candidate.ID
	_ = independentMaterialCreate(t, f.s, f.p, f.task.ID, "", "") // valid empty draft
	tx := recoveryInvariantStage(t, f.s)
	recoveryAllocationValid(t, tx)
	cases := []recoveryAllocationMutation{
		{"organization_documents", []string{`UPDATE pg_temp.material_storage SET used_documents=used_documents+1`}, nil},
		{"organization_bytes", []string{`UPDATE pg_temp.material_storage SET used_bytes=used_bytes+1`}, nil},
		{"missing_storage", []string{`DELETE FROM pg_temp.material_storage`}, nil},
		{"task_documents", []string{`UPDATE pg_temp.material_task_counts SET documents=documents+1`}, nil},
		{"missing_task_count", []string{`DELETE FROM pg_temp.material_task_counts`}, nil},
		{"publication_count", []string{`UPDATE pg_temp.material_publication_counts SET publications=publications+1`}, nil},
		{"missing_publication_count", []string{`DELETE FROM pg_temp.material_publication_counts`}, nil},
		{"reason_bytes", []string{`UPDATE pg_temp.material_revisions SET reason=reason||'Ю' WHERE version=2`}, nil},
		{"head_behind_history", []string{`UPDATE pg_temp.materials SET head_version=2 WHERE head_version=3`}, nil},
		{"revision_gap", []string{`DELETE FROM pg_temp.material_revisions WHERE version=2`, `UPDATE pg_temp.material_storage SET used_bytes=(SELECT sum(octet_length(content)+octet_length(reason)) FROM pg_temp.material_revisions)+(SELECT sum(octet_length(r.content)) FROM pg_temp.task_candidates c JOIN pg_temp.material_revisions r ON r.material_id=c.material_id AND r.version=c.material_version WHERE c.source='material')`}, nil},
		{"revision_digest", []string{`UPDATE pg_temp.material_revisions SET content_digest=repeat('0',64) WHERE version=1`}, nil},
		{"revision_reason_missing", []string{`UPDATE pg_temp.material_revisions SET reason='' WHERE version=3`, `UPDATE pg_temp.material_storage SET used_bytes=(SELECT sum(octet_length(content)+octet_length(reason)) FROM pg_temp.material_revisions)+(SELECT sum(octet_length(r.content)) FROM pg_temp.task_candidates c JOIN pg_temp.material_revisions r ON r.material_id=c.material_id AND r.version=c.material_version WHERE c.source='material')`}, nil},
		{"first_revision_creator", []string{`UPDATE pg_temp.material_revisions SET principal_id='different_actor' WHERE version=1`}, nil},
		{"publication_raw_bytes", []string{`UPDATE pg_temp.artifacts SET content='same stored digest, wrong bytes' WHERE id IN(SELECT artifact_id FROM pg_temp.task_candidates WHERE source='material')`}, nil},
		{"publication_metadata", []string{`UPDATE pg_temp.artifacts SET name='Different name' WHERE id IN(SELECT artifact_id FROM pg_temp.task_candidates WHERE source='material')`}, nil},
		{"publication_provenance", []string{`UPDATE pg_temp.artifacts SET provenance=jsonb_set(provenance,'{verified}','true') WHERE id IN(SELECT artifact_id FROM pg_temp.task_candidates WHERE source='material')`}, nil},
		{"basis_is_material", []string{`UPDATE pg_temp.task_candidates SET review_basis_candidate_id=$1 WHERE id=$2`}, []any{first, second}},
		{"parent_cycle_with_matching_provenance", []string{`UPDATE pg_temp.task_candidates SET parent_candidate_id=$2 WHERE id=$1`, `UPDATE pg_temp.artifacts SET provenance=jsonb_set(provenance,'{parent_candidate_id}',to_jsonb($2::text)) WHERE id=(SELECT artifact_id FROM pg_temp.task_candidates WHERE id=$1)`}, []any{first, second}},
	}
	recoveryAllocationReject(t, tx, cases)
	t.Run("retired_current_authority_is_not_admission", func(t *testing.T) {
		if _, err := tx.Exec(ctx, `UPDATE pg_temp.memberships SET active=false,security_epoch=security_epoch+1`); err != nil {
			t.Fatal("historical authority setup failed")
		}
		if _, err := tx.Exec(ctx, `UPDATE pg_temp.tasks SET budget_microusd=1`); err != nil {
			t.Fatal("lower budget setup failed")
		}
		recoveryAllocationValid(t, tx)
	})
	t.Run("cancelled", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if !errors.Is(validateRecoveryAllocations(cancelled, tx, "pg_temp"), context.Canceled) {
			t.Fatal("cancellation lost")
		}
	})
}

func TestRecoveryAllocationsAuthorAutomation(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentAutomationSetup(t, nil)
	ctx := context.Background()
	f.executeModel(t)
	first := f.grant(t, func(g *domain.CreateTaskAutomation) { g.MaxChargeMicroUSD = 300 })
	f.reconcile(t)
	v := f.executeRead(t)
	evidence := independentEvidence(17)
	evidence.Source = f.tool.Tool.Provider
	if _, err := f.s.AccountCost(ctx, f.p.OrganizationID, v.Execution.ReservationID, evidence); err != nil {
		t.Fatal("settled tool fixture failed")
	}
	f.reconcile(t) // first grant cannot afford its cumulative120+next-model300
	state, err := f.s.Automation(ctx, f.p, f.task.ID)
	if err != nil || state.Usage.AllocatedMicroUSD != 120 {
		t.Fatal("first grant allocation fixture failed")
	}
	if state.Grant.Status == "active" {
		if _, err = f.s.RevokeAutomation(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: state.Grant.ID, ExpectedVersion: state.Grant.Version, Reason: "Explicit renewal after first admission"}); err != nil {
			t.Fatal("grant retirement failed")
		}
	}
	_ = f.grant(t, nil)
	f.reconcile(t)
	f.executeModel(t)
	f.reconcile(t)
	tx := recoveryInvariantStage(t, f.s)
	// Final420 exceeds the first immutable grant300 legitimately; billed17
	// does not replace the original tool ceiling120.
	recoveryAllocationValid(t, tx)
	cases := []recoveryAllocationMutation{
		{"tool_own_count", []string{`UPDATE pg_temp.tool_execution_counts SET own_executions=own_executions+1`}, nil},
		{"tool_root_count", []string{`UPDATE pg_temp.tool_execution_counts SET tree_executions=tree_executions+1`}, nil},
		{"tool_missing_count", []string{`DELETE FROM pg_temp.tool_execution_counts`}, nil},
		{"model_own_count", []string{`UPDATE pg_temp.model_turn_counts SET own_turns=own_turns+1`}, nil},
		{"model_root_count", []string{`UPDATE pg_temp.model_turn_counts SET tree_turns=tree_turns+1`}, nil},
		{"model_missing_count", []string{`DELETE FROM pg_temp.model_turn_counts`}, nil},
		{"model_ordinal_gap", []string{`UPDATE pg_temp.model_turns SET ordinal=3 WHERE ordinal=2`}, nil},
		{"automation_model_count", []string{`UPDATE pg_temp.task_automation_usage SET model_turns=model_turns+1`}, nil},
		{"automation_tool_count", []string{`UPDATE pg_temp.task_automation_usage SET tool_executions=tool_executions+1`}, nil},
		{"automation_refund_to_bill", []string{`UPDATE pg_temp.task_automation_usage SET allocated_microusd=317`}, nil},
		{"automation_missing_usage", []string{`DELETE FROM pg_temp.task_automation_usage`}, nil},
		{"per_grant_exposure", []string{`UPDATE pg_temp.task_automation_grants SET grant_body=jsonb_set(grant_body,'{max_charge_microusd}','"1"') WHERE id=$1`}, []any{first.Grant.ID}},
		{"grant_owner", []string{`UPDATE pg_temp.task_automation_grants SET owner_id='different_actor',grant_body=jsonb_set(grant_body,'{owner_id}','"different_actor"')`}, nil},
		{"grant_model_pin", []string{`UPDATE pg_temp.task_automation_grants SET grant_body=jsonb_set(grant_body,'{model_connection_version}','2')`}, nil},
		{"tool_creator_binding", []string{`UPDATE pg_temp.tool_executions SET created_by='different_actor'`}, nil},
		{"model_actor_binding", []string{`UPDATE pg_temp.operations SET authority_principal_id='different_actor' WHERE id IN(SELECT operation_id FROM pg_temp.model_turns WHERE automation_grant_id IS NOT NULL)`}, nil},
		{"model_ceiling_missing", []string{`UPDATE pg_temp.model_connections SET profile=profile-'max_charge_microusd'`}, nil},
		{"model_ceiling_wrong_type", []string{`UPDATE pg_temp.model_connections SET profile=jsonb_set(profile,'{max_charge_microusd}','"300"')`}, nil},
		{"model_ceiling_unbounded", []string{`UPDATE pg_temp.model_connections SET profile=jsonb_set(profile,'{max_charge_microusd}','999999999999999999999999999999999999')`}, nil},
		{"tool_ceiling_missing", []string{`UPDATE pg_temp.tool_executions SET intent=intent-'max_charge_microusd'`}, nil},
		{"tool_ceiling_bad_decimal", []string{`UPDATE pg_temp.tool_executions SET intent=jsonb_set(intent,'{max_charge_microusd}','"01"')`}, nil},
	}
	recoveryAllocationReject(t, tx, cases)
	t.Run("retired_profiles_and_stale_grants", func(t *testing.T) {
		for _, statement := range []string{`UPDATE pg_temp.model_connections SET active=false`, `UPDATE pg_temp.tool_connections SET active=false`, `UPDATE pg_temp.memberships SET active=false,security_epoch=security_epoch+1`} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				t.Fatal("retired profile setup failed")
			}
		}
		recoveryAllocationValid(t, tx)
	})
}

func TestRecoveryAllocationsAuthorConfiguration(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	a, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	bAccount := independentSecurityService(t, f.s, f.owner, "member")
	b, _, _ := independentSecurityKey(t, f.s, bAccount, "member", time.Hour)
	x, _ := independentConfigurationInstallModel(t, f, "http://127.0.0.1:9/never-called")
	ref := domain.ConfigurationDestinationRef{ID: x.Destination.Destination.ID, Version: 1}
	root := independentConfigurationGrant(t, f, f.owner, a.ID, nil, 4, 4, 100, ref)
	leaf := independentConfigurationGrant(t, f, a, b.ID, &root, 3, 3, 50, ref)
	active := true
	plan, _ := independentConfigurationPlan(t, f, b, leaf.ID, domain.ConfigurationOperationInput{ID: "model", DestinationID: ref.ID, DestinationVersion: 1, Active: &active})
	plan = independentConfigurationApply(t, f, b, plan, independentConfigurationValidate(t, f, b, plan))
	_, _ = independentConfigurationStart(t, f, b, plan, "model", domain.NewID("key"), 17)
	// A retained planned-but-never-applied operation still consumes one plan.
	_, _ = independentConfigurationPlan(t, f, a, root.ID, domain.ConfigurationOperationInput{ID: "model", DestinationID: ref.ID, DestinationVersion: 1, ExpectedResourceVersion: 1, Active: &active})
	tx := recoveryInvariantStage(t, f.s)
	recoveryAllocationValid(t, tx)
	cases := []recoveryAllocationMutation{
		{"ancestor_plan_count", []string{`UPDATE pg_temp.configuration_grants SET used_plans=used_plans-1 WHERE id=$1`}, []any{root.ID}},
		{"leaf_plan_count", []string{`UPDATE pg_temp.configuration_grants SET used_plans=used_plans-1 WHERE id=$1`}, []any{leaf.ID}},
		{"ancestor_verification_count", []string{`UPDATE pg_temp.configuration_grants SET used_verifications=0 WHERE id=$1`}, []any{root.ID}},
		{"ancestor_ceiling_sum", []string{`UPDATE pg_temp.configuration_grants SET allocated_charge_microusd=16 WHERE id=$1`}, []any{root.ID}},
		{"leaf_ceiling_sum", []string{`UPDATE pg_temp.configuration_grants SET allocated_charge_microusd=16 WHERE id=$1`}, []any{leaf.ID}},
		{"grant_cycle", []string{`UPDATE pg_temp.configuration_grants SET parent_grant_id=id,depth=1 WHERE id=$1`}, []any{root.ID}},
		{"child_wrong_depth", []string{`UPDATE pg_temp.configuration_grants SET depth=2 WHERE id=$1`}, []any{leaf.ID}},
		{"child_escalated_ceiling", []string{`UPDATE pg_temp.configuration_grants SET max_allocated_charge_microusd=101 WHERE id=$1`}, []any{leaf.ID}},
		{"child_wrong_issuer", []string{`UPDATE pg_temp.configuration_grants SET issuer_id=subject_id,body=jsonb_set(body,'{issuer_id}',to_jsonb(subject_id)) WHERE id=$1`}, []any{leaf.ID}},
		{"plan_wrong_grant_subject", []string{`UPDATE pg_temp.configuration_plans SET actor_id=$1,body=jsonb_set(body,'{actor_id}',to_jsonb($1::text)) WHERE grant_id=$2`}, []any{f.owner.ID, leaf.ID}},
		{"verification_wrong_grant", []string{`UPDATE pg_temp.configuration_verifications SET grant_id=$1`}, []any{root.ID}},
		{"verification_ceiling_null", []string{`UPDATE pg_temp.configuration_verifications SET body=jsonb_set(body,'{max_charge_microusd}','null')`}, nil},
		{"verification_ceiling_wrong_type", []string{`UPDATE pg_temp.configuration_verifications SET body=jsonb_set(body,'{max_charge_microusd}','17')`}, nil},
		{"verification_ceiling_noncanonical", []string{`UPDATE pg_temp.configuration_verifications SET body=jsonb_set(body,'{max_charge_microusd}','"+17"')`}, nil},
	}
	recoveryAllocationReject(t, tx, cases)
	t.Run("retired_delegation_history", func(t *testing.T) {
		for _, statement := range []string{`UPDATE pg_temp.configuration_grants SET revoked=true`, `UPDATE pg_temp.configuration_destinations SET active=false`, `UPDATE pg_temp.credential_purpose_profiles SET active=false`, `UPDATE pg_temp.memberships SET active=false,security_epoch=security_epoch+1`} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				t.Fatal("retired delegation setup failed")
			}
		}
		recoveryAllocationValid(t, tx)
	})
}

func TestRecoveryAllocationsAuthorChildCounters(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentAutomationSetup(t, nil)
	ctx := context.Background()
	if _, err := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); err != nil {
		t.Fatal("unused task cancellation failed")
	}
	team := teamAgent(t, f.s, f.p, "parallel", "all", 2, teamMember("first", f.agent), teamMember("second", f.agent))
	root := task(t, f.s, f.p, team)
	teamTick(t, f.s)
	teamTick(t, f.s)
	children, _, err := f.s.Children(ctx, f.p, root.ID, "", 100)
	if err != nil || len(children) != 2 {
		t.Fatal("real team children unavailable")
	}
	// Claim order is creation order; discover the actual queued child first.
	if err = f.s.Pool.QueryRow(ctx, `SELECT t.id FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id WHERE t.root_id=$1 AND o.status='queued' ORDER BY o.created_at,o.id LIMIT 1`, root.ID).Scan(&f.task.ID); err != nil {
		t.Fatal("queued team child unavailable")
	}
	f.executeModel(t)
	_ = f.grant(t, nil)
	f.reconcile(t)
	_ = f.executeRead(t)
	tx := recoveryInvariantStage(t, f.s)
	recoveryAllocationValid(t, tx)
	recoveryAllocationReject(t, tx, []recoveryAllocationMutation{
		{"nonroot_tool_tree_count", []string{`UPDATE pg_temp.tool_execution_counts SET tree_executions=1 WHERE task_id=$1`}, []any{f.task.ID}},
		{"nonroot_model_tree_count", []string{`UPDATE pg_temp.model_turn_counts SET tree_turns=1 WHERE task_id=$1`}, []any{f.task.ID}},
		{"root_tool_own_count", []string{`UPDATE pg_temp.tool_execution_counts SET own_executions=1 WHERE task_id=$1`}, []any{root.ID}},
		{"root_model_own_count", []string{`UPDATE pg_temp.model_turn_counts SET own_turns=1 WHERE task_id=$1`}, []any{root.ID}},
		{"root_tool_projection", []string{`UPDATE pg_temp.tool_execution_counts SET tree_executions=0 WHERE task_id=$1`}, []any{root.ID}},
		{"root_model_projection", []string{`UPDATE pg_temp.model_turn_counts SET tree_turns=0 WHERE task_id=$1`}, []any{root.ID}},
	})
}

// Reuses the established SQL013 historical-fixture recipe. This is an author
// acceptance fixture for the new allocation checker, not a historical binary
// or independent runtime claim. All migrations run only in the owned cluster.
func TestRecoveryAllocationsAuthorLegacyRootAboveAdmissionCap(t *testing.T) {
	recoveryInvariantOwned(t)
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for actual PostgreSQL upgrade")
	}
	base, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal("open isolated upgrade base")
	}
	schema := domain.NewID("independent_upgrade")
	if _, err := base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		base.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		base.Close()
		t.Fatal("parse isolated upgrade configuration")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		base.Close()
		t.Fatal("open isolated upgrade schema")
	}
	t.Cleanup(func() {
		s.Close()
		_, err := base.Pool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if err != nil {
			t.Error(err)
		}
	})
	// Stage the actual immutable migrations through013, then construct a legal
	// old-schema tree with129 historical model operations. No inference occurs.
	// The public0.6 tree lifetime allowed up to384 attempts, above the new128.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "CREATE TABLE schema_migrations(name text PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		t.Fatal(err)
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, name := range files {
		if name >= "migrations/014_" {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatal("stage immutable migration", name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)", name, Hash(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// This is an exact013 staging fixture; current authentication requires016.
	// Current constructors run only in the test-only old-gate callback; the
	// fixture does not claim actual execution of a historical binary.
	var bootstrap Bootstrap
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		bootstrap, err = s.Bootstrap(ctx, "Historical independent upgrade", 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
	})
	p := domain.Principal{ID: bootstrap.PrincipalID, OrganizationID: bootstrap.OrganizationID, Role: "owner", TokenHash: Hash([]byte(bootstrap.Token)), CredentialKind: "bearer"}
	profile := domain.ModelConnection{ID: "independent-upgrade-model", OrganizationID: p.OrganizationID, Version: 1, Model: "independent-upgrade-model", ProviderAccount: "independent-upgrade-account", PriceVersion: "independent-upgrade-price", CredentialEnvironment: "PLATFORM_MODEL_KEY_UPGRADE", Endpoint: "https://example.invalid/model", MaxChargeMicroUSD: 300, MaxInputTokens: 100, MaxOutputTokens: 64, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 65536, MaxOutputBytes: 4096, TimeoutMilliseconds: 1000, BoundAssumptions: "Historical synthetic state; no external request", TokenLimitField: "max_tokens", Active: true}
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		if _, err := s.InstallModelConnection(ctx, profile); err != nil {
			t.Fatal(err)
		}
	})
	rootAgent, modelAgent, rootID := domain.NewID("agt"), domain.NewID("agt"), domain.NewID("tsk")
	if _, err := s.Pool.Exec(ctx, "INSERT INTO agents(organization_id,id,name,instructions,model,tools,budget_microusd,published) VALUES($1,$2,'Historical root','Synthetic historical root',$3,'[]',100000,true),($1,$4,'Historical model','Exact historical instructions',$5,'[]',100000,true)", p.OrganizationID, rootAgent, domain.FixtureModel, modelAgent, profile.Model); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd) VALUES($1,$2,$3,$4,1,'Historical root','queued',100000)", p.OrganizationID, rootID, p.ID, rootAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status) VALUES($1,$2,$3,$4,$5,1,'queued')", p.OrganizationID, domain.NewID("run"), rootID, p.ID, rootAgent); err != nil {
		t.Fatal(err)
	}
	var lastChild string
	for i := 0; i < 65; i++ {
		child := domain.NewID("tsk")
		if _, err := s.Pool.Exec(ctx, "INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd,parent_id,model_connection_id,model_connection_version) VALUES($1,$2,$3,$4,1,'Historical model child','queued',10000,$5,$6,1)", p.OrganizationID, child, p.ID, modelAgent, rootID, profile.ID); err != nil {
			t.Fatal(err)
		}
		n := 2
		if i == 64 {
			n = 1
		}
		lastChild = child
		for attempt := 1; attempt <= n; attempt++ {
			runID := domain.NewID("run")
			if _, err := s.Pool.Exec(ctx, "INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status,attempt) VALUES($1,$2,$3,$4,$5,1,'blocked',$6)", p.OrganizationID, runID, child, p.ID, modelAgent, attempt); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(ctx, "INSERT INTO operations(organization_id,id,task_id,run_id,status) VALUES($1,$2,$3,$4,'blocked')", p.OrganizationID, domain.NewID("op"), child, runID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("upgrade erased/refused valid old history above new cap", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("upgrade replay was not stable", err)
	}
	var tree, turns, own int
	if err := s.Pool.QueryRow(ctx, "SELECT tree_turns,(SELECT count(*) FROM model_turns WHERE organization_id=$1),(SELECT own_turns FROM model_turn_counts WHERE organization_id=$1 AND task_id=$3) FROM model_turn_counts WHERE organization_id=$1 AND task_id=$2", p.OrganizationID, rootID, lastChild).Scan(&tree, &turns, &own); err != nil || tree != 129 || turns != 129 || own != 1 {
		t.Fatal("historical model turns clamped, reset or omitted", tree, turns, own, err)
	}
	stage := recoveryInvariantStage(t, s)
	recoveryAllocationValid(t, stage)
	recoveryAllocationReject(t, stage, []recoveryAllocationMutation{
		{"clamped_historical_root", []string{`UPDATE pg_temp.model_turn_counts SET tree_turns=128 WHERE task_id=$1`}, []any{rootID}},
	})
}

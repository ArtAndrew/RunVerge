// SPDX-License-Identifier: Elastic-2.0
package storage

// These regressions intentionally enter through SQL as well as the trusted
// profile installers. They prove persisted defenses independently of the new
// public configuration command validators. Every database is a disposable
// testStore schema; no external endpoint is contacted.
import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type independentConfigurationSchema struct {
	s       *Store
	owner   domain.Principal
	service domain.ServiceAccount
	purpose domain.CredentialPurposeProfile
	dest    domain.ConfigurationDestinationProfile
	scope   domain.ConfigurationScope
}

type independentConfigurationSQL interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func independentConfigurationSchemaJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func independentConfigurationSchemaFixture(t *testing.T) independentConfigurationSchema {
	t.Helper()
	s := independentConfigurationSchemaStore(t)
	p, _ := identity(t, s, 1000000)
	f := independentConfigurationSchema{s: s, owner: p}
	f.service = independentSecurityService(t, s, p, "member")
	endpoint := "https://independent-configuration.example.invalid/mcp"
	var err error
	f.purpose, err = s.InstallCredentialPurposeProfile(context.Background(), domain.CredentialPurposeProfile{
		ID: domain.NewID("purpose"), OrganizationID: p.OrganizationID, Version: 1, Kind: "mcp",
		EndpointDigest: Hash([]byte(endpoint)), CredentialEnvironment: "PLATFORM_MCP_KEY_INDEPENDENT_SCHEMA", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.dest, err = s.InstallConfigurationDestination(context.Background(), domain.ConfigurationDestinationProfile{
		Destination: domain.ConfigurationDestination{ID: domain.NewID("destination"), OrganizationID: p.OrganizationID,
			Version: 1, Kind: "mcp", Name: "Independent SQL-only discovery slot", TargetID: domain.NewID("mcp"),
			CredentialPurposeID: f.purpose.ID, CredentialPurposeVersion: 1, RequiresApproval: true,
			DiscoveryUnmeteredApproved: true, Active: true},
		MCP: &domain.MCPConnectionProfile{Endpoint: endpoint, TimeoutSeconds: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := f.dest.Destination
	f.scope = domain.ConfigurationScope{DestinationID: d.ID, DestinationVersion: 1, Kind: "mcp", TargetID: d.TargetID,
		CredentialPurposeID: d.CredentialPurposeID, CredentialPurposeVersion: 1}
	return f
}

func independentConfigurationSchemaStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for isolated configuration schema defenses")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal("independent database unavailable")
	}
	schema := domain.NewID("test_config_schema")
	if _, err = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		base.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid isolated test configuration")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal("cannot open isolated configuration schema")
	}
	// Register before Migrate: even a syntax failure must remove the empty
	// disposable schema after its complete migration transaction rolls back.
	t.Cleanup(func() {
		s.Close()
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, err := base.Pool.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if err != nil {
			t.Error(err)
		}
	})
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("atomic isolated migration", err)
	}
	body, err := migrations.ReadFile("migrations/019_configuration_plane.sql")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("first successful isolated019 source checksum", Hash(body))
	return s
}

func independentConfigurationSchemaDenied(t *testing.T, err error) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Fatalf("persisted defense wanted23514, got %v", err)
	}
}

func (f independentConfigurationSchema) grant(issuer, subject string, parent *domain.CapabilityEnvelope) domain.CapabilityEnvelope {
	now := time.Now().UTC().Truncate(time.Microsecond)
	g := domain.CapabilityEnvelope{ID: domain.NewID("cfggrant"), OrganizationID: f.owner.OrganizationID,
		Audience: domain.ConfigurationAudience, IssuerID: issuer, SubjectID: subject, Version: 1,
		Actions: []string{"inspect", "plan", "validate", "apply", "verify", "delegate"}, Scopes: []domain.ConfigurationScope{f.scope},
		MaxPlans: 3, MaxVerifications: 3, MaxAllocatedChargeMicroUSD: 17, Status: "active", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if parent != nil {
		g.ParentGrantID, g.Depth = parent.ID, parent.Depth+1
		g.ExpiresAt = parent.ExpiresAt.Add(-time.Minute)
	}
	return g
}

func independentConfigurationSchemaInsertGrant(t *testing.T, db independentConfigurationSQL, g domain.CapabilityEnvelope) error {
	t.Helper()
	_, err := db.Exec(context.Background(), `INSERT INTO configuration_grants(organization_id,id,issuer_id,subject_id,parent_grant_id,depth,
 issuer_authority_epoch,subject_authority_epoch,issuer_policy_epoch,subject_policy_epoch,body,version,revoked,
 max_plans,used_plans,max_verifications,used_verifications,max_allocated_charge_microusd,allocated_charge_microusd,created_at,expires_at)
 VALUES($1,$2,$3,$4,NULLIF($5,''),$6,777,777,777,777,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		g.OrganizationID, g.ID, g.IssuerID, g.SubjectID, g.ParentGrantID, g.Depth, independentConfigurationSchemaJSON(t, g),
		g.Version, g.Revoked, g.MaxPlans, g.UsedPlans, g.MaxVerifications, g.UsedVerifications,
		g.MaxAllocatedChargeMicroUSD, g.AllocatedChargeMicroUSD, g.CreatedAt, g.ExpiresAt)
	return err
}

func (f independentConfigurationSchema) plan(t *testing.T) domain.ConfigurationPlan {
	t.Helper()
	var revision int64
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT revision FROM configuration_revisions WHERE organization_id=$1", f.owner.OrganizationID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := domain.NewID("cfgplan")
	return domain.ConfigurationPlan{ID: id, OrganizationID: f.owner.OrganizationID, ActorID: f.owner.ID, Version: 1,
		Status: "planned", BaseRevision: revision, Digest: Hash([]byte("Independent trusted SQL fixture intent " + id)),
		Operations: []domain.ConfigurationOperation{{ID: "connect", Scope: f.scope, Active: true}},
		Diff:       []domain.ConfigurationDiff{{OperationID: "connect", Kind: "mcp", TargetID: f.scope.TargetID, NewVersion: 1, Active: true, RetiredVersions: []int{}}},
		Reason:     "Independent SQL defense intent", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
}

func independentConfigurationSchemaInsertPlan(t *testing.T, db independentConfigurationSQL, p domain.ConfigurationPlan) error {
	t.Helper()
	_, err := db.Exec(context.Background(), `INSERT INTO configuration_plans(organization_id,id,actor_id,grant_id,actor_kind,admitted_role,
 actor_authority_epoch,actor_policy_epoch,base_revision,digest,body,version,status,created_at,expires_at,applied_at,applied_revision,receipt_id)
 VALUES($1,$2,$3,NULLIF($4,''),'human','owner',777,777,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,0),NULLIF($14,''))`,
		p.OrganizationID, p.ID, p.ActorID, p.GrantID, p.BaseRevision, p.Digest, independentConfigurationSchemaJSON(t, p),
		p.Version, p.Status, p.CreatedAt, p.ExpiresAt, p.AppliedAt, p.AppliedRevision, p.ReceiptID)
	return err
}

func (f independentConfigurationSchema) validation(p domain.ConfigurationPlan) domain.ConfigurationValidation {
	return domain.ConfigurationValidation{ID: domain.NewID("cfgvalidation"), OrganizationID: p.OrganizationID,
		PlanID: p.ID, PlanVersion: p.Version, PlanDigest: p.Digest, BaseRevision: p.BaseRevision, CurrentRevision: p.BaseRevision,
		Valid: true, Diff: p.Diff, CreatedAt: time.Now().UTC(), Decision: domain.PolicyDecision{ID: domain.NewID("cfgdecision"),
			OrganizationID: p.OrganizationID, Action: "apply", TargetID: p.ID, Outcome: "requires_approval",
			ConfigurationRevision: p.BaseRevision, ReasonCodes: []string{}, Obligations: []string{"owner_approval_required"}, EvaluatedAt: time.Now().UTC()}}
}

func independentConfigurationSchemaInsertValidation(t *testing.T, db independentConfigurationSQL, v domain.ConfigurationValidation) error {
	t.Helper()
	_, err := db.Exec(context.Background(), "INSERT INTO configuration_validations(organization_id,id,plan_id,body) VALUES($1,$2,$3,$4)",
		v.OrganizationID, v.ID, v.PlanID, independentConfigurationSchemaJSON(t, v))
	return err
}

func independentConfigurationSchemaInsertApproval(t *testing.T, db independentConfigurationSQL, p domain.ConfigurationPlan, validationID, principal string) error {
	t.Helper()
	a := domain.ConfigurationApproval{ID: domain.NewID("cfgapproval"), OrganizationID: p.OrganizationID,
		PlanID: p.ID, PlanDigest: p.Digest, ValidationID: validationID, PrincipalID: principal, Reason: "Independent exact owner approval", CreatedAt: time.Now().UTC()}
	_, err := db.Exec(context.Background(), `INSERT INTO configuration_approvals(organization_id,id,plan_id,validation_id,principal_id,authority_epoch,policy_epoch,body)
 VALUES($1,$2,$3,$4,$5,777,777,$6)`, a.OrganizationID, a.ID, a.PlanID, a.ValidationID, a.PrincipalID, independentConfigurationSchemaJSON(t, a))
	return err
}

func (f independentConfigurationSchema) bindMCP(t *testing.T, db independentConfigurationSQL, profile domain.MCPConnectionProfile) error {
	t.Helper()
	if _, err := db.Exec(context.Background(), "INSERT INTO mcp_connections(organization_id,id,version,profile,active) VALUES($1,$2,1,$3,true)",
		f.owner.OrganizationID, f.scope.TargetID, independentConfigurationSchemaJSON(t, profile)); err != nil {
		return err
	}
	_, err := db.Exec(context.Background(), `INSERT INTO configuration_resource_bindings(organization_id,kind,target_id,resource_version,destination_id,destination_version,credential_purpose_id,credential_purpose_version)
 VALUES($1,'mcp',$2,1,$3,1,$4,1)`, f.owner.OrganizationID, f.scope.TargetID, f.scope.DestinationID, f.scope.CredentialPurposeID)
	return err
}

func (f independentConfigurationSchema) apply(t *testing.T, p domain.ConfigurationPlan) domain.ConfigurationPlan {
	t.Helper()
	tx, err := f.s.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err = f.bindMCP(t, tx, *f.dest.MCP); err != nil {
		t.Fatal(err)
	}
	p.ReceiptID = domain.NewID("cfgreceipt")
	var at time.Time
	if err = tx.QueryRow(context.Background(), `UPDATE configuration_plans SET status='applied',receipt_id=$3,
 applied_revision=(SELECT revision FROM configuration_revisions WHERE organization_id=$1)
 WHERE organization_id=$1 AND id=$2 RETURNING version,applied_at,applied_revision`, p.OrganizationID, p.ID, p.ReceiptID).Scan(&p.Version, &at, &p.AppliedRevision); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Status, p.AppliedAt = "applied", &at
	return p
}

func TestIndependentConfigurationSchemaRejectsNullProfilesAndAttenuationForgery(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	t.Run("missing_private_descriptor", func(t *testing.T) {
		_, err := f.s.Pool.Exec(ctx, `INSERT INTO credential_purpose_profiles(organization_id,id,version,kind,endpoint_digest,profile,active)
 VALUES($1,$2,1,'mcp',$3,'{}',true)`, f.owner.OrganizationID, domain.NewID("purpose"), Hash([]byte("missing")))
		independentConfigurationSchemaDenied(t, err)
	})
	t.Run("destination_cannot_mix_valid_purpose_with_other_endpoint", func(t *testing.T) {
		d := f.dest
		d.Destination.ID = domain.NewID("destination")
		mcp := *d.MCP
		mcp.Endpoint = "https://different.example.invalid/mcp"
		d.MCP = &mcp
		_, err := f.s.Pool.Exec(ctx, `INSERT INTO configuration_destinations(organization_id,id,version,kind,target_id,credential_purpose_id,credential_purpose_version,profile,active)
 VALUES($1,$2,1,'mcp',$3,$4,1,$5,true)`, f.owner.OrganizationID, d.Destination.ID, d.Destination.TargetID, d.Destination.CredentialPurposeID, independentConfigurationSchemaJSON(t, d))
		independentConfigurationSchemaDenied(t, err)
	})
	root := f.grant(f.owner.ID, f.service.PrincipalID, nil)
	if err := independentConfigurationSchemaInsertGrant(t, f.s.Pool, root); err != nil {
		t.Fatal(err)
	}
	var authorityEpoch int64
	if err := f.s.Pool.QueryRow(ctx, "SELECT issuer_authority_epoch FROM configuration_grants WHERE organization_id=$1 AND id=$2", root.OrganizationID, root.ID).Scan(&authorityEpoch); err != nil || authorityEpoch != 1 {
		t.Fatal("SQL did not derive actual authority rather than supplied777", authorityEpoch, err)
	}
	for _, name := range []string{"self_parent", "expanded_actions", "expanded_cap", "later_expiry", "initial_spent", "initial_revoked"} {
		t.Run(name, func(t *testing.T) {
			g := f.grant(f.service.PrincipalID, f.service.PrincipalID, &root)
			switch name {
			case "self_parent":
				g.ParentGrantID = g.ID
			case "expanded_actions":
				g.Actions = append(g.Actions, "members.update")
			case "expanded_cap":
				g.MaxPlans = root.MaxPlans + 1
			case "later_expiry":
				g.ExpiresAt = root.ExpiresAt.Add(time.Minute)
			case "initial_spent":
				g.UsedPlans = 1
			case "initial_revoked":
				g.Revoked = true
			}
			independentConfigurationSchemaDenied(t, independentConfigurationSchemaInsertGrant(t, f.s.Pool, g))
		})
	}
	child := f.grant(f.service.PrincipalID, f.service.PrincipalID, &root)
	child.Actions = []string{"inspect", "plan"}
	if err := independentConfigurationSchemaInsertGrant(t, f.s.Pool, child); err != nil {
		t.Fatal("genuine narrower child must remain usable", err)
	}
	if _, err := f.s.Pool.Exec(ctx, "UPDATE configuration_grants SET used_plans=1 WHERE organization_id=$1 AND id=$2", root.OrganizationID, root.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.Pool.Exec(ctx, "UPDATE configuration_grants SET used_plans=0 WHERE organization_id=$1 AND id=$2", root.OrganizationID, root.ID)
	independentConfigurationSchemaDenied(t, err)
	if _, err = f.s.Pool.Exec(ctx, "UPDATE configuration_grants SET revoked=true WHERE organization_id=$1 AND id=$2", root.OrganizationID, root.ID); err != nil {
		t.Fatal(err)
	}
	var live bool
	if err = f.s.Pool.QueryRow(ctx, "SELECT configuration_grant_is_live($1,$2,$3,'plan')", child.OrganizationID, child.ID, child.SubjectID).Scan(&live); err != nil || live {
		t.Fatal("child revived revoked parent", live, err)
	}
	_, err = f.s.Pool.Exec(ctx, "UPDATE configuration_grants SET revoked=false WHERE organization_id=$1 AND id=$2", root.OrganizationID, root.ID)
	independentConfigurationSchemaDenied(t, err)
}

func TestIndependentConfigurationSchemaApprovalBindsExactValidationAndResult(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	p, other := f.plan(t), f.plan(t)
	for _, v := range []domain.ConfigurationPlan{p, other} {
		if err := independentConfigurationSchemaInsertPlan(t, f.s.Pool, v); err != nil {
			t.Fatal(err)
		}
	}
	v, otherValidation := f.validation(p), f.validation(other)
	for _, value := range []domain.ConfigurationValidation{v, otherValidation} {
		if err := independentConfigurationSchemaInsertValidation(t, f.s.Pool, value); err != nil {
			t.Fatal("requires_approval must still be statically valid", err)
		}
	}
	t.Run("foreign_validation_in_same_org", func(t *testing.T) {
		independentConfigurationSchemaDenied(t, independentConfigurationSchemaInsertApproval(t, f.s.Pool, p, otherValidation.ID, f.owner.ID))
	})
	t.Run("service_never_human_approver", func(t *testing.T) {
		independentConfigurationSchemaDenied(t, independentConfigurationSchemaInsertApproval(t, f.s.Pool, p, v.ID, f.service.PrincipalID))
	})
	t.Run("forged_policy_target", func(t *testing.T) {
		bad := f.validation(p)
		bad.Decision.TargetID = other.ID
		independentConfigurationSchemaDenied(t, independentConfigurationSchemaInsertValidation(t, f.s.Pool, bad))
	})
	t.Run("applied_initial_state", func(t *testing.T) {
		bad := f.plan(t)
		bad.Status, bad.ReceiptID = "applied", "forged"
		now := time.Now()
		bad.AppliedAt = &now // Missing applied_revision must never CHECK-NULL through.
		independentConfigurationSchemaDenied(t, independentConfigurationSchemaInsertPlan(t, f.s.Pool, bad))
	})
	t.Run("installed_profile_must_match_approved_template", func(t *testing.T) {
		tx, err := f.s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		bad := *f.dest.MCP
		bad.Endpoint = "https://unapproved.example.invalid/mcp"
		independentConfigurationSchemaDenied(t, f.bindMCP(t, tx, bad))
	})
	if err := independentConfigurationSchemaInsertApproval(t, f.s.Pool, p, v.ID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	p = f.apply(t, p)
	if p.Version != 2 || p.AppliedRevision <= p.BaseRevision {
		t.Fatal("application did not advance exact durable receipt", p.Version, p.AppliedRevision)
	}
	_, err := f.s.Pool.Exec(ctx, "UPDATE configuration_plans SET status='planned',applied_at=NULL,applied_revision=NULL,receipt_id=NULL WHERE organization_id=$1 AND id=$2", p.OrganizationID, p.ID)
	independentConfigurationSchemaDenied(t, err)
	_, err = f.s.Pool.Exec(ctx, "UPDATE configuration_plans SET body=jsonb_set(body,'{reason}','\"rewritten\"') WHERE organization_id=$1 AND id=$2", p.OrganizationID, p.ID)
	independentConfigurationSchemaDenied(t, err)
	bad := f.validation(other)
	independentConfigurationSchemaDenied(t, independentConfigurationSchemaInsertValidation(t, f.s.Pool, bad))
}

func TestIndependentConfigurationSchemaOriginalMarkerAdmitsLateSafeFactWithoutResend(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	p := f.plan(t)
	if err := independentConfigurationSchemaInsertPlan(t, f.s.Pool, p); err != nil {
		t.Fatal(err)
	}
	v := f.validation(p)
	if err := independentConfigurationSchemaInsertValidation(t, f.s.Pool, v); err != nil {
		t.Fatal(err)
	}
	if err := independentConfigurationSchemaInsertApproval(t, f.s.Pool, p, v.ID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	p = f.apply(t, p)
	verification := domain.ConfigurationVerification{ID: domain.NewID("cfgverification"), OrganizationID: p.OrganizationID,
		PlanID: p.ID, OperationID: "connect", ActorID: p.ActorID, Scope: f.scope, ResourceVersion: 1, Kind: "mcp", Status: "queued", Version: 1, CostStatus: "unmetered"}
	insert := func(v domain.ConfigurationVerification, status string) error {
		_, err := f.s.Pool.Exec(ctx, `INSERT INTO configuration_verifications(organization_id,id,plan_id,operation_id,actor_id,kind,target_id,resource_version,
 actor_authority_epoch,actor_policy_epoch,actor_kind,admitted_role,body,status)
 VALUES($1,$2,$3,$4,$5,'mcp',$6,1,777,777,'human','owner',$7,$8)`,
			v.OrganizationID, v.ID, v.PlanID, v.OperationID, v.ActorID, f.scope.TargetID, independentConfigurationSchemaJSON(t, v), status)
		return err
	}
	t.Run("initial_sent_needs_real_claim", func(t *testing.T) {
		independentConfigurationSchemaDenied(t, insert(verification, "sent"))
	})
	t.Run("unrelated_plan_operation", func(t *testing.T) {
		bad := verification
		bad.OperationID = "not-in-plan"
		independentConfigurationSchemaDenied(t, insert(bad, "queued"))
	})
	if err := insert(verification, "queued"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE configuration_verifications SET worker_id='independent-schema-worker',lease_until=clock_timestamp()+interval '30 seconds',fencing_token=1
 WHERE organization_id=$1 AND id=$2`, p.OrganizationID, verification.ID); err != nil {
		t.Fatal(err)
	}
	t.Run("marker_cannot_be_added_while_still_queued", func(t *testing.T) {
		_, err := f.s.Pool.Exec(ctx, `UPDATE configuration_verifications SET dispatch_started_at=clock_timestamp(),dispatched_worker_id=worker_id,dispatched_fencing_token=fencing_token
 WHERE organization_id=$1 AND id=$2`, p.OrganizationID, verification.ID)
		independentConfigurationSchemaDenied(t, err)
	})
	t.Run("half_marker_and_wrong_worker", func(t *testing.T) {
		for _, field := range []string{"NULL", "fencing_token"} {
			_, err := f.s.Pool.Exec(ctx, `UPDATE configuration_verifications SET status='sent',dispatch_started_at=clock_timestamp(),dispatched_worker_id='different-worker',dispatched_fencing_token=`+field+`
 WHERE organization_id=$1 AND id=$2`, p.OrganizationID, verification.ID)
			independentConfigurationSchemaDenied(t, err)
		}
	})
	if _, err := f.s.Pool.Exec(ctx, `UPDATE configuration_verifications SET status='sent',dispatch_started_at=clock_timestamp(),dispatched_worker_id=worker_id,dispatched_fencing_token=fencing_token
 WHERE organization_id=$1 AND id=$2`, p.OrganizationID, verification.ID); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"status='queued'", "fencing_token=2", "status='response_received'"} {
		_, err := f.s.Pool.Exec(ctx, "UPDATE configuration_verifications SET "+change+" WHERE organization_id=$1 AND id=$2", p.OrganizationID, verification.ID)
		independentConfigurationSchemaDenied(t, err)
	}
	if _, err := f.s.Pool.Exec(ctx, "UPDATE configuration_verifications SET status='unknown',reason_code='lost_response' WHERE organization_id=$1 AND id=$2", p.OrganizationID, verification.ID); err != nil {
		t.Fatal(err)
	}
	// A second actual human owner permits deprovisioning this actor without
	// weakening the last-owner invariant. The later receipt has no live authority.
	other, _ := identity(t, f.s, 1000000)
	if _, err := f.s.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role,display_name) VALUES($1,$2,'owner','Recovery owner')", p.OrganizationID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, p.ActorID); err != nil {
		t.Fatal(err)
	}
	unsafe := map[string]any{"outcome": "response_received", "protocol_version": "2026-07-28", "tool_count": 1,
		"catalog_digest": Hash([]byte("bounded catalog")), "raw_error": "must never persist raw peer fields"}
	_, err := f.s.Pool.Exec(ctx, "UPDATE configuration_verifications SET status='response_received',outcome=$3,evidence_digest=$4 WHERE organization_id=$1 AND id=$2",
		p.OrganizationID, verification.ID, independentConfigurationSchemaJSON(t, unsafe), Hash([]byte("unsafe")))
	independentConfigurationSchemaDenied(t, err)
	delete(unsafe, "raw_error")
	if _, err = f.s.Pool.Exec(ctx, "UPDATE configuration_verifications SET status='response_received',outcome=$3,evidence_digest=$4 WHERE organization_id=$1 AND id=$2",
		p.OrganizationID, verification.ID, independentConfigurationSchemaJSON(t, unsafe), Hash([]byte("safe original observation"))); err != nil {
		t.Fatal("exact original late receipt must survive deprovisioning", err)
	}
	_, err = f.s.Pool.Exec(ctx, "UPDATE configuration_verifications SET outcome=jsonb_set(outcome,'{tool_count}','2') WHERE organization_id=$1 AND id=$2", p.OrganizationID, verification.ID)
	independentConfigurationSchemaDenied(t, err)
}

func TestIndependentConfigurationSchemaModelBindingRequiresExactEndpointAndPurpose(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	model := domain.ModelConnection{ID: domain.NewID("model"), OrganizationID: f.owner.OrganizationID, Version: 1,
		Model: "independent-schema-model", ProviderAccount: "independent-synthetic-account", PriceVersion: "synthetic-v1",
		Endpoint: "https://independent-model.example.invalid/infer", MaxChargeMicroUSD: 17, MaxInputTokens: 1024,
		FixedMicroUSD: 17, MaxOutputTokens: 8, MaxInputBytes: 4096, MaxOutputBytes: 4096,
		TimeoutMilliseconds: 1000, BoundAssumptions: "Synthetic fixed charge only; no external request", TokenLimitField: "max_tokens"}
	purpose, err := f.s.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: domain.NewID("purpose"),
		OrganizationID: f.owner.OrganizationID, Version: 1, Kind: "model", EndpointDigest: Hash([]byte(model.Endpoint)),
		CredentialEnvironment: "PLATFORM_MODEL_KEY_APPROVED_SCHEMA", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := f.s.InstallConfigurationDestination(ctx, domain.ConfigurationDestinationProfile{
		Destination: domain.ConfigurationDestination{ID: domain.NewID("destination"), OrganizationID: f.owner.OrganizationID,
			Version: 1, Kind: "model", Name: "Approved model template", TargetID: model.ID, Model: model.Model,
			CredentialPurposeID: purpose.ID, CredentialPurposeVersion: 1, MaxVerifyChargeMicroUSD: 17, Active: true}, Model: &model})
	if err != nil {
		t.Fatal(err)
	}
	bind := func(version int) error {
		_, err := f.s.Pool.Exec(ctx, `INSERT INTO configuration_resource_bindings(organization_id,kind,target_id,resource_version,destination_id,destination_version,credential_purpose_id,credential_purpose_version)
 VALUES($1,'model',$2,$3,$4,1,$5,1)`, f.owner.OrganizationID, model.ID, version, dest.Destination.ID, purpose.ID)
		return err
	}
	for index, kind := range []string{"different_endpoint", "different_credential", "exact_template_next_version"} {
		t.Run(kind, func(t *testing.T) {
			installed := model
			installed.Version = index + 1
			installed.CredentialEnvironment = purpose.CredentialEnvironment
			if index == 0 {
				installed.Endpoint = "https://not-approved.example.invalid/infer"
			} else if index == 1 {
				installed.CredentialEnvironment = "PLATFORM_MODEL_KEY_DIFFERENT_SCHEMA"
			}
			if _, err := f.s.InstallModelConnection(ctx, installed); err != nil {
				t.Fatal(err)
			}
			err := bind(installed.Version)
			if index < 2 {
				independentConfigurationSchemaDenied(t, err)
			} else if err != nil {
				t.Fatal("actual next version with approved purpose must match normalized template", err)
			}
		})
	}
}

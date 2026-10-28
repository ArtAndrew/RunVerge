// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"io/fs"
	"net/url"
	"sort"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func webhookUpgrade020(t *testing.T) (*Store, map[string][]string) {
	t.Helper()
	admin, dsn := testStore(t)
	ctx := context.Background()
	schema := domain.NewID("test_webhook_upgrade")
	if _, err := admin.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid isolated database configuration")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal("open isolated historical schema")
	}
	t.Cleanup(func() {
		s.Close()
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Pool.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "CREATE TABLE schema_migrations(name text PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		t.Fatal(err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	n := 0
	for _, name := range names {
		if name >= "migrations/021_" {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			t.Fatal(name, err)
		}
		if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
			t.Fatal(name, err)
		}
		if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL DEFERRED"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)", name, Hash(body)); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 20 {
		t.Fatal("historical fixture must stage exact001-020", n)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	columns := map[string][]string{}
	rows, err := s.Pool.Query(ctx, `SELECT table_name,column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name<>'schema_migrations' ORDER BY table_name,ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var table, col string
		if err = rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		columns[table] = append(columns[table], col)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	return s, columns
}

func webhookUpgradeSnapshot(t *testing.T, ctx context.Context, s *Store, columns map[string][]string) configurationUpgradeImage {
	t.Helper()
	image := configurationUpgradeSnapshot(t, ctx, s, columns)
	// The shared projection helper's original migration cutoff is018. This
	// upgrade independently includes every immutable020 acknowledgment as well.
	var raw string
	if err := s.Pool.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(x) ORDER BY name)::text FROM schema_migrations x WHERE name<'migrations/021_'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	image.MigrationRows = Hash([]byte(raw))
	image.Migrations = map[string]string{}
	rows, err := s.Pool.Query(ctx, "SELECT name,sha256 FROM schema_migrations WHERE name<'migrations/021_' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, hash string
		if err = rows.Scan(&name, &hash); err != nil {
			t.Fatal(err)
		}
		image.Migrations[name] = hash
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	return image
}

func TestWebhookAuthorUpgradePreservesPopulated020AndRollback(t *testing.T) {
	s, columns := webhookUpgrade020(t)
	ctx := context.Background()
	var err error
	// Current constructors populate exact020 only inside this bounded test-only
	// emulation. It is removed before the old function/row/sequence snapshots.
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		pa, _ := identity(t, s, 1_000_000)
		pb, _ := identity(t, s, 1_000_000)
		a := agent(t, s, pa)
		ta := task(t, s, pa, a)
		other := task(t, s, pb, agent(t, s, pb))
		if _, err := s.CreateMaterial(ctx, pa, ta.ID, domain.NewID("key"), domain.CreateMaterial{Name: "Existing private draft", MediaType: "text/plain", Content: "Preserve original material"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CancelTask(ctx, pb, other.ID, domain.NewID("key")); err != nil {
			t.Fatal(err)
		}
		securityAuthorAccount(t, s, pa, "member")
		_ = independentSession(t, s, pa, "webhook-upgrade-existing-browser", time.Hour)
		endpoint := "https://webhook-upgrade-mcp.invalid/mcp"
		purpose, err := s.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: "old_purpose", OrganizationID: pa.OrganizationID, Version: 1, Kind: "mcp", EndpointDigest: Hash([]byte(endpoint)), CredentialEnvironment: "PLATFORM_MCP_KEY_UPGRADE", Active: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.InstallConfigurationDestination(ctx, domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: "old_destination", OrganizationID: pa.OrganizationID, Version: 1, Kind: "mcp", Name: "Preserved MCP profile", TargetID: "old_mcp", CredentialPurposeID: purpose.ID, CredentialPurposeVersion: 1, DiscoveryUnmeteredApproved: true, Active: true}, MCP: &domain.MCPConnectionProfile{Endpoint: endpoint, TimeoutSeconds: 2}}); err != nil {
			t.Fatal(err)
		}
	})
	before := webhookUpgradeSnapshot(t, ctx, s, columns)
	if len(before.Tables) != 63 || len(before.Migrations) != 20 {
		t.Fatal("baseline projection missing old tables/migrations", len(before.Tables), len(before.Migrations))
	}
	// A controlled mid-migration naming collision must roll back ALL earlier
	// new tables/indexes and keep the original populated image unchanged.
	if _, err = s.Pool.Exec(ctx, "CREATE TABLE webhook_audit(collision boolean)"); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err == nil {
		t.Fatal("forced mid-migration failure succeeded")
	}
	var partial bool
	if err = s.Pool.QueryRow(ctx, "SELECT to_regclass('webhook_destinations') IS NOT NULL").Scan(&partial); err != nil || partial {
		t.Fatal("failed migration left a partial schema", err)
	}
	configurationUpgradeAssertPreserved(t, before, webhookUpgradeSnapshot(t, ctx, s, columns))
	if _, err = s.Pool.Exec(ctx, "DROP TABLE webhook_audit"); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	after := webhookUpgradeSnapshot(t, ctx, s, columns)
	configurationUpgradeAssertPreserved(t, before, after)
	expected := EmbeddedSchemaIdentity()
	acknowledgments, err := s.Pool.Query(ctx, "SELECT name,sha256 FROM schema_migrations ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for acknowledgments.Next() {
		var name, digest string
		if err := acknowledgments.Scan(&name, &digest); err != nil {
			t.Fatal(err)
		}
		if n >= len(expected) || expected[n].Name != name || expected[n].SHA256 != digest {
			t.Fatal("upgrade acknowledgment does not match exact embedded history", name)
		}
		n++
	}
	acknowledgments.Close()
	if err := acknowledgments.Err(); err != nil || n != len(expected) {
		t.Fatal("upgrade acknowledgment missing", err, n)
	}
	var deliveryCount int
	if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries").Scan(&deliveryCount); err != nil || deliveryCount != 0 {
		t.Fatal("migration exported historical events", err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("idempotent reapplication", err)
	}
	configurationUpgradeAssertPreserved(t, before, webhookUpgradeSnapshot(t, ctx, s, columns))
	t.Logf("populated020->current (%d exact migration acknowledgments) preserved %d table projections, %d historical migrations, %d original triggers, %d original functions, %d sequences; forced mid-migration rollback and idempotent reapply passed", n, len(before.Tables), len(before.Migrations), len(before.Triggers), len(before.Functions), len(before.Sequences))
}

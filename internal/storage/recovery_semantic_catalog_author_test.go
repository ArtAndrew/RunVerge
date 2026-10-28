// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

var recoverySemanticAuthorOutput = flag.String("recovery-semantic-catalog-output", "", "test-only new trusted semantic catalogue metadata path")

func semanticAuthorRead(t *testing.T, s *Store, validate bool) RecoverySemanticCatalog {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal("catalogue transaction failed", err)
	}
	defer tx.Rollback(ctx)
	var before, after string
	if tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('search_path')").Scan(&before) != nil {
		t.Fatal("search path read failed")
	}
	var got RecoverySemanticCatalog
	if validate {
		got, err = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema)
	} else {
		got, err = readRecoverySemanticCatalog(ctx, tx, s.runtimeSchema)
	}
	if err != nil {
		t.Fatal("trusted semantic catalogue failed", err)
	}
	if tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('search_path')").Scan(&after) != nil || before != after {
		t.Fatal("catalogue changed transaction search path")
	}
	return got
}
func TestRecoverySemanticCatalogTrusted(t *testing.T) {
	s, _ := recoveryAuthorStore(t)
	catalog := semanticAuthorRead(t, s, false)
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if *recoverySemanticAuthorOutput != "" {
		f, err := os.OpenFile(*recoverySemanticAuthorOutput, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			t.Fatal("catalogue output must be new", err)
		}
		_, writeErr := f.Write(append(raw, '\n'))
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal("catalogue write failed")
		}
		t.Log("trusted semantic digest", Hash(raw), "relations", len(catalog.Relations), "constraints", len(catalog.Constraints), "indexes", len(catalog.Indexes), "columns", len(catalog.Columns), "triggers", len(catalog.Triggers), "functions", len(catalog.Functions))
		if recoverySemanticCatalogSHA256 == "UNSEALED" {
			t.Skip("reference capture only; runtime pin not yet sealed")
		}
	}
	if Hash(raw) != recoverySemanticCatalogSHA256 {
		t.Fatal("trusted catalogue differs from compiled semantic pin")
	}
	_ = semanticAuthorRead(t, s, true)
	second, _ := recoveryAuthorStore(t)
	other := semanticAuthorRead(t, second, true)
	otherRaw, _ := json.Marshal(other)
	if string(raw) != string(otherRaw) {
		t.Fatal("catalogue depends on fresh schema name or object OIDs")
	}
	generated := 0
	foreign := 0
	for _, c := range catalog.Columns {
		if c.Generated != "" {
			generated++
			if c.RawExpression == "" {
				t.Fatal("missing trusted actual-schema expression")
			}
		}
	}
	for _, c := range catalog.Constraints {
		if c.Type == "f" {
			foreign++
			if c.RefSchema != recoverySemanticNamespace || len(c.Keys) == 0 || len(c.Keys) != len(c.RefKeys) || len(c.RITriggers) != 4 {
				t.Fatal("foreign key descriptor incomplete")
			}
		}
	}
	if generated != 2 || foreign == 0 {
		t.Fatal("generated/foreign catalogue missing")
	}
}
func TestRecoverySemanticCatalogNormalization(t *testing.T) {
	cases := map[string]string{
		`CREATE INDEX a ON fresh_123.tab (x)`:                                `CREATE INDEX a ON "__runverge_recovery_target__".tab (x)`,
		`CHECK ("fresh_123" . f('fresh_123.keep'))`:                          `CHECK ("__runverge_recovery_target__" . f('fresh_123.keep'))`,
		`SELECT $tag$fresh_123.keep$tag$, 'fresh_123.keep', fresh_123.f()`:   `SELECT $tag$fresh_123.keep$tag$, 'fresh_123.keep', "__runverge_recovery_target__".f()`,
		"-- fresh_123.keep\n/* fresh_123.keep /* nested */ */ fresh_123.f()": "-- fresh_123.keep\n/* fresh_123.keep /* nested */ */ \"__runverge_recovery_target__\".f()",
	}
	for input, want := range cases {
		got, err := recoverySemanticNormalizeSQL(input, "fresh_123")
		if err != nil || got != want {
			t.Fatal("SQL normalization changed a literal/body or failed qualifier")
		}
	}
	for _, bad := range []string{"'unterminated", `"unterminated`, "$q$unterminated", "/* unterminated", strings.Repeat("x", recoverySemanticMaxRecord+1)} {
		if _, err := recoverySemanticNormalizeSQL(bad, "fresh_123"); !errors.Is(err, ErrRecoverySemanticCatalog) {
			t.Fatal("malformed or unbounded SQL definition accepted")
		}
	}
	raw := "CREATE OR REPLACE FUNCTION fresh_123.f()\n RETURNS void\n LANGUAGE sql\n SET search_path TO 'fresh_123', 'pg_catalog', 'pg_temp'\nAS $function$SELECT 'fresh_123.keep'$function$\n"
	normalized, err := recoverySemanticNormalizeFunction(raw, []string{"search_path=fresh_123, pg_catalog, pg_temp"}, "fresh_123")
	if err != nil || !strings.Contains(normalized, "'fresh_123.keep'") || !strings.Contains(normalized, "SET search_path="+recoverySemanticNamespace) {
		t.Fatal("function normalization lost exact body or pinned config")
	}
	if _, err = recoverySemanticNormalizeConfig("search_path=$user, public", "fresh_123"); err == nil {
		t.Fatal("ambiguous function search path accepted")
	}
}

func TestRecoverySemanticCatalogRejectsChanges(t *testing.T) {
	for _, scenario := range []string{"check", "default", "user_trigger", "function_body", "function_config", "foreign_deferrability", "foreign_trigger", "unvalidated_check", "extra_index", "collation_index", "operator_existing_function", "custom_domain", "rewrite_rule"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := recoveryAuthorStore(t)
			ctx := context.Background()
			_ = semanticAuthorRead(t, s, true)
			var statement string
			switch scenario {
			case "check":
				statement = "ALTER TABLE organizations DROP CONSTRAINT organizations_budget_microusd_check; ALTER TABLE organizations ADD CONSTRAINT organizations_budget_microusd_check CHECK(budget_microusd>=-1)"
			case "default":
				statement = "ALTER TABLE tasks ALTER COLUMN waiting_reason SET DEFAULT 'changed'"
			case "user_trigger":
				statement = "ALTER TABLE organizations DISABLE TRIGGER recovery_runtime_write"
			case "function_body":
				statement = "CREATE OR REPLACE FUNCTION protect_ledger_history() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$"
			case "function_config":
				statement = "ALTER FUNCTION runtime_recovery_normal() SET search_path TO pg_catalog"
			case "foreign_deferrability":
				statement = "ALTER TABLE reservations ALTER CONSTRAINT pending_quarantine_binding DEFERRABLE INITIALLY DEFERRED"
			case "foreign_trigger":
				var trigger string
				err := s.Pool.QueryRow(ctx, `SELECT t.tgname FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class r ON r.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=r.relnamespace JOIN pg_catalog.pg_constraint c ON c.oid=t.tgconstraint WHERE n.nspname=$1 AND r.relname='reservations' AND c.conname='pending_quarantine_binding' AND t.tgisinternal ORDER BY t.tgname LIMIT 1`, s.runtimeSchema).Scan(&trigger)
				if err != nil {
					t.Fatal("RI fixture lookup failed", err)
				}
				statement = "ALTER TABLE " + pgx.Identifier{s.runtimeSchema, "reservations"}.Sanitize() + " DISABLE TRIGGER " + pgx.Identifier{trigger}.Sanitize()
			case "unvalidated_check":
				statement = "ALTER TABLE organizations DROP CONSTRAINT organizations_budget_microusd_check; ALTER TABLE organizations ADD CONSTRAINT organizations_budget_microusd_check CHECK(budget_microusd>=0) NOT VALID"
			case "extra_index":
				statement = "CREATE INDEX extra_recovery_index ON organizations(name)"
			case "operator_existing_function":
				statement = "CREATE OPERATOR || (LEFTARG=text, RIGHTARG=text, FUNCTION=pg_catalog.textcat)"
			case "custom_domain":
				statement = "CREATE DOMAIN extra_recovery_domain AS text"
			case "rewrite_rule":
				statement = "CREATE RULE extra_recovery_rewrite AS ON DELETE TO organizations DO INSTEAD NOTHING"
			case "collation_index":
				statement = "DROP INDEX tool_occurrence_identity; CREATE UNIQUE INDEX tool_occurrence_identity ON tool_executions(organization_id,task_id,((intent->>'occurrence_id') COLLATE \"C\"))"
			}
			if _, err := s.Pool.Exec(ctx, statement); err != nil {
				t.Fatal("owned schema change fixture failed", err)
			}
			tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); !errors.Is(err, ErrRecoverySemanticCatalog) {
				t.Fatal("semantic schema change accepted", scenario, err)
			}
		})
	}
}
func TestRecoverySemanticCatalogDoesNotInvokeUnverifiedBuiltins(t *testing.T) {
	s, _ := recoveryAuthorStore(t)
	ctx := context.Background()
	// Canary only raises an exception if reached; no OS, network or data effects.
	// Target-first search_path would resolve these before pg_catalog builtins.
	statements := []string{
		`CREATE FUNCTION pg_get_functiondef(oid) RETURNS text LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'untrusted catalogue canary invoked'; END$$`,
		`CREATE FUNCTION unnest(smallint[]) RETURNS SETOF smallint LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'untrusted exact overload canary invoked'; END$$`,
		`CREATE FUNCTION recovery_shadow_concat(text,text) RETURNS text LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'untrusted operator canary invoked'; END$$`,
		`CREATE OPERATOR || (LEFTARG=text, RIGHTARG=text, FUNCTION=recovery_shadow_concat)`,
		`CREATE FUNCTION recovery_shadow_equals(name,name) RETURNS boolean LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'untrusted equality canary invoked'; END$$`,
		`CREATE OPERATOR = (LEFTARG=name, RIGHTARG=name, FUNCTION=recovery_shadow_equals)`,
	}
	for _, sql := range statements {
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			t.Fatal("shadow fixture failed", err)
		}
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); !errors.Is(err, ErrRecoverySemanticCatalog) {
		t.Fatal("shadow schema accepted")
	}
	// A canary RAISE would abort this transaction. A structural rejection must
	// leave it usable, proving no unverified builtin/operator was invoked.
	var one int
	if err = tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatal("unverified canary was executed")
	}
}

func TestRecoverySemanticCatalogBoundsAndCancellation(t *testing.T) {
	t.Run("oversize_function_metadata", func(t *testing.T) {
		s, _ := recoveryAuthorStore(t)
		ctx := context.Background()
		source := "CREATE OR REPLACE FUNCTION protect_ledger_history() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN /*" + strings.Repeat("x", recoverySemanticMaxRecord) + "*/ RETURN NEW; END$$"
		if _, err := s.Pool.Exec(ctx, source); err != nil {
			t.Fatal("owned large definition fixture failed", err)
		}
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = readRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); !errors.Is(err, ErrRecoverySemanticCatalog) {
			t.Fatal("oversize catalogue record accepted")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		s, _ := recoveryAuthorStore(t)
		ctx := context.Background()
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err = readRecoverySemanticCatalog(cancelled, tx, s.runtimeSchema); !errors.Is(err, context.Canceled) {
			t.Fatal("catalogue cancellation ignored")
		}
	})
}

func TestRecoverySemanticCatalogBeforeMigrationRows(t *testing.T) {
	for _, scenario := range []string{"view", "rls", "custom_column_type"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := recoveryAuthorStore(t)
			ctx := context.Background()
			statements := []string{}
			switch scenario {
			case "view":
				statements = []string{`CREATE FUNCTION recovery_migration_canary() RETURNS text LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'untrusted migration data canary invoked'; END$$`, `ALTER TABLE schema_migrations RENAME TO original_migration_metadata`, `CREATE VIEW schema_migrations AS SELECT recovery_migration_canary() AS name,repeat('0',64) AS sha256`}
			case "rls":
				statements = []string{`ALTER TABLE schema_migrations ENABLE ROW LEVEL SECURITY`, `ALTER TABLE schema_migrations FORCE ROW LEVEL SECURITY`}
			case "custom_column_type":
				statements = []string{`CREATE DOMAIN recovery_migration_type AS text`, `ALTER TABLE schema_migrations ALTER COLUMN name TYPE recovery_migration_type`}
			}
			for _, sql := range statements {
				if _, err := s.Pool.Exec(ctx, sql); err != nil {
					t.Fatal("migration metadata canary fixture failed", err)
				}
			}
			tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); !errors.Is(err, ErrRecoverySemanticCatalog) {
				t.Fatal("untrusted migration relation accepted")
			}
			var one int
			if tx.QueryRow(ctx, "SELECT 1").Scan(&one) != nil || one != 1 {
				t.Fatal("target migration relation was evaluated before rejection")
			}
		})
	}
}
func TestRecoverySemanticCatalogExternalTypeProfile(t *testing.T) {
	for _, kind := range []string{"domain_default", "enum", "range", "manual_composite", "inbound_fk"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := recoveryAuthorStore(t)
			ctx := context.Background()
			external := s.runtimeSchema + "_outside"
			ext := pgx.Identifier{external}.Sanitize()
			typ := pgx.Identifier{external, "custom_type"}.Sanitize()
			if _, err := s.Pool.Exec(ctx, "CREATE SCHEMA "+ext); err != nil {
				t.Fatal("external owned schema creation failed", err)
			}
			t.Cleanup(func() {
				if _, err := s.Pool.Exec(context.Background(), "DROP SCHEMA "+ext+" CASCADE"); err != nil {
					t.Error("external owned schema cleanup failed")
				}
			})
			var sql string
			switch kind {
			case "domain_default":
				sql = "CREATE DOMAIN " + typ + " AS text"
			case "enum":
				sql = "CREATE TYPE " + typ + " AS ENUM ('known')"
			case "range":
				sql = "CREATE TYPE " + typ + " AS RANGE (subtype=pg_catalog.int4)"
			case "manual_composite":
				sql = "CREATE TYPE " + typ + " AS (value pg_catalog.text)"
			case "inbound_fk":
				sql = "CREATE TABLE " + typ + "(id text REFERENCES " + pgx.Identifier{s.runtimeSchema, "principals"}.Sanitize() + "(id))"
			}
			if _, err := s.Pool.Exec(ctx, sql); err != nil {
				t.Fatal("external owned type creation failed", err)
			}
			if kind == "domain_default" {
				if _, err := s.Pool.Exec(ctx, "ALTER TABLE tasks ALTER COLUMN waiting_reason SET DEFAULT 'known'::"+typ); err != nil {
					t.Fatal("external type default fixture failed", err)
				}
			}
			tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = readRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); !errors.Is(err, ErrRecoverySemanticCatalog) {
				t.Fatal("external custom type reached supported profile")
			}
			var one int
			if tx.QueryRow(ctx, "SELECT 1").Scan(&one) != nil || one != 1 {
				t.Fatal("external type rejection aborted transaction")
			}
		})
	}
}

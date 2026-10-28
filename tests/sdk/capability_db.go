// SPDX-License-Identifier: Apache-2.0
// Acceptance-only schema setup/cleanup and read-only state fingerprints.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := run(ctx); err != nil {
		// Preserve the error class, withholding database configuration and row data.
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error_class": fmt.Sprintf("%T", err), "operation": "isolated_schema_helper"})
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	if len(os.Args) != 3 || !regexp.MustCompile(`^cap_sdk_[0-9a-f]{24}$`).MatchString(os.Args[2]) {
		return fmt.Errorf("explicit isolated schema and operation required")
	}
	conn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	schema := os.Args[2]
	name := pgx.Identifier{schema}.Sanitize()
	switch os.Args[1] {
	case "create":
		_, err = conn.Exec(ctx, "CREATE SCHEMA "+name)
	case "drop":
		_, err = conn.Exec(ctx, "DROP SCHEMA "+name+" CASCADE")
	case "exists":
		var exists bool
		err = conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)", schema).Scan(&exists)
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(map[string]bool{"exists": exists})
		}
	case "snapshot":
		rows, e := conn.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname=$1 ORDER BY tablename", schema)
		if e != nil {
			return e
		}
		tables := []string{}
		for rows.Next() {
			var table string
			if e = rows.Scan(&table); e != nil {
				rows.Close()
				return e
			}
			tables = append(tables, table)
		}
		rows.Close()
		if rows.Err() != nil {
			return rows.Err()
		}
		out := map[string]any{}
		for _, table := range tables {
			var count int64
			var hash string
			// Hash inside PostgreSQL; credential and user row values never leave it.
			query := "SELECT count(*), encode(sha256(convert_to(COALESCE(string_agg(to_jsonb(t)::text, E'\\n' ORDER BY to_jsonb(t)::text),''),'UTF8')),'hex') FROM " + pgx.Identifier{schema, table}.Sanitize() + " AS t"
			if e = conn.QueryRow(ctx, query).Scan(&count, &hash); e != nil {
				return e
			}
			out[table] = map[string]any{"count": count, "rows_sha256": hash}
		}
		err = json.NewEncoder(os.Stdout).Encode(out)
	default:
		return fmt.Errorf("unsupported schema operation")
	}
	return err
}

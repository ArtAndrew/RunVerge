// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/netip"
	"os"
	"strings"
	"time"
)

// OpenRecoveryEvidenceSource accepts only the direct numeric native-Docker
// endpoint constructed by the source observer. No migration or write is run.
func OpenRecoveryEvidenceSource(ctx context.Context, dsn string, expected RecoverySourceIdentity) (*Store, error) {
	for _, entry := range os.Environ() {
		k, v, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(k, "PG") && v != "" {
			return nil, ErrRecoverySnapshotSource
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, ErrRecoverySnapshotSource
	}
	ip, err := netip.ParseAddr(cfg.ConnConfig.Host)
	if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsLoopback() || cfg.ConnConfig.Port != 5432 || cfg.ConnConfig.Database != "runverge" || cfg.ConnConfig.User != "runverge_admin" || cfg.ConnConfig.Password == "" || cfg.ConnConfig.TLSConfig != nil || len(cfg.ConnConfig.Fallbacks) != 0 {
		return nil, ErrRecoverySnapshotSource
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	cfg.ConnConfig.RuntimeParams = map[string]string{"search_path": "public", "default_transaction_read_only": "on", "statement_timeout": "60000", "lock_timeout": "5000"}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, ErrRecoverySnapshotSource
	}
	var system, source string
	var oid uint32
	var allowed bool
	err = p.QueryRow(ctx, `SELECT s.system_identifier::text,d.oid,r.installation_id,NOT pg_catalog.pg_is_in_recovery() AND u.rolsuper AND current_setting('server_version_num')::int BETWEEN 180000 AND 189999 FROM pg_catalog.pg_control_system() s,pg_catalog.pg_database d,public.runtime_recovery_control r,pg_catalog.pg_roles u WHERE d.datname=current_database() AND r.singleton AND r.state='normal' AND u.rolname=current_user`).Scan(&system, &oid, &source, &allowed)
	if err != nil || !allowed || system != expected.SystemID || oid != expected.DatabaseOID || source != expected.SourceID {
		p.Close()
		return nil, ErrRecoverySnapshotSource
	}
	return &Store{Pool: p, runtimeSchema: "public"}, nil
}

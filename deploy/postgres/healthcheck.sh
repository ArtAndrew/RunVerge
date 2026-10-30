#!/bin/sh
# SPDX-License-Identifier: Elastic-2.0
# Authenticated readiness/profile check, not a runtime authorization barrier.
set -eu
case "${RUNVERGE_CREDENTIAL_PROFILE:-}" in
  ''|*[!0123456789abcdef]*) exit 1 ;;
esac
[ "${#RUNVERGE_CREDENTIAL_PROFILE}" -eq 32 ] || exit 1
marker="$PGDATA/runverge-credential-profile"
[ -f "$marker" ] && [ ! -L "$marker" ] || exit 1
[ "$(head -c 128 "$marker")" = "runverge-app-owner-v1:$RUNVERGE_CREDENTIAL_PROFILE" ] || exit 1
export PGPASSWORD="$POSTGRES_PASSWORD" PGCONNECT_TIMEOUT=2
# TCP excludes the entrypoint's temporary Unix-socket-only initialization server.
observed=$(psql -h 127.0.0.1 -U runverge_admin -d runverge -X -At --no-password -v ON_ERROR_STOP=1 2>/dev/null <<'SQL'
\set runtime_verifier ''
\getenv runtime_verifier RUNVERGE_APP_VERIFIER
SELECT 'ok' FROM pg_catalog.pg_authid r, pg_catalog.pg_database d, pg_catalog.pg_roles a
WHERE r.rolname='runverge' AND r.rolcanlogin AND NOT r.rolsuper
  AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolinherit
  AND NOT r.rolreplication AND NOT r.rolbypassrls
  AND r.rolpassword=:'runtime_verifier'
  AND d.datname='runverge' AND d.datdba=r.oid
  AND a.rolname=CURRENT_USER AND a.rolsuper AND a.oid<>r.oid
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members m WHERE m.member=r.oid);
SQL
)
[ "$observed" = ok ]

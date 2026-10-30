-- SPDX-License-Identifier: Elastic-2.0
-- Fresh cluster initialization only. Never demote/adopt an existing login.
\set ON_ERROR_STOP on
\set ECHO none
\set runtime_verifier ''
\getenv runtime_verifier RUNVERGE_APP_VERIFIER
SELECT :'runtime_verifier' ~ '^SCRAM-SHA-256[$]4096:[A-Za-z0-9+/]{22}==[$][A-Za-z0-9+/]{43}=:[A-Za-z0-9+/]{43}=$' AS valid_verifier \gset
\if :valid_verifier
BEGIN;
SELECT pg_catalog.format(
  'CREATE ROLE runverge LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',
  :'runtime_verifier') \gexec
ALTER DATABASE runverge OWNER TO runverge;
COMMIT;
\else
DO $$ BEGIN RAISE EXCEPTION 'A generated private runtime verifier is required'; END $$;
\endif

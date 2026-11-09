-- SPDX-License-Identifier: Elastic-2.0
CREATE TABLE recovery_finance_statements (
 batch_id text PRIMARY KEY REFERENCES recovery_accounting_batches(id),
 usage_id text NOT NULL REFERENCES recovery_usage_observations(id),
 statement_sha256 text NOT NULL CHECK(statement_sha256=encode(sha256(envelope),'hex')),
 envelope bytea NOT NULL CHECK(octet_length(envelope) BETWEEN 1 AND 16384),
 public_key bytea NOT NULL CHECK(octet_length(public_key)=32)
);
REVOKE ALL ON recovery_finance_statements FROM PUBLIC;

CREATE FUNCTION recovery_finance_statement_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'INSERT' THEN
  RAISE EXCEPTION 'finance statements are permanent' USING ERRCODE='55000';
 END IF;
 IF SESSION_USER<>CURRENT_USER OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=SESSION_USER AND rolsuper)
 OR NOT EXISTS(
  SELECT 1 FROM recovery_accounting_batches b JOIN recovery_usage_observations u
   ON u.id=NEW.usage_id AND u.evidence_batch_id=b.evidence_batch_id AND u.restore_id=b.restore_id
   AND u.target_installation_id=b.target_installation_id AND u.organization_id=b.organization_id AND u.reservation_id=b.reservation_id
  JOIN recovery_evidence_batches e ON e.id=b.evidence_batch_id AND e.payload_bytes=0
  JOIN recovery_accounting_mutations m ON m.batch_id=b.id AND m.table_name='cost_evidence'
   AND m.after_row->>'payload_digest'=NEW.statement_sha256
  WHERE b.id=NEW.batch_id AND b.creating_transaction=pg_catalog.pg_current_xact_id()
   AND b.operator_role=SESSION_USER)
 THEN RAISE EXCEPTION 'finance statement is outside its accounting transaction' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_finance_statement_insert BEFORE INSERT ON recovery_finance_statements
 FOR EACH ROW EXECUTE FUNCTION recovery_finance_statement_guard();
CREATE TRIGGER recovery_finance_statement_permanent BEFORE UPDATE OR DELETE OR TRUNCATE ON recovery_finance_statements
 FOR EACH STATEMENT EXECUTE FUNCTION recovery_finance_statement_guard();
DO $$
BEGIN
 EXECUTE pg_catalog.format('ALTER FUNCTION %I.recovery_finance_statement_guard() SET search_path TO %I,pg_catalog,pg_temp',current_schema(),current_schema());
END $$;

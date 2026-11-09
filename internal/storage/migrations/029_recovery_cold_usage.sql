-- SPDX-License-Identifier: Elastic-2.0
-- Keep a signed, already imported capture as a usage evidence anchor. It
-- contains no copied snapshot payload and never asserts a financial outcome.
ALTER TABLE recovery_evidence_batches
 DROP CONSTRAINT recovery_evidence_batches_restore_id_key,
 DROP CONSTRAINT recovery_evidence_batches_payload_bytes_check,
 DROP CONSTRAINT recovery_evidence_batches_chunk_count_check,
 ADD CONSTRAINT recovery_evidence_payload_bound CHECK(payload_bytes BETWEEN 0 AND 402653184 AND chunk_count BETWEEN 0 AND 384),
 ADD CONSTRAINT recovery_evidence_anchor_shape CHECK(
  (payload_bytes>0 AND report->>'profile' IS DISTINCT FROM 'retired-handoff-usage-anchor-v1') OR
  (payload_bytes=0 AND chunk_count=0 AND changed_rows=0 AND
   report->>'profile' IS NOT DISTINCT FROM 'retired-handoff-usage-anchor-v1' AND
   selected_snapshot_sha256=evidence_snapshot_sha256 AND selected_receipt_sha256=evidence_receipt_sha256));
CREATE UNIQUE INDEX recovery_evidence_archive_per_restore ON recovery_evidence_batches(restore_id) WHERE payload_bytes>0;
CREATE UNIQUE INDEX recovery_evidence_cold_anchor_per_restore ON recovery_evidence_batches(restore_id) WHERE payload_bytes=0;

CREATE OR REPLACE FUNCTION recovery_evidence_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE held runtime_recovery_control%ROWTYPE; imported recovery_import_receipts%ROWTYPE;
 parent recovery_evidence_batches%ROWTYPE; admitted recovery_work_admissions%ROWTYPE; capture jsonb;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'recovery evidence is permanent' USING ERRCODE='55000'; END IF;
 PERFORM pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext(TG_TABLE_SCHEMA));
 SELECT * INTO STRICT held FROM runtime_recovery_control WHERE singleton;
 SELECT * INTO STRICT imported FROM recovery_import_receipts WHERE singleton;
 IF held.state<>'held' OR held.restore_id IS DISTINCT FROM imported.restore_id
  OR held.installation_id IS DISTINCT FROM imported.target_installation_id THEN
  RAISE EXCEPTION 'evidence requires exact imported held target' USING ERRCODE='55000';
 END IF;
 IF TG_TABLE_NAME='recovery_evidence_batches' THEN
  IF NEW.restore_id IS DISTINCT FROM imported.restore_id OR NEW.target_installation_id IS DISTINCT FROM imported.target_installation_id
   OR NEW.source_id IS DISTINCT FROM imported.source_id OR NEW.selected_snapshot_sha256 IS DISTINCT FROM imported.snapshot_sha256
   OR NEW.created_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id()::text THEN
   RAISE EXCEPTION 'evidence identity mismatch' USING ERRCODE='55000';
  END IF;
  IF NEW.payload_bytes=0 THEN
   SELECT * INTO STRICT admitted FROM recovery_work_admissions WHERE restore_id=held.restore_id
    AND target_installation_id=held.installation_id AND database_oid=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database());
   capture=pg_catalog.convert_from(NEW.capture_link,'UTF8')::jsonb->'link';
   IF SESSION_USER<>CURRENT_USER OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=SESSION_USER AND rolsuper)
    OR NEW.report->>'activation_id' IS DISTINCT FROM admitted.activation_id
    OR NEW.report->>'import_receipt_sha256' IS DISTINCT FROM admitted.import_receipt_sha256
    OR NEW.report->>'review_sha256' IS DISTINCT FROM admitted.review_sha256
    OR NEW.report->>'source_stop_sha256' IS DISTINCT FROM admitted.source_stop_sha256
    OR NEW.report->>'retirement_seal_sha256' IS DISTINCT FROM admitted.retirement_seal_sha256
    OR capture->'version' IS DISTINCT FROM '2'::jsonb OR capture->>'operation_id' IS DISTINCT FROM NEW.id
    OR pg_catalog.jsonb_typeof(capture->'selected_snapshot_sha256') IS DISTINCT FROM 'string'
    OR (capture->>'selected_snapshot_sha256' ~ '^[a-f0-9]{64}$') IS DISTINCT FROM true
    OR pg_catalog.jsonb_typeof(capture->'selected_receipt_sha256') IS DISTINCT FROM 'string'
    OR (capture->>'selected_receipt_sha256' ~ '^[a-f0-9]{64}$') IS DISTINCT FROM true
    OR capture->>'source_id' IS DISTINCT FROM NEW.source_id OR capture->>'evidence_backup_id' IS DISTINCT FROM imported.backup_id
    OR capture->>'evidence_snapshot_sha256' IS DISTINCT FROM imported.snapshot_sha256
    OR capture->>'evidence_receipt_sha256' IS DISTINCT FROM NEW.evidence_receipt_sha256
    OR capture->>'evidence_schema_sha256' IS DISTINCT FROM NEW.evidence_schema_sha256
    OR NEW.evidence_schema_sha256 IS DISTINCT FROM imported.schema_sha256
    OR (capture->>'evidence_cut_utc')::timestamptz IS DISTINCT FROM imported.cut_at
    OR NEW.evidence_cut_at IS DISTINCT FROM imported.cut_at THEN
    RAISE EXCEPTION 'cold evidence anchor differs from admitted import' USING ERRCODE='55000';
   END IF;
  ELSIF NEW.evidence_cut_at<=imported.cut_at THEN
   RAISE EXCEPTION 'evidence identity mismatch' USING ERRCODE='55000';
  END IF;
 ELSE
  SELECT * INTO STRICT parent FROM recovery_evidence_batches WHERE id=NEW.batch_id;
  IF parent.restore_id IS DISTINCT FROM held.restore_id OR parent.created_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id()::text THEN
   RAISE EXCEPTION 'evidence batch already sealed' USING ERRCODE='55000';
  END IF;
  IF TG_TABLE_NAME='recovery_evidence_chunks' THEN
   IF NEW.ordinal>=parent.chunk_count OR octet_length(NEW.payload)<>LEAST(1048576,parent.payload_bytes-NEW.ordinal::bigint*1048576) THEN
    RAISE EXCEPTION 'evidence chunk outside declared payload' USING ERRCODE='55000';
   END IF;
  ELSIF TG_TABLE_NAME='recovery_evidence_changes' AND NEW.ordinal>=parent.changed_rows THEN
   RAISE EXCEPTION 'evidence change outside declared batch' USING ERRCODE='55000';
  END IF;
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION recovery_cold_usage_binding_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE e recovery_evidence_batches%ROWTYPE; plan jsonb; capture jsonb;
BEGIN
 SELECT * INTO STRICT e FROM recovery_evidence_batches WHERE id=NEW.evidence_batch_id;
 plan=pg_catalog.convert_from(NEW.source_plan_commitment,'UTF8')::jsonb;
 IF e.payload_bytes=0 THEN
  capture=pg_catalog.convert_from(e.capture_link,'UTF8')::jsonb->'link';
  IF plan->>'cold_activation_id' IS DISTINCT FROM e.report->>'activation_id'
   OR plan->>'original_snapshot_sha256' IS DISTINCT FROM capture->>'selected_snapshot_sha256'
   OR plan->>'original_receipt_sha256' IS DISTINCT FROM capture->>'selected_receipt_sha256' THEN
   RAISE EXCEPTION 'usage plan is not bound to the cold evidence anchor' USING ERRCODE='55000';
  END IF;
 ELSIF plan ? 'cold_activation_id' OR plan ? 'original_snapshot_sha256' OR plan ? 'original_receipt_sha256' THEN
  RAISE EXCEPTION 'cold usage plan needs its own evidence anchor' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_cold_usage_binding BEFORE INSERT ON recovery_usage_observations
 FOR EACH ROW EXECUTE FUNCTION recovery_cold_usage_binding_guard();

DO $$
BEGIN
 EXECUTE pg_catalog.format('ALTER FUNCTION %I.recovery_evidence_guard() SET search_path TO %I,pg_catalog,pg_temp',current_schema(),current_schema());
 EXECUTE pg_catalog.format('ALTER FUNCTION %I.recovery_cold_usage_binding_guard() SET search_path TO %I,pg_catalog,pg_temp',current_schema(),current_schema());
END $$;

-- SPDX-License-Identifier: Elastic-2.0
-- Target-local evidence. No source business row, authorization or hold is changed.
CREATE TABLE recovery_evidence_batches (
 id text PRIMARY KEY CHECK(id ~ '^[a-f0-9]{32}$'),
 restore_id text NOT NULL UNIQUE REFERENCES recovery_import_receipts(restore_id),
 target_installation_id text NOT NULL CHECK(target_installation_id ~ '^inst_[a-f0-9]{32}$'),
 source_id text NOT NULL CHECK(source_id ~ '^inst_[a-f0-9]{32}$'),
 selected_snapshot_sha256 text NOT NULL CHECK(selected_snapshot_sha256 ~ '^[a-f0-9]{64}$'),
 selected_receipt_sha256 text NOT NULL CHECK(selected_receipt_sha256 ~ '^[a-f0-9]{64}$'),
 evidence_receipt_sha256 text NOT NULL CHECK(evidence_receipt_sha256 ~ '^[a-f0-9]{64}$'),
 evidence_snapshot_sha256 text NOT NULL CHECK(evidence_snapshot_sha256 ~ '^[a-f0-9]{64}$'),
 evidence_schema_sha256 text NOT NULL CHECK(evidence_schema_sha256 ~ '^[a-f0-9]{64}$'),
 evidence_cut_at timestamptz NOT NULL,
 capture_link bytea NOT NULL CHECK(octet_length(capture_link) BETWEEN 1 AND 65536),
 capture_link_sha256 text NOT NULL CHECK(capture_link_sha256=encode(sha256(capture_link),'hex')),
 target_catalog_sha256 text NOT NULL CHECK(target_catalog_sha256 ~ '^[a-f0-9]{64}$'),
 payload_bytes bigint NOT NULL CHECK(payload_bytes BETWEEN 1 AND 402653184),
 chunk_count integer NOT NULL CHECK(chunk_count BETWEEN 1 AND 384),
 CHECK(chunk_count=(payload_bytes+1048575)/1048576),
 changed_rows bigint NOT NULL CHECK(changed_rows BETWEEN 0 AND 2000000),
 report jsonb NOT NULL CHECK(jsonb_typeof(report)='object'),
 reconciliation_state text NOT NULL DEFAULT 'pending' CHECK(reconciliation_state='pending'),
 created_transaction text NOT NULL DEFAULT pg_current_xact_id()::text,
 ingested_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE recovery_evidence_chunks (
 batch_id text NOT NULL REFERENCES recovery_evidence_batches(id),
 ordinal integer NOT NULL CHECK(ordinal BETWEEN 0 AND 383),
 payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 1048576),
 payload_sha256 text NOT NULL CHECK(payload_sha256=encode(sha256(payload),'hex')),
 PRIMARY KEY(batch_id,ordinal)
);
CREATE TABLE recovery_evidence_changes (
 batch_id text NOT NULL REFERENCES recovery_evidence_batches(id),
 ordinal bigint NOT NULL CHECK(ordinal BETWEEN 0 AND 1999999),
 table_name text NOT NULL CHECK(table_name ~ '^[a-z][a-z0-9_]{0,127}$'),
 key_parts jsonb NOT NULL CHECK(jsonb_typeof(key_parts)='array' AND octet_length(key_parts::text)<=1048576),
 key_sha256 text NOT NULL CHECK(key_sha256=encode(sha256(convert_to(key_parts::text,'UTF8')),'hex')),
 organization_id text CHECK(octet_length(organization_id) BETWEEN 1 AND 128),
 classification text NOT NULL CHECK(classification IN('added','changed','missing_from_later_snapshot')),
 before_sha256 text CHECK(before_sha256 ~ '^[a-f0-9]{64}$'),
 after_sha256 text CHECK(after_sha256 ~ '^[a-f0-9]{64}$'),
 CHECK((classification='added' AND before_sha256 IS NULL AND after_sha256 IS NOT NULL)
    OR (classification='changed' AND before_sha256 IS NOT NULL AND after_sha256 IS NOT NULL AND before_sha256<>after_sha256)
    OR (classification='missing_from_later_snapshot' AND before_sha256 IS NOT NULL AND after_sha256 IS NULL)),
 PRIMARY KEY(batch_id,table_name,key_sha256),
 UNIQUE(batch_id,ordinal)
);
REVOKE ALL ON recovery_evidence_batches,recovery_evidence_chunks,recovery_evidence_changes FROM PUBLIC;

CREATE FUNCTION recovery_evidence_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE held runtime_recovery_control%ROWTYPE; imported recovery_import_receipts%ROWTYPE;
 parent recovery_evidence_batches%ROWTYPE;
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
   OR NEW.evidence_cut_at<=imported.cut_at OR NEW.created_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id()::text THEN
   RAISE EXCEPTION 'evidence identity mismatch' USING ERRCODE='55000';
  END IF;
 ELSE
  SELECT * INTO STRICT parent FROM recovery_evidence_batches WHERE id=NEW.batch_id;
  IF parent.restore_id IS DISTINCT FROM held.restore_id OR parent.created_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id()::text THEN
   RAISE EXCEPTION 'evidence batch already sealed' USING ERRCODE='55000';
  END IF;
  -- Finite unique slots prevent appending after an early SET CONSTRAINTS
  -- completeness check in the same transaction, without per-row COUNT scans.
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
CREATE FUNCTION recovery_evidence_complete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE n bigint; size bigint; first_chunk integer; last_chunk integer; changes bigint;
BEGIN
 SELECT count(*),COALESCE(sum(octet_length(payload)),0),min(ordinal),max(ordinal)
  INTO n,size,first_chunk,last_chunk FROM recovery_evidence_chunks WHERE batch_id=NEW.id;
 SELECT count(*) INTO changes FROM recovery_evidence_changes WHERE batch_id=NEW.id;
 IF n<>NEW.chunk_count OR size<>NEW.payload_bytes OR first_chunk<>0 OR last_chunk<>NEW.chunk_count-1 OR changes<>NEW.changed_rows THEN
  RAISE EXCEPTION 'incomplete evidence batch' USING ERRCODE='55000';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER recovery_evidence_complete AFTER INSERT ON recovery_evidence_batches
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION recovery_evidence_complete();

DO $$
DECLARE target_schema text:=current_schema(); relation_name text; function_name text;
BEGIN
 FOREACH function_name IN ARRAY ARRAY['recovery_evidence_guard','recovery_evidence_complete'] LOOP
  EXECUTE pg_catalog.format('ALTER FUNCTION %I.%I() SET search_path TO %I,pg_catalog,pg_temp',target_schema,function_name,target_schema);
 END LOOP;
 FOREACH relation_name IN ARRAY ARRAY['recovery_evidence_batches','recovery_evidence_chunks','recovery_evidence_changes'] LOOP
  EXECUTE pg_catalog.format('CREATE TRIGGER recovery_evidence_insert BEFORE INSERT ON %I.%I FOR EACH ROW EXECUTE FUNCTION %I.recovery_evidence_guard()',target_schema,relation_name,target_schema);
  EXECUTE pg_catalog.format('CREATE TRIGGER recovery_evidence_permanent BEFORE UPDATE OR DELETE OR TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION %I.recovery_evidence_guard()',target_schema,relation_name,target_schema);
 END LOOP;
END $$;

-- SPDX-License-Identifier: Elastic-2.0
-- UNAPPLIED CANDIDATE. Move into embedded migrations only with the versioned
-- snapshot importer/exporter and reviewed SQL026 compatibility profiles.
-- Historical payload is never installed into current authority tables.
ALTER TABLE recovery_import_receipts ADD COLUMN creating_transaction xid8;
ALTER TABLE recovery_import_receipts ALTER COLUMN creating_transaction SET DEFAULT pg_current_xact_id();

CREATE TABLE recovery_lineage_imports (
 restore_id text PRIMARY KEY REFERENCES recovery_import_receipts(restore_id),
 target_installation_id text NOT NULL CHECK(target_installation_id ~ '^inst_[a-f0-9]{32}$'),
 snapshot_sha256 text NOT NULL CHECK(snapshot_sha256 ~ '^[a-f0-9]{64}$'),
 wire_profile text NOT NULL CHECK(wire_profile='recovery-lineage-v1'),
 attestation text NOT NULL CHECK(attestation='current-backup-attested-history'),
 generation_count integer NOT NULL CHECK(generation_count BETWEEN 1 AND 64),
 payload_bytes bigint NOT NULL CHECK(payload_bytes BETWEEN 1 AND 402653184),
 chunk_count integer NOT NULL CHECK(chunk_count BETWEEN 1 AND 384),
 CHECK(chunk_count=(payload_bytes+1048575)/1048576),
 -- Commitment to ordered ordinal:length:chunk_hash lines. Unlike concatenating
 -- the full payload, verification allocates only bounded chunk metadata.
 chunks_sha256 text NOT NULL CHECK(chunks_sha256 ~ '^[a-f0-9]{64}$'),
 creating_transaction xid8 NOT NULL DEFAULT pg_current_xact_id(),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE recovery_lineage_chunks (
 restore_id text NOT NULL REFERENCES recovery_lineage_imports(restore_id),
 ordinal integer NOT NULL CHECK(ordinal BETWEEN 0 AND 383),
 payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 1048576),
 payload_sha256 text NOT NULL CHECK(payload_sha256=encode(sha256(payload),'hex')),
 PRIMARY KEY(restore_id,ordinal)
);
REVOKE ALL ON recovery_lineage_imports,recovery_lineage_chunks FROM PUBLIC;

CREATE FUNCTION recovery_lineage_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE held runtime_recovery_control%ROWTYPE; receipt recovery_import_receipts%ROWTYPE;
 parent recovery_lineage_imports%ROWTYPE;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'historical lineage is permanent' USING ERRCODE='55000'; END IF;
 PERFORM pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext(TG_TABLE_SCHEMA));
 SELECT * INTO STRICT held FROM runtime_recovery_control WHERE singleton;
 SELECT * INTO STRICT receipt FROM recovery_import_receipts WHERE singleton;
 IF held.state<>'held' OR held.restore_id IS DISTINCT FROM receipt.restore_id
  OR held.installation_id IS DISTINCT FROM receipt.target_installation_id
  OR NEW.restore_id IS DISTINCT FROM receipt.restore_id
  OR receipt.creating_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id() THEN
  RAISE EXCEPTION 'lineage requires the current atomic held import' USING ERRCODE='55000';
 END IF;
 IF TG_TABLE_NAME='recovery_lineage_imports' THEN
  NEW.recorded_at=pg_catalog.clock_timestamp();
  IF NEW.target_installation_id IS DISTINCT FROM held.installation_id
   OR NEW.snapshot_sha256 IS DISTINCT FROM receipt.snapshot_sha256
   OR NEW.creating_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id() THEN
   RAISE EXCEPTION 'lineage import identity mismatch' USING ERRCODE='55000';
  END IF;
 ELSE
  SELECT * INTO STRICT parent FROM recovery_lineage_imports WHERE restore_id=NEW.restore_id;
  IF parent.creating_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id()
   OR NEW.ordinal>=parent.chunk_count
   OR octet_length(NEW.payload)<>LEAST(1048576,parent.payload_bytes-NEW.ordinal::bigint*1048576) THEN
   RAISE EXCEPTION 'lineage chunk outside current declared import' USING ERRCODE='55000';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE FUNCTION recovery_lineage_complete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE n bigint; size bigint; commitment text;
BEGIN
 SELECT count(*),COALESCE(sum(octet_length(payload)),0),
  encode(sha256(convert_to(E'RunVerge lineage chunks v1\n'||COALESCE(string_agg(ordinal::text||':'||octet_length(payload)::text||':'||payload_sha256,E'\n' ORDER BY ordinal),''),'UTF8')),'hex')
 INTO n,size,commitment FROM recovery_lineage_chunks WHERE restore_id=NEW.restore_id;
 IF n<>NEW.chunk_count OR size<>NEW.payload_bytes OR commitment<>NEW.chunks_sha256 THEN
  RAISE EXCEPTION 'incomplete or changed historical lineage' USING ERRCODE='55000';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER recovery_lineage_complete AFTER INSERT ON recovery_lineage_imports
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION recovery_lineage_complete();
DO $$
DECLARE target_schema text:=current_schema(); relation_name text; function_name text;
BEGIN
 FOREACH function_name IN ARRAY ARRAY['recovery_lineage_guard','recovery_lineage_complete'] LOOP
  EXECUTE pg_catalog.format('ALTER FUNCTION %I.%I() SET search_path TO %I,pg_catalog,pg_temp',target_schema,function_name,target_schema);
 END LOOP;
 FOREACH relation_name IN ARRAY ARRAY['recovery_lineage_imports','recovery_lineage_chunks'] LOOP
  EXECUTE pg_catalog.format('CREATE TRIGGER recovery_lineage_insert BEFORE INSERT ON %I.%I FOR EACH ROW EXECUTE FUNCTION %I.recovery_lineage_guard()',target_schema,relation_name,target_schema);
  EXECUTE pg_catalog.format('CREATE TRIGGER recovery_lineage_permanent BEFORE UPDATE OR DELETE OR TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION %I.recovery_lineage_guard()',target_schema,relation_name,target_schema);
 END LOOP;
END $$;

-- SPDX-License-Identifier: Elastic-2.0
-- Existing immutable accounting receipts retain an explicit historical profile.
-- New writes cannot claim that legacy profile. SQL025 itself remains unchanged.
ALTER TABLE recovery_accounting_batches ADD COLUMN mutation_profile text NOT NULL
 DEFAULT 'sql025-exact-mutation-v1'
 CHECK(mutation_profile IN('sql025-exact-mutation-v1','sql026-exact-mutation-v1'));
ALTER TABLE recovery_accounting_batches ALTER COLUMN mutation_profile SET DEFAULT 'sql026-exact-mutation-v1';

CREATE FUNCTION recovery_accounting_profile_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.mutation_profile IS DISTINCT FROM 'sql026-exact-mutation-v1' THEN
  RAISE EXCEPTION 'new accounting batch requires current mutation profile' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_accounting_profile_insert BEFORE INSERT ON recovery_accounting_batches
 FOR EACH ROW EXECUTE FUNCTION recovery_accounting_profile_guard();

-- An observation is NOT a final invoice, settlement or execution permission.
-- The exact commitment bytes are hashed; jsonb below is used only for binding
-- checks, never to reproduce Go's serialization by reformatting its JSON.
CREATE TABLE recovery_usage_observations (
 id text PRIMARY KEY CHECK(id ~ '^[a-f0-9]{32}$'),
 restore_id text NOT NULL REFERENCES recovery_import_receipts(restore_id),
 target_installation_id text NOT NULL CHECK(target_installation_id ~ '^inst_[a-f0-9]{32}$'),
 evidence_batch_id text NOT NULL REFERENCES recovery_evidence_batches(id),
 organization_id text NOT NULL,
 reservation_id text NOT NULL,
 source_plan_sha256 text NOT NULL CHECK(source_plan_sha256 ~ '^[a-f0-9]{64}$'),
 source_plan_commitment bytea NOT NULL CHECK(octet_length(source_plan_commitment) BETWEEN 1 AND 65536),
 acquisition_sha256 text NOT NULL CHECK(acquisition_sha256 ~ '^[a-f0-9]{64}$'),
 acquisition_commitment bytea NOT NULL CHECK(octet_length(acquisition_commitment) BETWEEN 1 AND 65536),
 response_sha256 text NOT NULL CHECK(response_sha256 ~ '^[a-f0-9]{64}$'),
 response bytea NOT NULL CHECK(octet_length(response) BETWEEN 1 AND 262144),
 admission_sha256 text NOT NULL CHECK(admission_sha256 ~ '^[a-f0-9]{64}$'),
 admission_commitment bytea NOT NULL CHECK(octet_length(admission_commitment) BETWEEN 1 AND 16384),
 observation_state text NOT NULL DEFAULT 'observed_unverified' CHECK(observation_state='observed_unverified'),
 creating_transaction xid8 NOT NULL DEFAULT pg_current_xact_id(),
 operator_role name NOT NULL DEFAULT session_user,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(organization_id,reservation_id) REFERENCES reservations(organization_id,id),
 UNIQUE(restore_id,acquisition_sha256),
 CHECK(source_plan_sha256=encode(sha256(source_plan_commitment),'hex')),
 CHECK(acquisition_sha256=encode(sha256(acquisition_commitment),'hex')),
 CHECK(response_sha256=encode(sha256(response),'hex')),
 CHECK(admission_sha256=encode(sha256(admission_commitment),'hex'))
);
REVOKE ALL ON recovery_usage_observations FROM PUBLIC;

-- Database owners remain trusted. The Go coordinator separately verifies the
-- authenticated archives, opaque acquisition and current private host policy.
-- This guard protects immutable row integrity/scope and performs no accounting.
CREATE FUNCTION recovery_usage_observation_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE held runtime_recovery_control%ROWTYPE; imported recovery_import_receipts%ROWTYPE;
 evidence recovery_evidence_batches%ROWTYPE; reservation reservations%ROWTYPE;
 plan jsonb; acquisition jsonb; request jsonb; policy jsonb; observation jsonb;
 admission jsonb; provider_response jsonb; head text;
BEGIN
 IF TG_OP<>'INSERT' THEN
  RAISE EXCEPTION 'usage observations are permanent' USING ERRCODE='55000';
 END IF;
 PERFORM pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext(TG_TABLE_SCHEMA));
 SELECT * INTO STRICT held FROM runtime_recovery_control WHERE singleton;
 SELECT * INTO STRICT imported FROM recovery_import_receipts WHERE singleton;
 SELECT * INTO STRICT evidence FROM recovery_evidence_batches WHERE id=NEW.evidence_batch_id;
 SELECT * INTO STRICT reservation FROM reservations WHERE organization_id=NEW.organization_id AND id=NEW.reservation_id;
 IF held.state<>'held' OR held.restore_id IS DISTINCT FROM imported.restore_id
  OR held.installation_id IS DISTINCT FROM imported.target_installation_id
  OR NEW.restore_id IS DISTINCT FROM held.restore_id OR NEW.target_installation_id IS DISTINCT FROM held.installation_id
  OR evidence.restore_id IS DISTINCT FROM held.restore_id OR evidence.target_installation_id IS DISTINCT FROM held.installation_id
  OR evidence.selected_snapshot_sha256 IS DISTINCT FROM imported.snapshot_sha256
  OR reservation.category<>'model' OR NEW.creating_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id()
  OR NEW.operator_role IS DISTINCT FROM session_user THEN
  RAISE EXCEPTION 'usage observation requires exact held recovery scope' USING ERRCODE='55000';
 END IF;
 plan=pg_catalog.convert_from(NEW.source_plan_commitment,'UTF8')::jsonb;
 acquisition=pg_catalog.convert_from(NEW.acquisition_commitment,'UTF8')::jsonb;
 admission=pg_catalog.convert_from(NEW.admission_commitment,'UTF8')::jsonb;
 provider_response=pg_catalog.convert_from(NEW.response,'UTF8')::jsonb;
 request=acquisition->'request'; policy=acquisition->'policy'; observation=acquisition->'observation';
 -- Anchor every pair-only comparison in a required typed value. Two omitted
 -- fields are not matching identities. All duplicated values below compare as
 -- JSON values where appropriate, preserving scalar types.
 IF EXISTS(SELECT 1 FROM (VALUES
  (plan->'generation_id',128),(plan->'model',256),(plan->'model_connection_id',128),
  (plan->'archived_provider_account',128),(policy->'credential_reference',128),
  (policy->'expires_at',64),(acquisition->'operator_reference',128),
  (acquisition->'policy_confirmed_at',64),(observation->'observed_at',64),(admission->'admitted_at',64)
 ) required(value,max_bytes) WHERE pg_catalog.jsonb_typeof(value) IS DISTINCT FROM 'string'
  OR pg_catalog.octet_length(value#>>'{}') NOT BETWEEN 1 AND max_bytes)
 OR (plan->>'generation_id' ~ '^gen-[0-9A-Za-z-]+$') IS DISTINCT FROM true
 OR EXISTS(SELECT 1 FROM (VALUES(plan->'connection_identity_sha256'),(plan->'target_catalog_sha256'),
  (plan->'reservation_identity_sha256'),(plan->'action_identity_sha256'),(plan->'dispatch_receipt_sha256'),
  (plan->'model_turn_sha256'),(plan->'candidate_sha256'),(plan->'artifact_sha256'),
  (policy->'credential_sha256'),(acquisition->'policy_sha256')) required(value)
  WHERE pg_catalog.jsonb_typeof(value) IS DISTINCT FROM 'string' OR (value#>>'{}' ~ '^[a-f0-9]{64}$') IS DISTINCT FROM true)
 OR pg_catalog.jsonb_typeof(plan->'model_connection_version') IS DISTINCT FROM 'number'
 OR (plan->>'model_connection_version' ~ '^[1-9][0-9]{0,9}$') IS DISTINCT FROM true
 OR ((plan->>'model_connection_version')::numeric BETWEEN 1 AND 2147483647) IS DISTINCT FROM true
 OR pg_catalog.jsonb_typeof(acquisition->'policy_epoch') IS DISTINCT FROM 'number'
 OR (acquisition->>'policy_epoch' ~ '^[1-9][0-9]{0,19}$') IS DISTINCT FROM true
 OR ((acquisition->>'policy_epoch')::numeric BETWEEN 1 AND 18446744073709551615) IS DISTINCT FROM true
 OR pg_catalog.jsonb_typeof(acquisition->'policy_operation_id') IS DISTINCT FROM 'string'
 OR (acquisition->>'policy_operation_id' ~ '^[a-f0-9]{32}$') IS DISTINCT FROM true THEN
  RAISE EXCEPTION 'usage commitment identity fields required' USING ERRCODE='55000';
 END IF;
 SELECT id INTO head FROM recovery_accounting_batches WHERE restore_id=held.restore_id ORDER BY ordinal DESC LIMIT 1;
 IF pg_catalog.jsonb_typeof(plan) IS DISTINCT FROM 'object' OR pg_catalog.jsonb_typeof(acquisition) IS DISTINCT FROM 'object'
  OR pg_catalog.jsonb_typeof(admission) IS DISTINCT FROM 'object' OR pg_catalog.jsonb_typeof(provider_response) IS DISTINCT FROM 'object'
  OR plan->>'plan_sha256' IS DISTINCT FROM '' OR plan->'version' IS DISTINCT FROM '1'::jsonb
  OR plan->>'algorithm' IS DISTINCT FROM 'archived-model-usage-binding-v1'
  OR plan->>'state' IS DISTINCT FROM 'request_identity_available' OR plan->'execution_held' IS DISTINCT FROM 'true'::jsonb
  OR plan->'rollback_confirmed' IS DISTINCT FROM 'true'::jsonb
  OR plan->>'source_id' IS DISTINCT FROM imported.source_id OR plan->>'target_id' IS DISTINCT FROM held.installation_id
  OR plan->>'restore_id' IS DISTINCT FROM held.restore_id OR plan->>'selected_snapshot_sha256' IS DISTINCT FROM imported.snapshot_sha256
  OR plan->>'selected_receipt_sha256' IS DISTINCT FROM evidence.selected_receipt_sha256
  OR plan->>'captured_receipt_sha256' IS DISTINCT FROM evidence.evidence_receipt_sha256
  OR plan->>'captured_snapshot_sha256' IS DISTINCT FROM evidence.evidence_snapshot_sha256
  OR plan->>'capture_link_sha256' IS DISTINCT FROM evidence.capture_link_sha256
  OR plan#>>'{selection,organization_id}' IS DISTINCT FROM NEW.organization_id
  OR plan#>>'{selection,reservation_id}' IS DISTINCT FROM NEW.reservation_id
  OR plan->>'task_id' IS DISTINCT FROM reservation.task_id OR plan->>'operation_id' IS DISTINCT FROM reservation.operation_id
  OR plan->>'action_id' IS DISTINCT FROM reservation.action_id OR plan->>'head_id' IS DISTINCT FROM COALESCE(head,'') THEN
  RAISE EXCEPTION 'usage source commitment mismatch' USING ERRCODE='55000';
 END IF;
 IF acquisition->>'receipt_sha256' IS DISTINCT FROM '' OR acquisition->'version' IS DISTINCT FROM '1'::jsonb
  OR acquisition->>'account_mapping_method' IS DISTINCT FROM 'current_host_operator_attestation'
  OR request->>'source_plan_sha256' IS DISTINCT FROM NEW.source_plan_sha256
  OR request#>>'{binding,target_id}' IS DISTINCT FROM held.installation_id
  OR request#>>'{binding,restore_id}' IS DISTINCT FROM held.restore_id
  OR request#>>'{binding,snapshot_sha256}' IS DISTINCT FROM imported.snapshot_sha256
  OR request->>'source_id' IS DISTINCT FROM imported.source_id OR request->>'organization_id' IS DISTINCT FROM NEW.organization_id
  OR request->'model_connection_id' IS DISTINCT FROM plan->'model_connection_id'
  OR request->'model_connection_version' IS DISTINCT FROM plan->'model_connection_version'
  OR request->'connection_identity_sha256' IS DISTINCT FROM plan->'connection_identity_sha256'
  OR request->'archived_provider_account' IS DISTINCT FROM plan->'archived_provider_account'
  OR request->'generation_id' IS DISTINCT FROM plan->'generation_id' OR request->'model' IS DISTINCT FROM plan->'model'
  OR policy->>'source_id' IS DISTINCT FROM request->>'source_id' OR policy->>'organization_id' IS DISTINCT FROM request->>'organization_id'
  OR policy->'model_connection_id' IS DISTINCT FROM request->'model_connection_id'
  OR policy->'model_connection_version' IS DISTINCT FROM request->'model_connection_version'
  OR policy->'connection_identity_sha256' IS DISTINCT FROM request->'connection_identity_sha256'
  OR policy->'archived_provider_account' IS DISTINCT FROM request->'archived_provider_account'
  OR policy->'enabled' IS DISTINCT FROM 'true'::jsonb
  OR observation->>'profile' IS DISTINCT FROM 'openrouter-generation-observation-v1'
  OR observation->>'settlement_state' IS DISTINCT FROM 'unverified' OR observation->>'response_sha256' IS DISTINCT FROM NEW.response_sha256
  OR observation->'generation_id' IS DISTINCT FROM request->'generation_id' OR observation->'model' IS DISTINCT FROM request->'model'
  OR provider_response#>'{data,id}' IS DISTINCT FROM request->'generation_id'
  OR provider_response#>'{data,model}' IS DISTINCT FROM request->'model' THEN
  RAISE EXCEPTION 'usage acquisition commitment mismatch' USING ERRCODE='55000';
 END IF;
 IF admission->>'admission_sha256' IS DISTINCT FROM '' OR admission->'version' IS DISTINCT FROM '1'::jsonb
  OR admission->>'algorithm' IS DISTINCT FROM 'recovery-usage-admission-v1'
  OR admission->>'operation_id' IS DISTINCT FROM NEW.id
  OR admission->>'source_plan_sha256' IS DISTINCT FROM NEW.source_plan_sha256
  OR admission->>'acquisition_sha256' IS DISTINCT FROM NEW.acquisition_sha256
  OR admission->>'response_sha256' IS DISTINCT FROM NEW.response_sha256
  OR admission->'target_catalog_sha256' IS DISTINCT FROM plan->'target_catalog_sha256'
  OR admission->'policy_sha256' IS DISTINCT FROM acquisition->'policy_sha256'
  OR admission->'policy_epoch' IS DISTINCT FROM acquisition->'policy_epoch'
  OR admission->'policy_operation_id' IS DISTINCT FROM acquisition->'policy_operation_id'
  OR admission->>'state' IS DISTINCT FROM NEW.observation_state THEN
  RAISE EXCEPTION 'usage admission commitment mismatch' USING ERRCODE='55000';
 END IF;
 NEW.recorded_at=pg_catalog.clock_timestamp();
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_usage_observation_insert BEFORE INSERT ON recovery_usage_observations
 FOR EACH ROW EXECUTE FUNCTION recovery_usage_observation_guard();
CREATE TRIGGER recovery_usage_observation_permanent BEFORE UPDATE OR DELETE OR TRUNCATE ON recovery_usage_observations
 FOR EACH STATEMENT EXECUTE FUNCTION recovery_usage_observation_guard();

DO $$
DECLARE target_schema text:=current_schema(); item record;
BEGIN
 FOR item IN SELECT p.proname,pg_catalog.pg_get_function_identity_arguments(p.oid) AS args
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname=target_schema AND p.proname IN('recovery_accounting_profile_guard','recovery_usage_observation_guard') LOOP
  EXECUTE pg_catalog.format('ALTER FUNCTION %I.%I(%s) SET search_path TO %I,pg_catalog,pg_temp',target_schema,item.proname,item.args,target_schema);
 END LOOP;
END $$;

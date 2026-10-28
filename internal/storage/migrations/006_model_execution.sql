-- SPDX-License-Identifier: Elastic-2.0
CREATE TABLE model_connections (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,version integer NOT NULL CHECK(version>0),model text NOT NULL,
 provider_account text NOT NULL,price_version text NOT NULL,credential_environment text NOT NULL,profile jsonb NOT NULL,
 active boolean NOT NULL DEFAULT true,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id,version)
);
CREATE UNIQUE INDEX model_one_active_name ON model_connections(organization_id,model) WHERE active;
CREATE UNIQUE INDEX model_one_active_id ON model_connections(organization_id,id) WHERE active;
CREATE FUNCTION immutable_model_profile() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.version IS DISTINCT FROM OLD.version
 OR NEW.model IS DISTINCT FROM OLD.model OR NEW.provider_account IS DISTINCT FROM OLD.provider_account OR NEW.price_version IS DISTINCT FROM OLD.price_version
 OR NEW.credential_environment IS DISTINCT FROM OLD.credential_environment OR NEW.profile IS DISTINCT FROM OLD.profile OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
 RAISE EXCEPTION 'model connection version is immutable' USING ERRCODE='23514'; END IF; RETURN NEW;
END $$;
CREATE TRIGGER model_profile_immutable BEFORE UPDATE ON model_connections FOR EACH ROW EXECUTE FUNCTION immutable_model_profile();
ALTER TABLE tasks ADD COLUMN model_connection_id text;
ALTER TABLE tasks ADD COLUMN model_connection_version integer;
ALTER TABLE tasks ADD CONSTRAINT task_model_connection FOREIGN KEY(organization_id,model_connection_id,model_connection_version) REFERENCES model_connections(organization_id,id,version);
ALTER TABLE tasks ADD CONSTRAINT task_model_pin_pair CHECK((model_connection_id IS NULL)=(model_connection_version IS NULL));
CREATE FUNCTION immutable_task_model_pin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.model_connection_id IS DISTINCT FROM OLD.model_connection_id OR NEW.model_connection_version IS DISTINCT FROM OLD.model_connection_version THEN
 RAISE EXCEPTION 'task model profile is immutable' USING ERRCODE='23514'; END IF; RETURN NEW; END $$;
CREATE TRIGGER task_model_pin_immutable BEFORE UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION immutable_task_model_pin();
ALTER TABLE artifacts ADD CONSTRAINT artifact_task_binding UNIQUE(organization_id,id,task_id);
ALTER TABLE reservations ADD CONSTRAINT reservation_full_binding UNIQUE(organization_id,id,operation_id,task_id);
CREATE TABLE model_candidates (
 organization_id text NOT NULL,operation_id text NOT NULL,task_id text NOT NULL,reservation_id text NOT NULL,artifact_id text NOT NULL,
 content_digest text NOT NULL,intent_hash text NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,operation_id),
 FOREIGN KEY(organization_id,operation_id,task_id) REFERENCES operations(organization_id,id,task_id),
 FOREIGN KEY(organization_id,reservation_id,operation_id,task_id) REFERENCES reservations(organization_id,id,operation_id,task_id),
 FOREIGN KEY(organization_id,artifact_id,task_id) REFERENCES artifacts(organization_id,id,task_id)
);
CREATE TRIGGER candidate_receipt_immutable BEFORE UPDATE OR DELETE ON model_candidates FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

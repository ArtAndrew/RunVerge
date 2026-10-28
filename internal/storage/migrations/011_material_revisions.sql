-- SPDX-License-Identifier: Elastic-2.0
-- Logical materials and append-only drafts are separate from immutable result
-- artifacts. Publishing always creates a fresh physical artifact identifier.
ALTER TABLE task_candidates DROP CONSTRAINT task_candidates_organization_id_run_id_key;
ALTER TABLE task_candidates DROP CONSTRAINT task_candidates_source_check;
ALTER TABLE task_candidates ADD CONSTRAINT task_candidate_source CHECK(source IN('model','team','fixture','material'));
CREATE UNIQUE INDEX one_generated_candidate_per_run ON task_candidates(organization_id,run_id) WHERE source IN('model','team','fixture');
ALTER TABLE task_candidates ADD CONSTRAINT candidate_task_identity UNIQUE(organization_id,id,task_id);
ALTER TABLE task_candidates ADD CONSTRAINT candidate_full_operation_identity UNIQUE(organization_id,id,task_id,run_id,operation_id);
ALTER TABLE task_candidates ADD COLUMN parent_candidate_id text;
ALTER TABLE task_candidates ADD COLUMN review_basis_candidate_id text;
ALTER TABLE task_candidates ADD COLUMN material_id text;
ALTER TABLE task_candidates ADD COLUMN material_version integer;
ALTER TABLE task_candidates ADD COLUMN published_by text;

CREATE TABLE material_storage (
 organization_id text PRIMARY KEY REFERENCES organizations(id),
 used_bytes bigint NOT NULL DEFAULT 0 CHECK(used_bytes>=0),
 max_bytes bigint NOT NULL DEFAULT 268435456 CHECK(max_bytes BETWEEN 1 AND 268435456),
 used_documents integer NOT NULL DEFAULT 0 CHECK(used_documents>=0),
 max_documents integer NOT NULL DEFAULT 10000 CHECK(max_documents BETWEEN 1 AND 10000),
 CHECK(used_bytes<=max_bytes),CHECK(used_documents<=max_documents)
);
CREATE TABLE material_task_counts (
 organization_id text NOT NULL,task_id text NOT NULL,documents integer NOT NULL DEFAULT 1 CHECK(documents BETWEEN 1 AND 32),
 PRIMARY KEY(organization_id,task_id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id)
);
CREATE TABLE material_publication_counts (
 organization_id text NOT NULL,task_id text NOT NULL,publications integer NOT NULL DEFAULT 1 CHECK(publications BETWEEN 1 AND 64),
 PRIMARY KEY(organization_id,task_id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id)
);

CREATE TABLE materials (
 organization_id text NOT NULL,id text NOT NULL,task_id text NOT NULL,
 name text NOT NULL CHECK(octet_length(name) BETWEEN 1 AND 120),
 media_type text NOT NULL CHECK(media_type IN('text/plain','text/markdown','application/json')),
 head_version integer NOT NULL DEFAULT 1 CHECK(head_version BETWEEN 1 AND 128),
 base_candidate_id text,created_by text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),UNIQUE(organization_id,id,task_id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id),
 FOREIGN KEY(organization_id,base_candidate_id,task_id) REFERENCES task_candidates(organization_id,id,task_id),
 FOREIGN KEY(organization_id,created_by) REFERENCES memberships(organization_id,principal_id)
);
CREATE INDEX material_task_listing ON materials(organization_id,task_id,created_at DESC,id DESC);
CREATE TABLE material_revisions (
 organization_id text NOT NULL,id text NOT NULL,task_id text NOT NULL,material_id text NOT NULL,
 version integer NOT NULL CHECK(version BETWEEN 1 AND 128),base_version integer NOT NULL CHECK(base_version>=0 AND version=base_version+1),
 merge_base_version integer CHECK(merge_base_version BETWEEN 1 AND base_version),
 content text NOT NULL CHECK(octet_length(content)<=262144),
 content_digest text NOT NULL CHECK(length(content_digest)=64 AND content_digest ~ '^[0-9a-f]{64}$'),
 principal_id text NOT NULL,reason text NOT NULL DEFAULT '' CHECK(octet_length(reason)<=4096),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),UNIQUE(organization_id,material_id,version),
 UNIQUE(organization_id,material_id,version,content_digest,task_id),
 FOREIGN KEY(organization_id,material_id,task_id) REFERENCES materials(organization_id,id,task_id),
 FOREIGN KEY(organization_id,material_id,merge_base_version) REFERENCES material_revisions(organization_id,material_id,version),
 FOREIGN KEY(organization_id,principal_id) REFERENCES memberships(organization_id,principal_id)
);
ALTER TABLE materials ADD CONSTRAINT material_current_revision FOREIGN KEY(organization_id,id,head_version) REFERENCES material_revisions(organization_id,material_id,version) DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX material_revision_listing ON material_revisions(organization_id,material_id,version DESC);
CREATE TRIGGER material_revision_immutable BEFORE UPDATE OR DELETE ON material_revisions FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER material_no_delete BEFORE DELETE ON materials FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER material_storage_no_delete BEFORE DELETE ON material_storage FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER material_task_counts_no_delete BEFORE DELETE ON material_task_counts FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER material_publication_counts_no_delete BEFORE DELETE ON material_publication_counts FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE FUNCTION protect_material_head() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id
  OR NEW.name IS DISTINCT FROM OLD.name OR NEW.media_type IS DISTINCT FROM OLD.media_type OR NEW.base_candidate_id IS DISTINCT FROM OLD.base_candidate_id
  OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.head_version<>OLD.head_version+1 THEN
  RAISE EXCEPTION 'material identity and sequential history are immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER material_head_guard BEFORE UPDATE ON materials FOR EACH ROW EXECUTE FUNCTION protect_material_head();
CREATE FUNCTION protect_material_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id THEN RAISE EXCEPTION 'material quota identity is immutable' USING ERRCODE='23514'; END IF;
 IF TG_TABLE_NAME='material_storage' THEN
  IF NEW.max_bytes IS DISTINCT FROM OLD.max_bytes OR NEW.max_documents IS DISTINCT FROM OLD.max_documents
   OR NEW.used_bytes<OLD.used_bytes OR NEW.used_documents<OLD.used_documents THEN RAISE EXCEPTION 'material quotas cannot reset' USING ERRCODE='23514'; END IF;
 ELSE
  IF NEW.task_id IS DISTINCT FROM OLD.task_id THEN RAISE EXCEPTION 'material task identity is immutable' USING ERRCODE='23514'; END IF;
  IF TG_TABLE_NAME='material_task_counts' THEN
   IF NEW.documents<>OLD.documents+1 THEN RAISE EXCEPTION 'material count cannot reset' USING ERRCODE='23514'; END IF;
  ELSIF TG_TABLE_NAME='material_publication_counts' THEN
   IF NEW.publications<>OLD.publications+1 THEN RAISE EXCEPTION 'material publications cannot reset' USING ERRCODE='23514'; END IF;
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER material_storage_guard BEFORE UPDATE ON material_storage FOR EACH ROW EXECUTE FUNCTION protect_material_usage();
CREATE TRIGGER material_task_counts_guard BEFORE UPDATE ON material_task_counts FOR EACH ROW EXECUTE FUNCTION protect_material_usage();
CREATE TRIGGER material_publication_counts_guard BEFORE UPDATE ON material_publication_counts FOR EACH ROW EXECUTE FUNCTION protect_material_usage();

CREATE FUNCTION account_material_document() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO material_storage(organization_id) VALUES(NEW.organization_id) ON CONFLICT DO NOTHING;
 UPDATE material_storage SET used_documents=used_documents+1 WHERE organization_id=NEW.organization_id AND used_documents<max_documents;
 IF NOT FOUND THEN RAISE EXCEPTION 'organization material document quota exceeded' USING ERRCODE='23514'; END IF;
 INSERT INTO material_task_counts(organization_id,task_id) VALUES(NEW.organization_id,NEW.task_id)
 ON CONFLICT(organization_id,task_id) DO UPDATE SET documents=material_task_counts.documents+1 WHERE material_task_counts.documents<32;
 IF NOT FOUND THEN RAISE EXCEPTION 'task material document quota exceeded' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER material_document_account AFTER INSERT ON materials FOR EACH ROW EXECUTE FUNCTION account_material_document();
CREATE FUNCTION account_material_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE head integer; bytes bigint;
BEGIN
 SELECT head_version INTO head FROM materials WHERE organization_id=NEW.organization_id AND id=NEW.material_id AND task_id=NEW.task_id;
 IF NOT FOUND OR (NEW.version=1 AND head<>1) OR (NEW.version>1 AND NEW.base_version<>head) THEN
  RAISE EXCEPTION 'material revision does not follow current head' USING ERRCODE='23514'; END IF;
 bytes=octet_length(NEW.content)::bigint+octet_length(NEW.reason)::bigint;
 UPDATE material_storage SET used_bytes=used_bytes+bytes WHERE organization_id=NEW.organization_id AND bytes<=max_bytes-used_bytes;
 IF NOT FOUND THEN RAISE EXCEPTION 'organization material byte quota exceeded' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER material_revision_account BEFORE INSERT ON material_revisions FOR EACH ROW EXECUTE FUNCTION account_material_revision();

ALTER TABLE task_candidates ADD CONSTRAINT candidate_parent_binding FOREIGN KEY(organization_id,parent_candidate_id,task_id,run_id,operation_id) REFERENCES task_candidates(organization_id,id,task_id,run_id,operation_id);
ALTER TABLE task_candidates ADD CONSTRAINT candidate_review_basis_binding FOREIGN KEY(organization_id,review_basis_candidate_id,task_id,run_id,operation_id) REFERENCES task_candidates(organization_id,id,task_id,run_id,operation_id);
ALTER TABLE task_candidates ADD CONSTRAINT candidate_material_binding FOREIGN KEY(organization_id,material_id,material_version,content_digest,task_id) REFERENCES material_revisions(organization_id,material_id,version,content_digest,task_id);
ALTER TABLE task_candidates ADD CONSTRAINT candidate_publisher_binding FOREIGN KEY(organization_id,published_by) REFERENCES memberships(organization_id,principal_id);
ALTER TABLE task_candidates ADD CONSTRAINT candidate_material_shape CHECK(
 (source='material' AND parent_candidate_id IS NOT NULL AND review_basis_candidate_id IS NOT NULL AND material_id IS NOT NULL AND material_version IS NOT NULL AND published_by IS NOT NULL)
 OR (source<>'material' AND parent_candidate_id IS NULL AND review_basis_candidate_id IS NULL AND material_id IS NULL AND material_version IS NULL AND published_by IS NULL));
CREATE FUNCTION guard_material_candidate() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE parent task_candidates; basis task_candidates; current_task tasks; bytes bigint;
BEGIN
 IF NEW.source<>'material' THEN RETURN NEW; END IF;
 SELECT * INTO parent FROM task_candidates WHERE organization_id=NEW.organization_id AND id=NEW.parent_candidate_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'missing material predecessor' USING ERRCODE='23514'; END IF;
 SELECT * INTO basis FROM task_candidates WHERE organization_id=NEW.organization_id AND id=NEW.review_basis_candidate_id;
 IF NOT FOUND OR basis.source='material' OR basis.id<>(CASE WHEN parent.source='material' THEN parent.review_basis_candidate_id ELSE parent.id END) THEN
  RAISE EXCEPTION 'material review basis cannot change' USING ERRCODE='23514'; END IF;
 SELECT * INTO current_task FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
 IF NOT FOUND OR current_task.status<>'waiting_input' OR current_task.current_run_id IS DISTINCT FROM NEW.run_id OR current_task.candidate_id IS DISTINCT FROM parent.id THEN
  RAISE EXCEPTION 'material candidate requires current waiting result' USING ERRCODE='23514'; END IF;
 INSERT INTO material_publication_counts(organization_id,task_id) VALUES(NEW.organization_id,NEW.task_id)
 ON CONFLICT(organization_id,task_id) DO UPDATE SET publications=material_publication_counts.publications+1 WHERE material_publication_counts.publications<64;
 IF NOT FOUND THEN RAISE EXCEPTION 'task material publication quota exceeded' USING ERRCODE='23514'; END IF;
 SELECT octet_length(content)::bigint INTO bytes FROM material_revisions WHERE organization_id=NEW.organization_id AND material_id=NEW.material_id AND version=NEW.material_version;
 UPDATE material_storage SET used_bytes=used_bytes+bytes WHERE organization_id=NEW.organization_id AND bytes<=max_bytes-used_bytes;
 IF NOT FOUND THEN RAISE EXCEPTION 'organization material byte quota exceeded' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER material_candidate_guard BEFORE INSERT ON task_candidates FOR EACH ROW EXECUTE FUNCTION guard_material_candidate();

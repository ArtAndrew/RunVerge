-- SPDX-License-Identifier: Elastic-2.0
-- Historical JSON remains unmodified and explicitly untyped (version zero).
-- New application producers validate version-one payloads before insertion.
ALTER TABLE events ADD COLUMN payload_version integer NOT NULL DEFAULT 0 CHECK(payload_version IN(0,1));
CREATE TRIGGER event_history_immutable BEFORE UPDATE OR DELETE ON events FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

-- This is a logical replay boundary, not deletion or archival. Durable events
-- and their outbox/financial/deduplication obligations remain intact.
CREATE TABLE task_event_windows (
 organization_id text NOT NULL, task_id text NOT NULL,
 after_sequence bigint NOT NULL DEFAULT 0 CHECK(after_sequence>=0),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,task_id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id)
);
INSERT INTO task_event_windows(organization_id,task_id) SELECT organization_id,id FROM tasks;
CREATE FUNCTION initialize_task_event_window() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO task_event_windows(organization_id,task_id) VALUES(NEW.organization_id,NEW.id);
 RETURN NEW;
END $$;
CREATE TRIGGER task_initialize_event_window AFTER INSERT ON tasks FOR EACH ROW EXECUTE FUNCTION initialize_task_event_window();
CREATE FUNCTION guard_task_event_window() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE head bigint;
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.after_sequence<>0 OR NEW.version<>1 THEN
   RAISE EXCEPTION 'event window must start at zero' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END IF;
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.task_id IS DISTINCT FROM OLD.task_id
  OR NEW.after_sequence<=OLD.after_sequence OR NEW.version NOT IN(OLD.version,OLD.version+1)
  OR NEW.updated_at IS DISTINCT FROM OLD.updated_at THEN
  RAISE EXCEPTION 'event window cannot rewind or change identity' USING ERRCODE='23514';
 END IF;
 SELECT COALESCE(max(sequence),0) INTO head FROM events WHERE organization_id=NEW.organization_id AND task_id=NEW.task_id;
 IF NEW.after_sequence>head THEN
  RAISE EXCEPTION 'event window exceeds task history' USING ERRCODE='23514';
 END IF;
 NEW.version=OLD.version+1;
 NEW.updated_at=GREATEST(clock_timestamp(),OLD.updated_at+interval '1 microsecond');
 RETURN NEW;
END $$;
CREATE TRIGGER task_event_window_guard BEFORE INSERT OR UPDATE ON task_event_windows FOR EACH ROW EXECUTE FUNCTION guard_task_event_window();
CREATE TRIGGER task_event_window_no_delete BEFORE DELETE ON task_event_windows FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TABLE event_replay_window_audit (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL,
 old_after_sequence bigint NOT NULL CHECK(old_after_sequence>=0),
 new_after_sequence bigint NOT NULL CHECK(new_after_sequence>old_after_sequence),
 reason text NOT NULL CHECK(octet_length(reason) BETWEEN 1 AND 4096),
 actor_type text NOT NULL DEFAULT 'trusted_host' CHECK(actor_type='trusted_host'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id)
);
CREATE INDEX event_replay_audit_listing ON event_replay_window_audit(organization_id,task_id,created_at DESC,id DESC);
CREATE TRIGGER event_replay_audit_immutable BEFORE UPDATE OR DELETE ON event_replay_window_audit FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

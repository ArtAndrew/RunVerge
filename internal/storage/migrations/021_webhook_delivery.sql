-- SPDX-License-Identifier: Elastic-2.0
-- New webhook state only. Never apply to the saved main profile before upgrade acceptance.
CREATE TABLE webhook_destinations (
 organization_id text NOT NULL REFERENCES organizations(id), id text NOT NULL,
 version integer NOT NULL CHECK(version>0), profile jsonb NOT NULL CHECK(jsonb_typeof(profile)='object'),
 profile_digest text NOT NULL CHECK(profile_digest~'^[a-f0-9]{64}$'), active boolean NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id,version),
 CHECK(octet_length(profile::text)<=131072),
 CHECK((profile->'destination'->>'id'=id AND profile->'destination'->>'organization_id'=organization_id
  AND profile->'destination'->'version'=to_jsonb(version)
  AND profile->'destination'->'unmetered_approved'='true'::jsonb
  AND profile->'destination'->>'signing_key_id'~'^[A-Za-z0-9_-]{1,128}$'
  AND profile->>'signing_key_environment'~'^PLATFORM_WEBHOOK_KEY_[A-Z0-9_]{1,64}$'
  AND profile->>'endpoint'~'^https://') IS TRUE)
);
CREATE UNIQUE INDEX webhook_destination_active ON webhook_destinations(organization_id,id) WHERE active;

CREATE TABLE webhook_subscriptions (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL,
 creator_id text NOT NULL, owner_id text NOT NULL,
 creator_authority_epoch bigint NOT NULL CHECK(creator_authority_epoch>0),
 creator_policy_epoch bigint NOT NULL CHECK(creator_policy_epoch>0),
 owner_authority_epoch bigint NOT NULL CHECK(owner_authority_epoch>0),
 owner_policy_epoch bigint NOT NULL CHECK(owner_policy_epoch>0),
 destination_id text NOT NULL, destination_version integer NOT NULL,
 profile_digest text NOT NULL CHECK(profile_digest~'^[a-f0-9]{64}$'),
 event_types text[] NOT NULL CHECK(cardinality(event_types) BETWEEN 1 AND 16),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),
 status text NOT NULL DEFAULT 'active' CHECK(status IN('active','exhausted','revoked')),
 start_after_sequence bigint NOT NULL CHECK(start_after_sequence>=0),
 last_admitted_sequence bigint NOT NULL DEFAULT 0 CHECK(last_admitted_sequence>=0),
 max_notifications integer NOT NULL CHECK(max_notifications BETWEEN 1 AND 1024),
 admitted_notifications integer NOT NULL DEFAULT 0 CHECK(admitted_notifications BETWEEN 0 AND max_notifications),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), expires_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,id), UNIQUE(organization_id,id,task_id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id),
 FOREIGN KEY(organization_id,creator_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,owner_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,destination_id,destination_version) REFERENCES webhook_destinations(organization_id,id,version),
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '24 hours'),
 CHECK((admitted_notifications=0 AND last_admitted_sequence=0) OR (admitted_notifications>0 AND last_admitted_sequence>start_after_sequence)),
 CHECK(status<>'exhausted' OR admitted_notifications=max_notifications),
 CHECK(status<>'active' OR admitted_notifications<max_notifications)
);
CREATE INDEX webhook_subscriptions_match ON webhook_subscriptions(organization_id,task_id) WHERE status='active';

CREATE UNIQUE INDEX webhook_event_identity ON events(organization_id,id,task_id,sequence);
CREATE TABLE webhook_deliveries (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL, subscription_id text NOT NULL,
 destination_id text NOT NULL, destination_version integer NOT NULL,
 profile_digest text NOT NULL CHECK(profile_digest~'^[a-f0-9]{64}$'),
 event_id text NOT NULL, event_sequence bigint NOT NULL CHECK(event_sequence>0),
 envelope text NOT NULL CHECK(octet_length(envelope) BETWEEN 1 AND 4096),
 envelope_digest text NOT NULL CHECK(envelope_digest=encode(sha256(convert_to(envelope,'UTF8')),'hex')),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),
 status text NOT NULL DEFAULT 'queued' CHECK(status IN('queued','leased','sent','unknown','acknowledged','not_acknowledged','stopped','expired')),
 attempt_count integer NOT NULL DEFAULT 1 CHECK(attempt_count BETWEEN 1 AND 8),
 max_attempts integer NOT NULL DEFAULT 8 CHECK(max_attempts=8), current_attempt_id text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), expires_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,id), UNIQUE(organization_id,id,task_id),
 UNIQUE(organization_id,subscription_id,event_sequence),
 FOREIGN KEY(organization_id,subscription_id,task_id) REFERENCES webhook_subscriptions(organization_id,id,task_id),
 FOREIGN KEY(organization_id,destination_id,destination_version) REFERENCES webhook_destinations(organization_id,id,version),
 FOREIGN KEY(organization_id,event_id,task_id,event_sequence) REFERENCES events(organization_id,id,task_id,sequence),
 CHECK(jsonb_typeof(envelope::jsonb)='object'),
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '24 hours')
);
CREATE INDEX webhook_deliveries_pending ON webhook_deliveries(created_at,id) WHERE status IN('queued','leased','sent');

CREATE TABLE webhook_attempts (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL, delivery_id text NOT NULL,
 number integer NOT NULL CHECK(number BETWEEN 1 AND 8),
 status text NOT NULL DEFAULT 'queued' CHECK(status IN('queued','leased','sent','unknown','acknowledged','not_acknowledged','stopped','expired')),
 http_status integer NOT NULL DEFAULT 0 CHECK(http_status=0 OR http_status BETWEEN 100 AND 599),
 reason_code text NOT NULL DEFAULT '' CHECK(reason_code~'^[a-z0-9_]{0,64}$'),
 worker_id text, lease_until timestamptz, fencing_token bigint NOT NULL DEFAULT 0 CHECK(fencing_token>=0),
 recoveries integer NOT NULL DEFAULT 0 CHECK(recoveries BETWEEN 0 AND 5),
 signature_timestamp bigint CHECK(signature_timestamp>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), sent_at timestamptz, observed_at timestamptz, unknown_at timestamptz,
 PRIMARY KEY(organization_id,id), UNIQUE(organization_id,delivery_id,number),
 UNIQUE(organization_id,id,delivery_id),
 FOREIGN KEY(organization_id,delivery_id,task_id) REFERENCES webhook_deliveries(organization_id,id,task_id),
 CHECK((worker_id IS NULL)=(lease_until IS NULL)),
 CHECK(status<>'leased' OR (worker_id IS NOT NULL AND fencing_token>0)),
 CHECK((sent_at IS NULL)=(signature_timestamp IS NULL)),
 CHECK(sent_at IS NULL OR status IN('sent','unknown','acknowledged','not_acknowledged')),
 CHECK(unknown_at IS NULL OR sent_at IS NOT NULL),
 CHECK(status NOT IN('sent','unknown','acknowledged','not_acknowledged') OR (sent_at IS NOT NULL AND signature_timestamp IS NOT NULL)),
 CHECK(status<>'unknown' OR unknown_at IS NOT NULL),
 CHECK((observed_at IS NOT NULL)=(status IN('acknowledged','not_acknowledged'))),
 CHECK((status='acknowledged' AND http_status BETWEEN 200 AND 299)
  OR (status='not_acknowledged' AND http_status BETWEEN 100 AND 599 AND http_status NOT BETWEEN 200 AND 299)
  OR (status NOT IN('acknowledged','not_acknowledged') AND http_status=0))
);
ALTER TABLE webhook_deliveries ADD CONSTRAINT webhook_current_attempt
 FOREIGN KEY(organization_id,current_attempt_id,id) REFERENCES webhook_attempts(organization_id,id,delivery_id)
 DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX webhook_attempts_claim ON webhook_attempts(created_at,id) WHERE status IN('queued','leased');

CREATE TABLE webhook_audit (
 organization_id text NOT NULL REFERENCES organizations(id), id text NOT NULL, task_id text NOT NULL,
 actor_id text, object_id text NOT NULL, action text NOT NULL, reason_code text NOT NULL DEFAULT '',
 reason_digest text NOT NULL DEFAULT '' CHECK(reason_digest='' OR reason_digest~'^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id),
 FOREIGN KEY(organization_id,actor_id) REFERENCES memberships(organization_id,principal_id),
 CHECK(action~'^[a-z0-9_.]{1,64}$'), CHECK(reason_code~'^[a-z0-9_]{0,64}$')
);

CREATE FUNCTION guard_webhook_destination() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.organization_id,NEW.id,NEW.version,NEW.profile,NEW.profile_digest,NEW.created_at)
 IS DISTINCT FROM ROW(OLD.organization_id,OLD.id,OLD.version,OLD.profile,OLD.profile_digest,OLD.created_at)
 OR (NEW.active AND NOT OLD.active) THEN RAISE EXCEPTION 'immutable webhook profile' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER webhook_destination_guard BEFORE UPDATE ON webhook_destinations FOR EACH ROW EXECUTE FUNCTION guard_webhook_destination();

CREATE FUNCTION guard_webhook_subscription() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.organization_id,NEW.id,NEW.task_id,NEW.creator_id,NEW.owner_id,NEW.creator_authority_epoch,NEW.creator_policy_epoch,
 NEW.owner_authority_epoch,NEW.owner_policy_epoch,NEW.destination_id,NEW.destination_version,NEW.profile_digest,NEW.event_types,
 NEW.start_after_sequence,NEW.max_notifications,NEW.created_at,NEW.expires_at)
 IS DISTINCT FROM ROW(OLD.organization_id,OLD.id,OLD.task_id,OLD.creator_id,OLD.owner_id,OLD.creator_authority_epoch,OLD.creator_policy_epoch,
 OLD.owner_authority_epoch,OLD.owner_policy_epoch,OLD.destination_id,OLD.destination_version,OLD.profile_digest,OLD.event_types,
 OLD.start_after_sequence,OLD.max_notifications,OLD.created_at,OLD.expires_at)
 OR NEW.version<>OLD.version+1 OR NEW.admitted_notifications NOT BETWEEN OLD.admitted_notifications AND OLD.admitted_notifications+1
 OR ((NEW.admitted_notifications=OLD.admitted_notifications+1)<>(NEW.last_admitted_sequence>OLD.last_admitted_sequence))
 OR NEW.last_admitted_sequence<OLD.last_admitted_sequence OR (OLD.status<>'active' AND NEW.admitted_notifications<>OLD.admitted_notifications)
 OR (OLD.status='revoked' AND NEW.status<>'revoked') OR (OLD.status='exhausted' AND NEW.status NOT IN('exhausted','revoked')) THEN
 RAISE EXCEPTION 'immutable webhook subscription or counters' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER webhook_subscription_guard BEFORE UPDATE ON webhook_subscriptions FOR EACH ROW EXECUTE FUNCTION guard_webhook_subscription();

CREATE FUNCTION guard_webhook_delivery() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.organization_id,NEW.id,NEW.task_id,NEW.subscription_id,NEW.destination_id,NEW.destination_version,NEW.profile_digest,
 NEW.event_id,NEW.event_sequence,NEW.envelope,NEW.envelope_digest,NEW.max_attempts,NEW.created_at,NEW.expires_at)
 IS DISTINCT FROM ROW(OLD.organization_id,OLD.id,OLD.task_id,OLD.subscription_id,OLD.destination_id,OLD.destination_version,OLD.profile_digest,
 OLD.event_id,OLD.event_sequence,OLD.envelope,OLD.envelope_digest,OLD.max_attempts,OLD.created_at,OLD.expires_at)
 OR NEW.version<>OLD.version+1 OR NEW.attempt_count NOT BETWEEN OLD.attempt_count AND OLD.attempt_count+1
 OR (OLD.status='acknowledged' AND NEW.status<>'acknowledged')
 OR NEW.current_attempt_id<>'wha_'||encode(sha256(convert_to(NEW.id||chr(31)||NEW.attempt_count::text,'UTF8')),'hex')
 OR (NEW.attempt_count=OLD.attempt_count+1 AND (NEW.status<>'queued' OR OLD.status NOT IN('unknown','not_acknowledged','stopped')))
 OR (NEW.status<>'acknowledged' AND EXISTS(SELECT 1 FROM webhook_attempts WHERE organization_id=NEW.organization_id AND delivery_id=NEW.id AND status='acknowledged'))
 OR ((NEW.current_attempt_id<>OLD.current_attempt_id)<>(NEW.attempt_count=OLD.attempt_count+1)) THEN
 RAISE EXCEPTION 'immutable webhook envelope or attempt lifetime' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER webhook_delivery_guard BEFORE UPDATE ON webhook_deliveries FOR EACH ROW EXECUTE FUNCTION guard_webhook_delivery();

CREATE FUNCTION guard_webhook_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.organization_id,NEW.id,NEW.task_id,NEW.delivery_id,NEW.number,NEW.created_at)
 IS DISTINCT FROM ROW(OLD.organization_id,OLD.id,OLD.task_id,OLD.delivery_id,OLD.number,OLD.created_at)
 OR NEW.fencing_token<OLD.fencing_token OR NEW.recoveries<OLD.recoveries
 OR (OLD.sent_at IS NOT NULL AND ROW(NEW.sent_at,NEW.signature_timestamp) IS DISTINCT FROM ROW(OLD.sent_at,OLD.signature_timestamp))
 OR (OLD.sent_at IS NOT NULL AND ROW(NEW.worker_id,NEW.lease_until,NEW.fencing_token,NEW.recoveries) IS DISTINCT FROM ROW(OLD.worker_id,OLD.lease_until,OLD.fencing_token,OLD.recoveries))
 OR (OLD.status='unknown' AND NEW.status='sent')
 OR (OLD.status IN('stopped','expired') AND NEW IS DISTINCT FROM OLD)
 OR (OLD.status='queued' AND NEW.status NOT IN('queued','leased','stopped','expired'))
 OR (OLD.status='leased' AND NEW.status NOT IN('leased','sent','stopped','expired'))
 OR (OLD.unknown_at IS NOT NULL AND NEW.unknown_at IS DISTINCT FROM OLD.unknown_at)
 OR (OLD.sent_at IS NOT NULL AND NEW.status NOT IN('sent','unknown','acknowledged','not_acknowledged'))
 OR (OLD.sent_at IS NULL AND NEW.sent_at IS NOT NULL AND (OLD.status<>'leased' OR NEW.status<>'sent' OR NEW.worker_id IS NULL OR NEW.fencing_token<=0))
 OR (OLD.observed_at IS NOT NULL AND ROW(NEW.observed_at,NEW.http_status,NEW.status,NEW.reason_code) IS DISTINCT FROM ROW(OLD.observed_at,OLD.http_status,OLD.status,OLD.reason_code)) THEN
 RAISE EXCEPTION 'immutable webhook physical attempt or observation' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER webhook_attempt_guard BEFORE UPDATE ON webhook_attempts FOR EACH ROW EXECUTE FUNCTION guard_webhook_attempt();

CREATE TRIGGER webhook_destination_no_delete BEFORE DELETE ON webhook_destinations FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER webhook_subscription_no_delete BEFORE DELETE ON webhook_subscriptions FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER webhook_delivery_no_delete BEFORE DELETE ON webhook_deliveries FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER webhook_attempt_no_delete BEFORE DELETE ON webhook_attempts FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER webhook_audit_no_update BEFORE UPDATE OR DELETE ON webhook_audit FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

-- Relational intent is checked as well as tenant FKs. A notification can never
-- manufacture another destination, source event, payload or initial send fact.
CREATE FUNCTION validate_webhook_subscription_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actual_owner text; destination_digest text;
BEGIN
 SELECT owner_id INTO actual_owner FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
 SELECT profile_digest INTO destination_digest FROM webhook_destinations
  WHERE organization_id=NEW.organization_id AND id=NEW.destination_id AND version=NEW.destination_version AND active;
 IF NEW.owner_id IS DISTINCT FROM actual_owner OR NEW.profile_digest IS DISTINCT FROM destination_digest
 OR NEW.version<>1 OR NEW.status<>'active' OR NEW.admitted_notifications<>0 OR NEW.last_admitted_sequence<>0
 OR NEW.start_after_sequence<>(SELECT COALESCE(max(sequence),0) FROM events WHERE organization_id=NEW.organization_id)
 OR array_position(NEW.event_types,NULL) IS NOT NULL
 OR cardinality(NEW.event_types)<>(SELECT count(DISTINCT x) FROM unnest(NEW.event_types) x)
 OR NEW.creator_authority_epoch IS DISTINCT FROM current_member_epoch(NEW.organization_id,NEW.creator_id)
 OR NEW.owner_authority_epoch IS DISTINCT FROM current_member_epoch(NEW.organization_id,NEW.owner_id)
 OR NEW.creator_policy_epoch IS DISTINCT FROM current_principal_policy_epoch(NEW.organization_id,NEW.creator_id)
 OR NEW.owner_policy_epoch IS DISTINCT FROM current_principal_policy_epoch(NEW.organization_id,NEW.owner_id) THEN
 RAISE EXCEPTION 'invalid webhook subscription intent' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER webhook_subscription_insert BEFORE INSERT ON webhook_subscriptions FOR EACH ROW EXECUTE FUNCTION validate_webhook_subscription_insert();

CREATE FUNCTION validate_webhook_delivery_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s webhook_subscriptions%ROWTYPE; e events%ROWTYPE; expected jsonb; expected_id text;
BEGIN
 SELECT * INTO s FROM webhook_subscriptions WHERE organization_id=NEW.organization_id AND id=NEW.subscription_id AND task_id=NEW.task_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid webhook subscription binding' USING ERRCODE='23514'; END IF;
 SELECT * INTO e FROM events WHERE organization_id=NEW.organization_id AND id=NEW.event_id AND task_id=NEW.task_id AND sequence=NEW.event_sequence;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid webhook event binding' USING ERRCODE='23514'; END IF;
 expected_id='whd_'||encode(sha256(convert_to(NEW.organization_id||chr(31)||s.id||chr(31)||e.id,'UTF8')),'hex');
 expected=jsonb_build_object('envelope_version',1,'delivery_id',expected_id,'organization_id',NEW.organization_id,'subscription_id',s.id,
  'event',jsonb_build_object('id',e.id,'task_id',e.task_id,'sequence',e.sequence::text,'type',e.type,'payload_version',e.payload_version,'created_at',e.created_at));
 IF ROW(NEW.destination_id,NEW.destination_version,NEW.profile_digest,NEW.expires_at)
 IS DISTINCT FROM ROW(s.destination_id,s.destination_version,s.profile_digest,s.expires_at)
 OR NEW.id<>expected_id OR NEW.envelope IS DISTINCT FROM expected::text
 OR NEW.current_attempt_id<>'wha_'||encode(sha256(convert_to(expected_id||chr(31)||'1','UTF8')),'hex')
 OR NEW.version<>1 OR NEW.status<>'queued' OR NEW.attempt_count<>1
 OR s.status<>'active' OR s.admitted_notifications>=s.max_notifications
 OR e.sequence<=s.start_after_sequence OR e.sequence<=s.last_admitted_sequence OR NOT(e.type=ANY(s.event_types))
 OR NEW.created_at<s.created_at THEN
 RAISE EXCEPTION 'invalid webhook delivery intent' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER webhook_delivery_insert BEFORE INSERT ON webhook_deliveries FOR EACH ROW EXECUTE FUNCTION validate_webhook_delivery_insert();

CREATE FUNCTION validate_webhook_attempt_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE d webhook_deliveries%ROWTYPE;
BEGIN
 SELECT * INTO d FROM webhook_deliveries WHERE organization_id=NEW.organization_id AND id=NEW.delivery_id AND task_id=NEW.task_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid webhook delivery binding' USING ERRCODE='23514'; END IF;
 IF NEW.id IS DISTINCT FROM d.current_attempt_id OR NEW.number<>d.attempt_count
 OR NEW.id<>'wha_'||encode(sha256(convert_to(d.id||chr(31)||NEW.number::text,'UTF8')),'hex')
 OR NEW.status<>'queued' OR NEW.http_status<>0 OR NEW.reason_code<>'' OR NEW.worker_id IS NOT NULL OR NEW.lease_until IS NOT NULL
 OR NEW.fencing_token<>0 OR NEW.recoveries<>0 OR NEW.signature_timestamp IS NOT NULL
 OR NEW.sent_at IS NOT NULL OR NEW.observed_at IS NOT NULL OR NEW.unknown_at IS NOT NULL
 OR NEW.created_at<d.created_at OR NEW.created_at>=d.expires_at
 OR (NEW.number=1 AND NEW.created_at<>d.created_at) THEN
 RAISE EXCEPTION 'invalid webhook physical attempt intent' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER webhook_attempt_insert BEFORE INSERT ON webhook_attempts FOR EACH ROW EXECUTE FUNCTION validate_webhook_attempt_insert();

CREATE FUNCTION enqueue_webhook_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s webhook_subscriptions%ROWTYPE; delivery text; attempt text; body text; expires timestamptz; admitted_at timestamptz;
BEGIN
 -- emit() already holds the organization allocation gate. No IAM locks are
 -- acquired here; final execution independently checks current consent.
 PERFORM id FROM organizations WHERE id=NEW.organization_id FOR UPDATE;
 FOR s IN SELECT * FROM webhook_subscriptions WHERE organization_id=NEW.organization_id AND task_id=NEW.task_id
  AND status='active' AND expires_at>clock_timestamp() AND NEW.sequence>start_after_sequence AND NEW.type=ANY(event_types)
  ORDER BY id FOR UPDATE LOOP
  IF s.admitted_notifications>=s.max_notifications THEN RAISE EXCEPTION 'webhook admission invariant' USING ERRCODE='23514'; END IF;
  delivery='whd_'||encode(sha256(convert_to(NEW.organization_id||chr(31)||s.id||chr(31)||NEW.id,'UTF8')),'hex');
  attempt='wha_'||encode(sha256(convert_to(delivery||chr(31)||'1','UTF8')),'hex');
  body=jsonb_build_object('envelope_version',1,'delivery_id',delivery,'organization_id',NEW.organization_id,'subscription_id',s.id,
   'event',jsonb_build_object('id',NEW.id,'task_id',NEW.task_id,'sequence',NEW.sequence::text,'type',NEW.type,
   'payload_version',NEW.payload_version,'created_at',NEW.created_at))::text;
  -- Lifetime is measured from admission, not a possibly older transaction's
  -- now()-based event timestamp. Recheck expiry after all lock waits.
  expires=s.expires_at;
  admitted_at=clock_timestamp();
  IF expires<=admitted_at THEN CONTINUE; END IF;
  INSERT INTO webhook_deliveries(organization_id,id,task_id,subscription_id,destination_id,destination_version,profile_digest,
   event_id,event_sequence,envelope,envelope_digest,current_attempt_id,created_at,expires_at)
  VALUES(NEW.organization_id,delivery,NEW.task_id,s.id,s.destination_id,s.destination_version,s.profile_digest,
   NEW.id,NEW.sequence,body,encode(sha256(convert_to(body,'UTF8')),'hex'),attempt,admitted_at,expires);
  INSERT INTO webhook_attempts(organization_id,id,task_id,delivery_id,number,created_at) VALUES(NEW.organization_id,attempt,NEW.task_id,delivery,1,admitted_at);
  UPDATE webhook_subscriptions SET admitted_notifications=admitted_notifications+1,last_admitted_sequence=NEW.sequence,
   status=CASE WHEN admitted_notifications+1=max_notifications THEN 'exhausted' ELSE 'active' END,version=version+1
   WHERE organization_id=s.organization_id AND id=s.id;
 END LOOP;
 RETURN NEW;
END $$;
CREATE TRIGGER webhook_event_fanout AFTER INSERT ON events FOR EACH ROW EXECUTE FUNCTION enqueue_webhook_event();

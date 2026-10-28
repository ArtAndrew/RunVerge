-- SPDX-License-Identifier: Elastic-2.0
CREATE TABLE organizations (
 id text PRIMARY KEY, name text NOT NULL,
 budget_microusd bigint NOT NULL CHECK(budget_microusd>=0),
 reserved_microusd bigint NOT NULL DEFAULT 0 CHECK(reserved_microusd>=0),
 spent_microusd bigint NOT NULL DEFAULT 0 CHECK(spent_microusd>=0),
 active boolean NOT NULL DEFAULT true, created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(reserved_microusd+spent_microusd<=budget_microusd)
);
CREATE TABLE principals (id text PRIMARY KEY, name text NOT NULL, active boolean NOT NULL DEFAULT true);
CREATE TABLE memberships (
 organization_id text NOT NULL REFERENCES organizations(id), principal_id text NOT NULL REFERENCES principals(id),
 role text NOT NULL CHECK(role IN ('owner','admin','member','viewer')), active boolean NOT NULL DEFAULT true,
 PRIMARY KEY(organization_id,principal_id)
);
CREATE TABLE access_tokens (
 token_hash text PRIMARY KEY, principal_id text NOT NULL REFERENCES principals(id),
 revoked boolean NOT NULL DEFAULT false, expires_at timestamptz, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE agents (
 organization_id text NOT NULL REFERENCES organizations(id), id text NOT NULL,
 name text NOT NULL, instructions text NOT NULL, model text NOT NULL,
 tools jsonb NOT NULL DEFAULT '[]', budget_microusd bigint NOT NULL CHECK(budget_microusd>0),
 version integer NOT NULL DEFAULT 1 CHECK(version>0), published boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(organization_id,id,version)
);
CREATE TABLE tasks (
 organization_id text NOT NULL REFERENCES organizations(id), id text NOT NULL,
 owner_id text NOT NULL REFERENCES principals(id), agent_id text NOT NULL, agent_version integer NOT NULL,
 goal text NOT NULL, status text NOT NULL CHECK(status IN('queued','running','succeeded','failed','cancelled','waiting_input','waiting_approval','blocked')),
 budget_microusd bigint NOT NULL CHECK(budget_microusd>0), reserved_microusd bigint NOT NULL DEFAULT 0 CHECK(reserved_microusd>=0),
 spent_microusd bigint NOT NULL DEFAULT 0 CHECK(spent_microusd>=0), version integer NOT NULL DEFAULT 1,
 waiting_reason text NOT NULL DEFAULT '', result text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(organization_id,id), FOREIGN KEY(organization_id,agent_id,agent_version) REFERENCES agents(organization_id,id,version),
 FOREIGN KEY(organization_id,owner_id) REFERENCES memberships(organization_id,principal_id),
 CHECK(reserved_microusd+spent_microusd<=budget_microusd)
);
CREATE INDEX tasks_listing ON tasks(organization_id,created_at DESC,id DESC);
CREATE TABLE runs (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL, principal_id text NOT NULL,
 agent_id text NOT NULL, agent_version integer NOT NULL, status text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id),
 FOREIGN KEY(organization_id,agent_id,agent_version) REFERENCES agents(organization_id,id,version)
);
CREATE TABLE operations (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL, run_id text NOT NULL,
 status text NOT NULL CHECK(status IN('queued','leased','succeeded','failed','cancelled','blocked')),
 lease_owner text, lease_until timestamptz, fencing_token bigint NOT NULL DEFAULT 0,
 attempts integer NOT NULL DEFAULT 0, max_attempts integer NOT NULL DEFAULT 3 CHECK(max_attempts BETWEEN 1 AND 10),
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id), FOREIGN KEY(organization_id,run_id) REFERENCES runs(organization_id,id)
);
CREATE INDEX operations_dispatch ON operations(status,lease_until,created_at);
CREATE TABLE reservations (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL, operation_id text NOT NULL,
 amount_microusd bigint NOT NULL CHECK(amount_microusd>=0), actual_microusd bigint CHECK(actual_microusd>=0),
 status text NOT NULL CHECK(status IN('reserved','settled','unknown','released')),
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(organization_id,id), UNIQUE(organization_id,operation_id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id), FOREIGN KEY(organization_id,operation_id) REFERENCES operations(organization_id,id),
 CHECK(actual_microusd IS NULL OR actual_microusd<=amount_microusd)
);
CREATE TABLE events (
 sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, organization_id text NOT NULL, id text NOT NULL,
 task_id text NOT NULL, type text NOT NULL, data jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id), UNIQUE(organization_id,id)
);
CREATE INDEX events_task ON events(organization_id,task_id,sequence);
CREATE TABLE outbox (
 sequence bigint PRIMARY KEY REFERENCES events(sequence), organization_id text NOT NULL REFERENCES organizations(id),
 delivered_at timestamptz
);
CREATE TABLE artifacts (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL, name text NOT NULL,
 media_type text NOT NULL, content text NOT NULL CHECK(octet_length(content)<=262144), version integer NOT NULL DEFAULT 1,
 provenance jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(organization_id,id,version), FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id)
);
CREATE TABLE idempotency_keys (
 organization_id text NOT NULL REFERENCES organizations(id), principal_id text NOT NULL REFERENCES principals(id),
 scope text NOT NULL, key text NOT NULL, request_hash text NOT NULL, response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(organization_id,principal_id,scope,key)
);
CREATE TABLE actions (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL, operation_id text NOT NULL,
 intent_hash text NOT NULL, idempotency_key text NOT NULL, status text NOT NULL CHECK(status IN('pending','sent','confirmed','unknown','rejected')),
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(organization_id,id), UNIQUE(organization_id,idempotency_key),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id), FOREIGN KEY(organization_id,operation_id) REFERENCES operations(organization_id,id)
);

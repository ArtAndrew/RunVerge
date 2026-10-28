-- SPDX-License-Identifier: Elastic-2.0
-- Preserve accepted facts even when their projection cannot fit signed int64.
DROP TRIGGER evidence_immutable ON cost_evidence;
ALTER TABLE cost_evidence ADD COLUMN id text;
ALTER TABLE cost_evidence ADD COLUMN request_hash text;
ALTER TABLE cost_evidence ADD COLUMN final boolean NOT NULL DEFAULT true CHECK(final);
ALTER TABLE cost_evidence ADD COLUMN currency text NOT NULL DEFAULT 'USD' CHECK(currency='USD');
WITH numbered AS (
 SELECT organization_id,source,reference,'evi_legacy_'||row_number() OVER(ORDER BY organization_id,source,reference) AS id FROM cost_evidence
) UPDATE cost_evidence e SET id=n.id FROM numbered n WHERE e.organization_id=n.organization_id AND e.source=n.source AND e.reference=n.reference;
UPDATE cost_evidence e SET request_hash=COALESCE((
 SELECT l.event_hash FROM ledger_entries l WHERE l.organization_id=e.organization_id AND l.reservation_id=e.reservation_id
 AND l.event_key IN('settle:'||e.source||':'||e.reference,'adjustment:'||e.source||':'||e.reference) LIMIT 1
),'legacy-unavailable');
ALTER TABLE cost_evidence ALTER COLUMN id SET NOT NULL;
ALTER TABLE cost_evidence ALTER COLUMN request_hash SET NOT NULL;
ALTER TABLE cost_evidence ADD CONSTRAINT evidence_id_scope UNIQUE(organization_id,id);
ALTER TABLE cost_evidence ADD CONSTRAINT evidence_reservation_binding UNIQUE(organization_id,id,reservation_id);
CREATE TRIGGER evidence_immutable BEFORE UPDATE OR DELETE ON cost_evidence FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE cost_quarantines (
 organization_id text NOT NULL, id text NOT NULL, evidence_id text NOT NULL, reservation_id text NOT NULL,
 base_revision integer NOT NULL CHECK(base_revision>0), revision integer NOT NULL CHECK(revision=base_revision+1),
 base_status text NOT NULL CHECK(base_status IN('reserved','unknown','released','settled')),
 base_actual_microusd bigint CHECK(base_actual_microusd>=0),
 requested_kind text NOT NULL CHECK(requested_kind IN('settle','adjustment')),
 expected_revision integer, reason text NOT NULL CHECK(reason='projection_overflow'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id), UNIQUE(organization_id,evidence_id),
 UNIQUE(organization_id,id,reservation_id,evidence_id), UNIQUE(organization_id,id,reservation_id),
 FOREIGN KEY(organization_id,evidence_id,reservation_id) REFERENCES cost_evidence(organization_id,id,reservation_id),
 CHECK((requested_kind='settle' AND expected_revision IS NULL) OR (requested_kind='adjustment' AND expected_revision=base_revision))
);
CREATE INDEX quarantine_reservation ON cost_quarantines(organization_id,reservation_id,created_at,id);
CREATE TABLE cost_quarantine_resolutions (
 organization_id text NOT NULL,id text NOT NULL,quarantine_id text NOT NULL,reservation_id text NOT NULL,evidence_id text NOT NULL,
 resulting_revision integer NOT NULL CHECK(resulting_revision>0),created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id), UNIQUE(organization_id,quarantine_id),
 FOREIGN KEY(organization_id,quarantine_id,reservation_id,evidence_id) REFERENCES cost_quarantines(organization_id,id,reservation_id,evidence_id)
);
ALTER TABLE reservations ADD COLUMN pending_quarantine_id text;
ALTER TABLE reservations ADD CONSTRAINT pending_quarantine_binding FOREIGN KEY(organization_id,pending_quarantine_id,id) REFERENCES cost_quarantines(organization_id,id,reservation_id);
CREATE TRIGGER quarantine_immutable BEFORE UPDATE OR DELETE ON cost_quarantines FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER quarantine_resolution_immutable BEFORE UPDATE OR DELETE ON cost_quarantine_resolutions FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

-- SPDX-License-Identifier: Elastic-2.0
-- Preserve issuer/subject and every prior principal and session. The latest
-- append-only binding points to the newly authorized recovery principal.
CREATE TABLE recovery_oidc_rebindings (
 issuer text NOT NULL,
 subject text NOT NULL,
 generation bigint NOT NULL CHECK(generation>0),
 organization_id text NOT NULL REFERENCES organizations(id),
 previous_principal_id text NOT NULL REFERENCES principals(id),
 principal_id text NOT NULL REFERENCES principals(id),
 activation_id text NOT NULL CHECK(activation_id ~ '^activate_[a-f0-9]{32}$'),
 PRIMARY KEY(issuer,subject,generation),
 UNIQUE(issuer,subject,principal_id),
 FOREIGN KEY(issuer,subject) REFERENCES identity_mappings(issuer,subject),
 CHECK(previous_principal_id<>principal_id)
);
REVOKE ALL ON recovery_oidc_rebindings FROM PUBLIC;

CREATE FUNCTION recovery_oidc_principal(iss text, sub text, original text, enabled boolean) RETURNS text
 LANGUAGE sql STABLE SECURITY DEFINER AS $$
 SELECT COALESCE((SELECT r.principal_id FROM recovery_oidc_rebindings r WHERE r.issuer=iss AND r.subject=sub
  ORDER BY generation DESC LIMIT 1),CASE WHEN enabled THEN original END)
$$;

CREATE FUNCTION recovery_oidc_rebinding_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE prior bigint; principal text;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'SSO recovery binding is permanent' USING ERRCODE='55000'; END IF;
 PERFORM pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext(TG_TABLE_SCHEMA));
 SELECT generation,principal_id INTO prior,principal FROM recovery_oidc_rebindings
 WHERE issuer=NEW.issuer AND subject=NEW.subject ORDER BY generation DESC LIMIT 1;
 IF NOT FOUND THEN
  SELECT 0,principal_id INTO STRICT prior,principal FROM identity_mappings WHERE issuer=NEW.issuer AND subject=NEW.subject;
 END IF;
 IF SESSION_USER<>CURRENT_USER OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=SESSION_USER AND rolsuper)
 OR NOT EXISTS(SELECT 1 FROM recovery_work_admissions a JOIN runtime_recovery_control c
  ON c.singleton AND c.state='held' AND c.restore_id=a.restore_id AND c.installation_id=a.target_installation_id
  WHERE a.activation_id=NEW.activation_id AND NEW.organization_id=ANY(a.organizations)
   AND a.database_oid=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database()))
 OR NOT EXISTS(SELECT 1 FROM principals p JOIN memberships m ON m.principal_id=p.id
  JOIN organization_login_policies q ON q.organization_id=m.organization_id
  WHERE p.id=NEW.principal_id AND p.active AND p.kind='human' AND m.organization_id=NEW.organization_id AND m.active AND m.role='owner'
   AND (q.mode='mixed' OR (q.mode='oidc_required' AND q.oidc_issuer=NEW.issuer)))
 OR NEW.previous_principal_id IS DISTINCT FROM principal OR NEW.generation IS DISTINCT FROM prior+1
 OR NOT recovery_historical_principal(principal) OR recovery_historical_principal(NEW.principal_id)
 THEN RAISE EXCEPTION 'SSO recovery binding is outside the admitted scope' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER "000_recovery_history_row" BEFORE INSERT OR UPDATE OR DELETE ON recovery_oidc_rebindings
 FOR EACH ROW EXECUTE FUNCTION recovery_history_row_guard('issuer','subject','generation');
CREATE TRIGGER recovery_oidc_rebinding_insert BEFORE INSERT ON recovery_oidc_rebindings
 FOR EACH ROW EXECUTE FUNCTION recovery_oidc_rebinding_guard();
CREATE TRIGGER recovery_oidc_rebinding_permanent BEFORE UPDATE OR DELETE OR TRUNCATE ON recovery_oidc_rebindings
 FOR EACH STATEMENT EXECUTE FUNCTION recovery_oidc_rebinding_guard();
DO $$
BEGIN
 EXECUTE pg_catalog.format('ALTER FUNCTION %I.recovery_oidc_principal(text,text,text,boolean) SET search_path TO %I,pg_catalog,pg_temp',current_schema(),current_schema());
 EXECUTE pg_catalog.format('ALTER FUNCTION %I.recovery_oidc_rebinding_guard() SET search_path TO %I,pg_catalog,pg_temp',current_schema(),current_schema());
END $$;

-- SPDX-License-Identifier: Elastic-2.0
-- Forward repair: generic PostgreSQL JSON and containment operators share
-- precedence. Parenthesize both JSON operands so real child attenuation is
-- evaluated as containment, rather than applying -> to its boolean result.
CREATE OR REPLACE FUNCTION guard_new_configuration_grant() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE parent configuration_grants;item jsonb;valid boolean;name text;
BEGIN
 IF NEW.version<>1 OR NEW.revoked OR NEW.used_plans<>0 OR NEW.used_verifications<>0 OR NEW.allocated_charge_microusd<>0
 OR NEW.expires_at<=clock_timestamp() OR NOT configuration_json_strings(NEW.body,ARRAY['id','organization_id','issuer_id','subject_id','audience'],128)
 OR NEW.body->>'audience' IS DISTINCT FROM 'platform.configuration.v1'
 OR (NEW.body?'parent_grant_id' AND jsonb_typeof(NEW.body->'parent_grant_id') IS DISTINCT FROM 'string')
 OR COALESCE(NEW.body->>'parent_grant_id','') IS DISTINCT FROM COALESCE(NEW.parent_grant_id,'')
 OR NEW.body->'depth' IS DISTINCT FROM to_jsonb(NEW.depth) OR NEW.body->'max_plans' IS DISTINCT FROM to_jsonb(NEW.max_plans)
 OR NEW.body->'max_verifications' IS DISTINCT FROM to_jsonb(NEW.max_verifications)
 OR NEW.body->'max_allocated_charge_microusd' IS DISTINCT FROM to_jsonb(NEW.max_allocated_charge_microusd::text)
 OR jsonb_typeof(NEW.body->'actions') IS DISTINCT FROM 'array' OR jsonb_typeof(NEW.body->'scopes') IS DISTINCT FROM 'array' THEN
  RAISE EXCEPTION 'grant requires typed exact initial consent and limits' USING ERRCODE='23514';END IF;
 IF jsonb_array_length(NEW.body->'actions') NOT BETWEEN 1 AND 6 OR jsonb_array_length(NEW.body->'scopes') NOT BETWEEN 1 AND 16 THEN
  RAISE EXCEPTION 'grant action and scope sets must be bounded' USING ERRCODE='23514';END IF;
 FOR item IN SELECT value FROM jsonb_array_elements(NEW.body->'actions') LOOP
  IF jsonb_typeof(item) IS DISTINCT FROM 'string' OR item#>>'{}' NOT IN('inspect','plan','validate','apply','verify','delegate') THEN
   RAISE EXCEPTION 'grant contains unknown action' USING ERRCODE='23514';END IF;
 END LOOP;
 IF (SELECT count(*)<>count(DISTINCT value) FROM jsonb_array_elements(NEW.body->'actions'))
 OR (SELECT count(*)<>count(DISTINCT value->>'destination_id') FROM jsonb_array_elements(NEW.body->'scopes')) THEN
  RAISE EXCEPTION 'grant cannot duplicate actions or destination versions' USING ERRCODE='23514';END IF;
 FOR item IN SELECT value FROM jsonb_array_elements(NEW.body->'scopes') LOOP
  IF NOT configuration_scope_is_valid(NEW.organization_id,item,true) THEN
   RAISE EXCEPTION 'grant scope must be an exact active destination and purpose' USING ERRCODE='23514';END IF;
 END LOOP;
 SELECT EXISTS(SELECT 1 FROM memberships im JOIN principals ip ON ip.id=im.principal_id
  JOIN memberships sm ON sm.organization_id=im.organization_id AND sm.principal_id=NEW.subject_id JOIN principals sp ON sp.id=sm.principal_id
  JOIN organizations o ON o.id=im.organization_id JOIN organization_login_policies policy ON policy.organization_id=o.id
  WHERE im.organization_id=NEW.organization_id AND im.principal_id=NEW.issuer_id AND im.active AND ip.active AND sm.active AND sp.active AND o.active
  AND im.role<>'viewer' AND sm.role<>'viewer' AND ip.kind IN('human','service') AND sp.kind IN('human','service')
  AND (ip.kind='human' OR (policy.service_access_enabled AND ip.service_organization_id=o.id))
  AND (sp.kind='human' OR (policy.service_access_enabled AND sp.service_organization_id=o.id))
  AND (NEW.parent_grant_id IS NOT NULL OR (ip.kind='human' AND im.role='owner'))) INTO valid;
 IF NOT valid THEN RAISE EXCEPTION 'grant requires current local issuer and subject authority' USING ERRCODE='23514';END IF;
 IF NEW.parent_grant_id IS NOT NULL THEN
  SELECT * INTO parent FROM configuration_grants WHERE organization_id=NEW.organization_id AND id=NEW.parent_grant_id;
  IF NOT FOUND OR NEW.id=NEW.parent_grant_id OR NEW.depth<>parent.depth+1 OR NEW.depth>4
  OR NOT configuration_grant_is_live(NEW.organization_id,parent.id,NEW.issuer_id,'delegate')
  OR NOT((parent.body->'actions') @> (NEW.body->'actions')) OR NOT((parent.body->'scopes') @> (NEW.body->'scopes'))
  OR NEW.expires_at>parent.expires_at OR NEW.max_plans>parent.max_plans OR NEW.max_verifications>parent.max_verifications
  OR NEW.max_allocated_charge_microusd>parent.max_allocated_charge_microusd THEN
   RAISE EXCEPTION 'child grant must attenuate the actual live parent authority' USING ERRCODE='23514';END IF;
 END IF;
 NEW.issuer_authority_epoch=current_member_epoch(NEW.organization_id,NEW.issuer_id);
 NEW.subject_authority_epoch=current_member_epoch(NEW.organization_id,NEW.subject_id);
 NEW.issuer_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.issuer_id);
 NEW.subject_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.subject_id);
 RETURN NEW;
END $$;

# SPDX-License-Identifier: Elastic-2.0
"""Actual local security SDK flow; synthetic service identities, no inference."""
from __future__ import annotations
import json
from pathlib import Path
import sys
import time
sys.path.insert(0,str(Path(__file__).resolve().parents[2]/'packages/python-sdk/src'))
from agent_platform import APIError,Client
from issue_service_fixture_token import issue_service_token

def expect_error(call,code,status):
    try:call()
    except APIError as error:
        assert error.code==code and error.status_code==status
        return
    raise AssertionError('Expected explicit current authorization rejection')

def exercise_security(client:Client,output:Path,key:str,foreign_org:str|None=None)->dict:
    me=client.get_current_identity();assert me['role']=='owner' and me.get('principal_kind')=='human'
    original=client.get_login_policy();assert original['mode']=='mixed' and original['service_access_enabled'] and not original['can_require_oidc']
    assert 'oidc_not_configured' in original['readiness_reasons']
    temporary=original['max_service_key_ttl_seconds']-1 if original['max_service_key_ttl_seconds']>1 else 2
    changed=client.update_login_policy(expected_version=original['version'],mode=original['mode'],oidc_issuer=original['oidc_issuer'],service_access_enabled=original['service_access_enabled'],max_service_key_ttl_seconds=temporary,reason='Synthetic SDK TTL-only policy check',idempotency_key=key+':policy')
    assert changed['max_service_key_ttl_seconds']==temporary and changed['version']>original['version']
    restored=client.update_login_policy(expected_version=changed['version'],mode=original['mode'],oidc_issuer=original['oidc_issuer'],service_access_enabled=original['service_access_enabled'],max_service_key_ttl_seconds=original['max_service_key_ttl_seconds'],reason='Restore prior TTL; application access unchanged',idempotency_key=key+':restore-policy')
    expect_error(lambda:client.update_login_policy(expected_version=restored['version'],mode='oidc_required',oidc_issuer='https://unconfigured.example',service_access_enabled=True,max_service_key_ttl_seconds=restored['max_service_key_ttl_seconds'],reason='Must refuse unconfigured browser-owner policy',idempotency_key=key+':deny-oidc'),'forbidden',403)
    created=client.create_service_account('SDK Python '+key[-8:],'member',reason='Synthetic SDK service identity',idempotency_key=key+':create');principal=created['principal_id'];assert created['principal_kind']=='service' and 'token' not in created
    assert client.create_service_account(created['display_name'],'member',reason='Synthetic SDK service identity',idempotency_key=key+':create')==created
    expect_error(lambda:client.create_service_account(created['display_name'],'viewer',reason='Changed create payload',idempotency_key=key+':create'),'conflict',409)
    assert client.get_service_account(principal)==created
    found=[];cursor=None
    for _ in range(300):
        page=client.list_service_accounts(cursor=cursor,limit=1);found.extend(v['principal_id'] for v in page.data);cursor=page.next_cursor
        if not cursor:break
    assert cursor is None and found.count(principal)==1 and len(found)==len(set(found))
    file=output/'python-viewer-key.json';issue_service_token(client.organization_id,principal,file,'viewer',min(60,restored['max_service_key_ttl_seconds']));credentials=json.loads(file.read_text());metadata=credentials['key'];assert metadata['role_ceiling']=='viewer'
    low=Client(client.base_url,token=credentials['token'],organization_id=client.organization_id)
    observed=low.get_current_identity();assert observed['principal_kind']=='service' and observed['role']=='viewer' and 'identity.manage' not in observed['permissions'] and 'tasks.review_own' not in observed['permissions']
    expect_error(low.get_login_policy,'forbidden',403)
    expect_error(lambda:low.create_agent('Must reject viewer key','No execution',model='fixture:echo',budget_microusd=100,idempotency_key=key+':deny-viewer'),'forbidden',403)
    assert client.get_service_key(principal,metadata['id'])==metadata
    assert any(v['id']==metadata['id'] for v in client.list_service_keys(principal,limit=1).data)
    cross=Client(client.base_url,token=credentials['token'],organization_id=foreign_org or 'org_unrelated_security_fixture')
    expect_error(cross.get_current_identity,'unauthenticated',401)
    expect_error(lambda:client.update_service_account(principal,expected_version=created['version'],display_name=created['display_name'],role='member',active=True,reason='No-op rejected',idempotency_key=key+':noop'),'invalid_transition',409)
    renamed=client.update_service_account(principal,expected_version=created['version'],display_name=created['display_name']+' renamed',role='member',active=True,reason='Name-only update',idempotency_key=key+':rename')
    assert low.get_current_identity()['role']=='viewer'
    expect_error(lambda:client.update_service_account(principal,expected_version=created['version'],display_name='Stale',role='viewer',active=True,reason='Stale version',idempotency_key=key+':stale'),'conflict',409)
    revoked=client.revoke_service_key(principal,metadata['id'],expected_version=metadata['version'],reason='Credential-only revoke',idempotency_key=key+':revoke');assert revoked['revoked']
    expect_error(low.get_current_identity,'unauthenticated',401)
    expiration=output/'python-expiring-key.json';issue_service_token(client.organization_id,principal,expiration,'viewer',1);expiring=json.loads(expiration.read_text());short=Client(client.base_url,token=expiring['token'],organization_id=client.organization_id)
    time.sleep(1.2);expect_error(short.get_current_identity,'unauthenticated',401)
    disable=client.update_service_account(principal,expected_version=renamed['version'],display_name=renamed['display_name'],role='member',active=False,reason='Final synthetic service deactivation',idempotency_key=key+':disable');assert disable['active'] is False
    events=client.list_security_audit(principal_id=principal,limit=100).data;assert events and all(v['target_principal_id']==principal for v in events);assert any(v['action']=='service_key.issued' for v in events) and any(v['action']=='service_key.revoked' for v in events)
    current=client.get_login_policy();assert all(current[k]==original[k] for k in ('mode','oidc_issuer','service_access_enabled','max_service_key_ttl_seconds'))
    return dict(sdk='python',organization_id=created['organization_id'],principal_id=principal,final_active=disable['active'],final_version=disable['version'],key_id=metadata['id'],key_revoked=True,expired_key_id=expiring['key']['id'],audit_entries=len(events),existing_foreign_organization_tested=bool(foreign_org),checks=['all10routes','TTL-only policy change+restore','unconfigured SSO rejected','durable create idempotency and changed-payload conflict','exact CAS and no-op','viewer key ceiling and service principal permissions','cross-org selector rejected','metadata-only key reads','credential-only revoke','natural one-second key expiry','final service deactivation','filtered audit'],task_or_inference_requested=False)

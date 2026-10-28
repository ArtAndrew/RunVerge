# SPDX-License-Identifier: Elastic-2.0
"""Actual local IAM SDK flow; no inference or task execution is requested."""
from __future__ import annotations
import json
from pathlib import Path
import sys
sys.path.insert(0,str(Path(__file__).resolve().parents[2]/'packages/python-sdk/src'))
from agent_platform import APIError,Client
from issue_membership_fixture_token import issue_token

def expect_error(call,code,status):
    try:call()
    except APIError as error:
        assert error.code==code and error.status_code==status
        return
    raise AssertionError('Expected explicit server policy rejection')

def exercise_members(client:Client,output:Path,key:str)->dict:
    me=client.get_current_identity();caps=client.membership_capabilities();assert me['role']=='owner' and caps['can_create_members'] and caps['self_management_allowed'] is False
    created=client.create_member('SDK Python '+key[-8:],'viewer',reason='Synthetic local SDK membership provisioning',idempotency_key=key+':create')
    assert 'token' not in created and 'email' not in created and created['principal_id']!=me['principal_id']
    replay=client.create_member(created['display_name'],'viewer',reason='Synthetic local SDK membership provisioning',idempotency_key=key+':create');assert replay==created
    expect_error(lambda:client.create_member(created['display_name'],'viewer',reason='Changed payload',idempotency_key=key+':create'),'conflict',409)
    principal=created['principal_id'];observed=[];cursor=None
    for _ in range(300):
        page=client.list_members(cursor=cursor,limit=1);observed += [v['principal_id'] for v in page.data];cursor=page.next_cursor
        if not cursor:break
    assert cursor is None and observed.count(principal)==1 and len(observed)==len(set(observed))
    assert client.get_member(principal)['version']==created['version']
    def credential(label):
        file=output/(label+'.json');issue_token(client.organization_id,principal,file);data=json.loads(file.read_text());assert data['organization_id']==client.organization_id
        return Client(client.base_url,token=data['token'],organization_id=data['organization_id'])
    viewer=credential('python-viewer-credential');assert viewer.get_current_identity()['role']=='viewer';assert viewer.membership_capabilities()['can_create_members'] is False
    expect_error(lambda:viewer.list_members(),'forbidden',403)
    own=client.get_member(me['principal_id'])
    expect_error(lambda:client.update_member(me['principal_id'],expected_version=own['version'],display_name=own['display_name']+' attempted',role=own['role'],active=True,reason='Self forbidden',idempotency_key=key+':self-update'),'invalid_transition',409)
    expect_error(lambda:client.revoke_member_credentials(me['principal_id'],expected_version=own['version'],reason='Self forbidden',idempotency_key=key+':self-revoke'),'invalid_transition',409)
    def update(current,role,active,suffix):return client.update_member(principal,expected_version=current['version'],display_name=current['display_name'],role=role,active=active,reason='Synthetic access transition '+suffix,idempotency_key=key+':'+suffix)
    member=update(created,'admin',True,'promote');assert viewer.get_current_identity()['role']=='admin' and viewer.membership_capabilities()['can_create_members'] is True
    expect_error(lambda:viewer.update_member(own['principal_id'],expected_version=own['version'],display_name=own['display_name']+' attempted',role='owner',active=True,reason='Admin cannot manage owner',idempotency_key=key+':admin-owner'),'forbidden',403)
    expect_error(lambda:update(created,'member',True,'stale'),'conflict',409)
    expect_error(lambda:update(member,'admin',True,'noop'),'invalid_transition',409)
    member=client.revoke_member_credentials(principal,expected_version=member['version'],reason='Invalidate old org logins; grant authority unchanged',idempotency_key=key+':revoke');assert member['active'] and member['role']=='admin' and member.get('credentials_revoked_at')
    expect_error(lambda:viewer.get_current_identity(),'unauthenticated',401)
    fresh=credential('python-admin-fresh');assert fresh.get_current_identity()['role']=='admin'
    member=update(member,'admin',False,'disable');expect_error(lambda:fresh.get_current_identity(),'unauthenticated',401)
    member=update(member,'admin',True,'restore');expect_error(lambda:fresh.get_current_identity(),'unauthenticated',401);expect_error(lambda:viewer.get_current_identity(),'unauthenticated',401)
    restored=credential('python-admin-restored');assert restored.get_current_identity()['role']=='admin'
    member=update(member,'viewer',True,'demote');assert restored.get_current_identity()['role']=='viewer'
    member=update(member,'viewer',False,'final-disable');expect_error(lambda:restored.get_current_identity(),'unauthenticated',401)
    audit=[];cursor=None
    for _ in range(20):
        page=client.list_membership_audit(principal_id=principal,cursor=cursor,limit=1);audit += page.data;cursor=page.next_cursor
        if not cursor:break
    assert cursor is None and len(audit)==7 and len({entry['id'] for entry in audit})==7
    assert all(entry['organization_id']==client.organization_id and entry['target_principal_id']==principal for entry in audit)
    return {'sdk':'python','principal_id':principal,'organization_id':client.organization_id,'final_role':member['role'],'final_active':member['active'],'final_version':member['version'],'audit_entries':len(audit),'checks':['7 actual public routes','opaque cursor pages','durable idempotent create, changed payload conflict','viewer denied, role edit keeps login with new current role','self forbidden, admin cannot manage owner','stale CAS and no-op update rejected','org credential cutoff invalidates old token','disable/restore never resurrect old token','fresh trusted issuance works; final disable invalidates it'],'scope':'synthetic provisioned local user; no email/SSO linking, inference, task grant or real provider billing claim'}

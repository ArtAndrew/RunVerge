# SPDX-License-Identifier: Apache-2.0
"""Actual isolated TLS API event flow. Host controls are private fixture files."""
from __future__ import annotations
import json
import os
from pathlib import Path
import stat
import sys
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen
from uuid import uuid4

ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'packages/python-sdk/src'))
from agent_platform import Client, APIError


def private_json(path: Path) -> dict:
    descriptor=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
    with os.fdopen(descriptor,'r') as source:
        facts=os.fstat(source.fileno())
        assert stat.S_ISREG(facts.st_mode) and stat.S_IMODE(facts.st_mode)==0o600 and facts.st_size<=65536
        return json.load(source)


def host_command(metadata: dict, command: str, task_id: str, **extra) -> dict:
    identifier=uuid4().hex
    path=Path(metadata['command_file']);temporary=path.with_name(path.name+'.'+identifier+'.tmp')
    descriptor=os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(descriptor,'w') as target:
        json.dump(dict(id=identifier,command=command,task_id=task_id,**extra),target);target.flush();os.fsync(target.fileno())
    os.replace(temporary,path)
    deadline=time.monotonic()+12
    while time.monotonic()<deadline:
        try:
            reply=private_json(Path(metadata['reply_file']))
            if reply.get('id')==identifier:
                assert reply.get('ok') is True,reply.get('error_code','host_control_failed')
                return reply
        except FileNotFoundError:pass
        time.sleep(.05)
    raise TimeoutError('Private host command did not complete')


def run(metadata: dict) -> dict:
    client=Client(metadata['base_url'],token=metadata['token'],organization_id=metadata['organization_id'],timeout=10)
    org=metadata['organization_id'];key='events-python-'+uuid4().hex
    task=client.create_task('Synthetic event SDK conformance',metadata['agent_id'],10000,idempotency_key=key)
    identifier=task['id'];initial=client.get_task_event_snapshot(identifier)
    assert initial['task']['status']=='queued' and initial['events']
    assert all(isinstance(event['sequence'],str) and event['payload_version']==1 for event in initial['events'])
    history=client.events(identifier,after='0',limit=100).data
    assert history and client.task_event_feed(identifier,cursor=initial['replay_cursor'])['events']
    with client.stream_task_events(identifier,cursor=initial['cursor']) as frames:
        first=next(frames);assert first['kind']=='batch'
        host_command(metadata,'cancel',identifier)
        for frame in frames:
            assert frame['kind']=='batch'
            if any(event['type']=='task.cancelled' for event in frame['batch']['events']):
                processed=frame['batch']['cursor'];break
        else:raise AssertionError('Committed cancel event missing from stream')
    with client.stream_task_events(identifier,cursor=processed) as frames:
        replay=next(frames);assert replay['kind']=='batch' and not replay['batch']['events']
    host_command(metadata,'advance',identifier)
    reset=client.task_event_feed(identifier,cursor=initial['cursor'])
    assert reset['reset_required'] and reset['events']==[] and reset['cursor']=='' and reset['window']['version']>=2
    with client.stream_task_events(identifier,cursor=initial['cursor']) as frames:
        assert next(frames)['kind']=='reset_required'
    restored=client.get_task_event_snapshot(identifier)
    assert restored['window']['version']>=2 and restored['task']['status']=='cancelled'
    assert not client.task_event_feed(identifier,cursor=restored['cursor'])['reset_required']
    other=client.create_task('Separate synthetic cursor scope',metadata['agent_id'],10000,idempotency_key=key+'-other')
    try:client.task_event_feed(other['id'],cursor=initial['cursor'])
    except APIError as error:assert error.status_code in (400,404)
    else:raise AssertionError('Cross-task cursor accepted')
    bad=Client(metadata['base_url'],token='synthetic-invalid-token',organization_id=org,timeout=5)
    try:
        with bad.stream_task_events(identifier) as frames:next(frames)
    except APIError as error:assert error.status_code==401
    else:raise AssertionError('Unauthenticated subscription accepted')
    before=len(client.list_tasks(limit=100).data)
    request=Request(metadata['base_url']+'/api/v1/tasks',data=json.dumps(dict(goal='Must not be admitted',agent_id=metadata['agent_id'],budget_microusd=10000)).encode(),headers={'Authorization':'Bearer '+metadata['token'],'X-Organization-ID':org,'X-Platform-API-Version':'0.10.0','Content-Type':'application/json','Idempotency-Key':key+'-incompatible'},method='POST')
    try:urlopen(request,timeout=5)
    except HTTPError as error:
        with error:assert error.code==409 and json.load(error)['error']['code']=='incompatible_client'
    else:raise AssertionError('Incompatible command accepted')
    assert len(client.list_tasks(limit=100).data)==before
    return dict(sdk='python',passed=True,task_id=identifier,other_task_id=other['id'],window_revision=restored['window']['version'],checks=['CA-verified actual TLS','snapshot/feed/history','typed payloads','single connection and committed cancel','explicit reconnect','actual expired floor reset','fresh snapshot revision2+','cross-task cursor','401 subscription','incompatible POST not admitted'],paid_calls=0)


if __name__=='__main__':
    metadata=private_json(Path(sys.argv[1]));report=run(metadata)
    Path(sys.argv[2]).write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps({'sdk':'python','passed':True,'paid_calls':0}))

# SPDX-License-Identifier: Elastic-2.0
"""Actual CLI handoff against disposable PostgreSQL/API/worker; never main."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import time
import uuid


def private_json(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'r') as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.getuid() or not 1 <= info.st_size <= 32768:
            raise ValueError('Unsafe private handoff metadata')
        return json.loads(source.read(32769))


def execute(binary: Path, metadata, scratch: Path):
    cases = []
    base = dict(os.environ)
    base.update(DATABASE_URL='intentionally-unusable-database', PLATFORM_URL=metadata['base_url'], PLATFORM_ORGANIZATION_ID=metadata['organization_id'])
    key = lambda: 'cli-config-' + uuid.uuid4().hex
    with tempfile.TemporaryDirectory(prefix='configuration-cli-', dir=scratch) as temporary:
        private = Path(temporary)

        def call(identity, group, action=None, *, target=None, payload=None, durable_key=None, status=0, extra=()):
            args = [str(binary), 'config', group]
            if action: args.append(action)
            if target: args += ['--id', target]
            if payload is not None:
                path = private / (uuid.uuid4().hex + '.json')
                path.write_text(json.dumps(payload)); path.chmod(0o600)
                args += ['--file', str(path), '--key', durable_key or key()]
            args += [*extra, '--json']
            env = dict(base); env['PLATFORM_TOKEN'] = metadata['token'] if identity == 'owner' else metadata['service_token']
            result = subprocess.run(args, env=env, text=True, capture_output=True, timeout=30)
            response = json.loads(result.stdout)
            okay = result.returncode == 0 if status == 0 else result.returncode != 0 and response.get('error', {}).get('status') == status
            cases.append(dict(command='config ' + group + (' ' + action if action else ''), identity=identity, passed=okay, expected_http_error=status or None))
            if not okay: raise AssertionError('CLI command failed: ' + cases[-1]['command'])
            return response.get('data') if status == 0 else response['error']

        initial = private_json(metadata['observations_file'])
        destinations = call('owner', 'destinations', 'list')
        destination = next(d for d in destinations if d['id'] == metadata['mcp_destination_id'])
        grant = call('owner', 'grants', 'create', payload=dict(subject_id=metadata['service_principal_id'], actions=['inspect','plan','validate','apply','verify'], destinations=[dict(id=destination['id'],version=destination['version'])], ttl_seconds=300, max_plans=2, max_verifications=1, max_allocated_charge_microusd='0', reason='Explicit isolated CLI delegation'))
        catalog = call('service', 'catalog')
        resources = call('service', 'resources', 'list')
        version = max((r['version'] for r in resources if r['kind'] == 'mcp' and r['id'] == destination['target_id']), default=0)
        operation = dict(id='cli_mcp', destination_id=destination['id'], destination_version=destination['version'], expected_resource_version=version, active=True)
        plan = call('service', 'plans', 'create', payload=dict(grant_id=grant['id'], base_revision=catalog['revision'], ttl_seconds=300, operations=[operation], reason='CLI exact static configuration'))
        exact = call('service', 'plans', 'get', target=plan['id'])
        assert exact['digest'] == plan['digest']
        snapshot = dict(expected_version=plan['version'], plan_digest=plan['digest'])
        validation = call('service', 'plans', 'validate', target=plan['id'], payload=snapshot)
        assert validation['valid']
        decision = dict(**snapshot, validation_id=validation['id'], reason='Exact CLI owner decision')
        call('owner', 'plans', 'approve', target=plan['id'], payload=decision)
        applied = call('service', 'plans', 'apply', target=plan['id'], payload=decision)
        assert applied['status'] == 'applied' and applied['version'] == 2
        assert private_json(metadata['observations_file']) == initial, 'Static commands performed external I/O'
        verification = dict(expected_plan_version=applied['version'], plan_digest=applied['digest'], operation_id='cli_mcp', max_charge_microusd='0', acknowledge_cost=True, reason='One explicitly acknowledged CLI MCP check')
        occurrence_key = key()
        receipt = call('service', 'plans', 'verify', target=plan['id'], payload=verification, durable_key=occurrence_key)
        deadline = time.monotonic() + 25
        while receipt['status'] in ('queued','sent') and time.monotonic() < deadline:
            time.sleep(0.1)
            receipt = call('service', 'verifications', 'get', target=receipt['id'])
        assert receipt['status'] == 'response_received' and receipt['cost_status'] == 'unmetered' and receipt['tool_count'] == 1
        replay = call('service', 'plans', 'verify', target=plan['id'], payload=verification, durable_key=occurrence_key)
        assert replay['id'] == receipt['id'], 'Same key lost occurrence identity'
        call('service', 'plans', 'verify', target=plan['id'], payload=verification, status=409)
        listed = call('service', 'verifications', 'list', extra=('--plan-id',plan['id']))
        assert len(listed) == 1 and listed[0]['id'] == receipt['id']
        usage = call('owner', 'grants', 'get', target=grant['id'])
        assert usage['used_plans'] == 1 and usage['used_verifications'] == 1 and usage['allocated_charge_microusd'] == '0'
        call('owner', 'grants', 'revoke', target=grant['id'], payload=dict(expected_version=usage['version'],reason='Finish CLI delegation explicitly'))
        assert call('service', 'plans', 'get', target=plan['id'])['id'] == plan['id']
        call('service', 'plans', 'create', payload=dict(grant_id=grant['id'],base_revision=call('owner','catalog')['revision'],ttl_seconds=60,operations=[operation],reason='Revoked grant must fail'),status=403)
        call('owner','audit','list')
        final = private_json(metadata['observations_file'])
        fields = ('model_calls','mcp_discover_calls','mcp_list_calls','tools_call_calls','authentication_failures')
        delta = {field:final[field]-initial[field] for field in fields}
        assert delta == dict(model_calls=0,mcp_discover_calls=1,mcp_list_calls=1,tools_call_calls=0,authentication_failures=0)
        return dict(passed=True,scope='actual isolated API/worker/TLS MCP, CLI has unusable DATABASE_URL; not main or a paid-provider proof',cases=cases,physical_request_delta=delta,no_hidden_retry=True,plan_id=plan['id'],verification_id=receipt['id'])


if __name__ == '__main__':
    parser=argparse.ArgumentParser();parser.add_argument('--binary',type=Path,required=True);parser.add_argument('--metadata',required=True);parser.add_argument('--scratch',type=Path,required=True);parser.add_argument('--report',type=Path,required=True);args=parser.parse_args()
    report=execute(args.binary.resolve(),private_json(args.metadata),args.scratch)
    with args.report.open('x') as output:json.dump(report,output,indent=2);output.write('\n')
    print(json.dumps(dict(passed=True,cases=len(report['cases']),report=str(args.report))))

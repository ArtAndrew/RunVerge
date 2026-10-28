# SPDX-License-Identifier: Apache-2.0
"""Explicit isolated-fixture configuration flow. Never retries a mutation."""
import argparse
import json
import os
from pathlib import Path
import stat
import time
import uuid
from agent_platform import Client, APIError, API_VERSION


def private_json(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, 'rb') as file:
        info = os.fstat(file.fileno())
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != os.getuid() or not 1 <= info.st_size <= 32768:
            raise ValueError('Unsafe private fixture metadata')
        return json.loads(file.read(32769))


def execute(metadata):
    owner = Client(metadata['base_url'], organization_id=metadata['organization_id'], token=metadata['token'])
    agent = Client(metadata['base_url'], organization_id=metadata['organization_id'], token=metadata['service_token'])
    key = lambda: 'python-config-' + uuid.uuid4().hex
    initial = private_json(metadata['observations_file'])
    catalog = owner.configuration_catalog()
    ids = {metadata['model_destination_id'], metadata['mcp_destination_id']}
    destinations = [d for d in owner.configuration_destinations().data if d['id'] in ids]
    assert len(destinations) == 2
    grant = owner.create_configuration_grant(dict(subject_id=metadata['service_principal_id'], actions=['inspect', 'plan', 'validate', 'apply', 'verify'], destinations=[dict(id=d['id'], version=d['version']) for d in destinations], ttl_seconds=600, max_plans=2, max_verifications=2, max_allocated_charge_microusd='10000', reason='Explicit isolated Python configuration flow'), idempotency_key=key())
    resources = agent.configuration_resources().data
    inputs = [dict(id='python_' + d['kind'], destination_id=d['id'], destination_version=d['version'], expected_resource_version=max((r['version'] for r in resources if r['kind'] == d['kind'] and r['id'] == d['target_id']), default=0), active=True) for d in destinations]
    plan = agent.create_configuration_plan(dict(grant_id=grant['id'], base_revision=catalog['revision'], ttl_seconds=600, operations=inputs, reason='Exact model and MCP slots, no external work on apply'), idempotency_key=key())
    validation = agent.validate_configuration_plan(plan['id'], dict(expected_version=plan['version'], plan_digest=plan['digest']), idempotency_key=key())
    assert validation['valid'] and validation['decision']['outcome'] in ('allow', 'requires_approval')
    approval = owner.approve_configuration_plan(plan['id'], dict(expected_version=plan['version'], plan_digest=plan['digest'], validation_id=validation['id'], reason='Owner approved exact Python plan'), idempotency_key=key())
    assert approval['plan_digest'] == plan['digest']
    applied = agent.apply_configuration_plan(plan['id'], dict(expected_version=plan['version'], plan_digest=plan['digest'], validation_id=validation['id'], reason='Apply exact statically checked settings'), idempotency_key=key())
    assert applied['status'] == 'applied' and applied['version'] == 2 and applied['receipt_id']
    assert private_json(metadata['observations_file']) == initial, 'Static configuration unexpectedly contacted a peer'
    receipts = []
    for operation in applied['operations']:
        cap = '10000' if operation['scope']['kind'] == 'model' else '0'
        request = dict(expected_plan_version=applied['version'], plan_digest=applied['digest'], operation_id=operation['id'], max_charge_microusd=cap, acknowledge_cost=True, reason='One explicit synthetic connection check')
        receipt = agent.start_configuration_verification(plan['id'], request, idempotency_key=key())
        deadline = time.monotonic() + 25
        while receipt['status'] in ('queued', 'sent') and time.monotonic() < deadline:
            time.sleep(0.1); receipt = agent.configuration_verification(receipt['id'])
        assert receipt['status'] == 'response_received', 'Actual connection check did not receive a safe response'
        if receipt['kind'] == 'model':
            task = agent.get_task(receipt['task_id'])
            deadline = time.monotonic() + 25
            while task['status'] in ('queued', 'running') and time.monotonic() < deadline:
                time.sleep(0.1); task = agent.get_task(receipt['task_id'])
            assert task['status'] == 'waiting_input' and task.get('candidate_id') and not task.get('accepted_decision_id'), 'Model probe must leave an unaccepted candidate'
            assert any(c['id'] == task['candidate_id'] for c in agent.candidates(task['id']).data)
            assert not any(d['decision'] == 'accept' for d in agent.decisions(task['id']).data)
            cost = agent.costs(task['id'])
            assert int(cost['reserved_microusd']) > 0, 'Estimated provider usage must not settle unknown billing'
            assert receipt['response_digest'] and receipt['cost_status'] == 'unknown'
        else:
            assert receipt['cost_status'] == 'unmetered' and receipt['tool_count'] == 1 and receipt['catalog_digest']
        try:
            agent.start_configuration_verification(plan['id'], request, idempotency_key=key())
            raise AssertionError('A second verification was admitted')
        except APIError as error:
            assert error.status_code == 409
        receipts.append({field: receipt[field] for field in ('id', 'kind', 'status', 'cost_status', 'max_charge_microusd')})
    usage = owner.configuration_grant(grant['id'])
    assert usage['used_plans'] == 1 and usage['used_verifications'] == 2 and usage['allocated_charge_microusd'] == '10000'
    revoked = owner.revoke_configuration_grant(grant['id'], dict(expected_version=usage['version'], reason='Explicitly finish the isolated delegation'), idempotency_key=key())
    assert revoked['revoked'] and revoked['allocated_charge_microusd'] == '10000'
    assert agent.configuration_plan(plan['id'])['id'] == plan['id'], 'Own history disappeared after revoke'
    try:
        agent.create_configuration_plan(dict(grant_id=grant['id'], base_revision=owner.configuration_catalog()['revision'], ttl_seconds=600, operations=inputs, reason='Must fail after revoke'), idempotency_key=key())
        raise AssertionError('Revoked grant admitted a new plan')
    except APIError as error:
        assert error.status_code == 403
    final = private_json(metadata['observations_file'])
    delta = {field: final[field] - initial[field] for field in ('model_calls', 'mcp_discover_calls', 'mcp_list_calls', 'tools_call_calls', 'authentication_failures')}
    assert delta == dict(model_calls=1, mcp_discover_calls=1, mcp_list_calls=1, tools_call_calls=0, authentication_failures=0)
    return dict(passed=True, contract_version=API_VERSION, scope='actual isolated PostgreSQL/API/worker + local synthetic model/TLS MCP; not main/paid-provider acceptance', plan_id=plan['id'], grant_id=grant['id'], receipts=receipts, physical_request_delta=delta, no_hidden_POST_retry=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(); parser.add_argument('--metadata', required=True); parser.add_argument('--report', required=True); args = parser.parse_args()
    report = execute(private_json(args.metadata))
    with Path(args.report).open('x') as output: json.dump(report, output, indent=2); output.write('\n')
    print(json.dumps({'passed': True, 'plan_id': report['plan_id'], 'report': args.report}))

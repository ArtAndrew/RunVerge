#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Verify an installed local-model draft plus an explicitly approved MCP read.

The MCP peer uses the upstream SDK, not RunVerge's action/result envelope. This
is a scripted owner action on a real model draft, not model-chosen tool use or
independent human usability. All services/DB are owned by the native verifier.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import time
import uuid

from verify_local_model import local_probe
from verify_native_bundle import verify

DOCUMENT = ('Pilot policy example (synthetic, not an organization policy).\n'
            'Access: only an authorized team may read pilot documents.\n'
            'Resources: limit concurrent local jobs and execution time.\n'
            'Human review: approve a specific result before operational use.\n')


def read_probe(server_binary):
    server_binary = server_binary.resolve(strict=True)
    if not server_binary.is_file() or not os.access(server_binary, os.X_OK):
        raise RuntimeError('An explicit prebuilt MCP test server is required')

    snapshot = {}

    def exercise(context, task, candidate):
        private, request = context['private'], context['request']
        ready, document = private / 'mcp-ready.json', private / 'pilot-policy.txt'
        document.write_text(DOCUMENT, encoding='utf-8'); document.chmod(0o600)
        child = context['process']('standard-mcp-reader', [server_binary, '--ready', ready, '--document', document])
        deadline = time.monotonic() + 15
        while True:
            if child.poll() is not None or time.monotonic() >= deadline:
                raise RuntimeError('Owned MCP peer readiness failed')
            try:
                with ready.open('rb') as stream:
                    raw = stream.read(16385)
                if len(raw) > 16384:
                    raise RuntimeError('Owned MCP peer readiness exceeds limit')
                peer = json.loads(raw)
                if not isinstance(peer, dict) or set(peer) != {'endpoint', 'trusted_ca_pem'}:
                    raise RuntimeError('Invalid owned MCP peer readiness')
                break
            except (FileNotFoundError, json.JSONDecodeError):
                time.sleep(0.05)
        schema = '{"type":"object","properties":{},"additionalProperties":false}'
        profile = {
            'tool': {'id': 'owned_standard_document', 'organization_id': context['credentials']['organization_id'],
                     'version': 1, 'name': 'Owned pilot policy example',
                     'description': 'Read one operator-selected example document through standard MCP',
                     'effect': 'read', 'destination': 'Explicit owned loopback MCP peer',
                     'provider': 'owned-local-mcp', 'price_version': 'operator-zero-tariff-v1',
                     'max_charge_microusd': '0', 'input_schema_json': schema,
                     'input_schema_digest': hashlib.sha256(schema.encode()).hexdigest(),
                     'require_distinct_approver': False, 'timeout_seconds': 5,
                     'max_input_bytes': 4096, 'max_output_bytes': 4096, 'active': True},
            'transport': 'mcp_https', 'result_contract': 'mcp_read_text_v1',
            'remote_tool': 'read_document', 'endpoint': peer['endpoint'],
            'trusted_ca_pem': peer['trusted_ca_pem'], 'allow_private_network': True,
        }
        profile_file = private / 'mcp-profile.json'
        with profile_file.open('x') as stream:
            os.chmod(profile_file, 0o600); json.dump(profile, stream)
        context['record']('install_standard_mcp_read_profile', [context['binary'], 'install-tool', '--file', profile_file])
        route = '/api/v1/tasks/' + task['id']
        view = request(route + '/tool-executions', {
            'occurrence_id': 'owned-read-' + uuid.uuid4().hex,
            'expected_task_version': task['version'], 'candidate_id': candidate['id'],
            'artifact_id': candidate['artifact_id'], 'artifact_version': candidate['artifact_version'],
            'content_digest': candidate['content_digest'], 'tool_id': profile['tool']['id'],
            'tool_version': 1, 'arguments_json': '{}', 'expires_in_seconds': 60,
        })
        execution_route = route + '/tool-executions/' + view['execution']['id']
        def binding(value):
            return {'expected_task_version': value['task']['version'],
                    'expected_execution_version': value['execution']['version'],
                    'expected_approval_version': value['approval']['version'],
                    'intent_hash': value['execution']['intent_hash']}
        view = request(execution_route + '/decide', dict(binding(view), decision='approve', reason='Scripted owner approval of exactly this bounded example read'))
        request(execution_route + '/dispatch', dict(binding(view), acknowledge_cost=True))
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            view = request(execution_route)
            if view['execution']['status'] == 'succeeded':
                break
            if view['execution']['status'] in ('rejected', 'unknown', 'cancelled') or child.poll() is not None:
                raise RuntimeError('Installed standard MCP read did not succeed')
            time.sleep(0.1)
        if view['execution']['status'] != 'succeeded' or not view.get('receipt'):
            raise RuntimeError('Installed MCP read timed out')
        receipt = view['receipt']
        artifact = request(route + '/artifacts/' + receipt['artifact_id'])
        text = artifact['content']
        if (json.loads(text) != [DOCUMENT, 'Read invocation 1']
                or hashlib.sha256(text.encode()).hexdigest() != receipt['content_digest']
                or view['task']['candidate_id'] != candidate['id']
                or view['task']['status'] != 'waiting_input'):
            raise RuntimeError('Standard read did not retain exact data and source candidate')
        context['stop_child'](child)
        report = {'passed': True, 'transport': 'actual_tls_sse', 'protocol': '2026-07-28',
                'peer': 'upstream_go_sdk_1.8.0_stateless_test_document_reader',
                'tool_selection': 'scripted_owner_not_model', 'observed_response_invocation_ordinal': 1,
                'result_sha256': receipt['content_digest'], 'content_blocks': 2,
                'task_accepted_by_read': False, 'charge_bound_microusd': '0',
                'charge_is_operator_assumption': True, 'billing_settled': False,
                'receipt_id': receipt['id'], 'artifact_id': artifact['id'], 'artifact_version': artifact['version'],
                'receipt_execution_restart_verified': False}
        snapshot.update(route=execution_route, execution=view['execution'], receipt=receipt, report=report)
        return report

    def verify_restart(request):
        view = request(snapshot['route'])
        if view['execution'] != snapshot['execution'] or view.get('receipt') != snapshot['receipt']:
            raise RuntimeError('MCP execution or receipt changed after owned database restart')
        snapshot['report']['receipt_execution_restart_verified'] = True
        return True

    exercise.verify_restart = verify_restart
    return exercise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle', type=Path, required=True)
    parser.add_argument('--manifest-sha256', required=True)
    parser.add_argument('--endpoint', required=True)
    parser.add_argument('--server', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--response', type=Path, required=True)
    args = parser.parse_args()
    probe = local_probe(args.endpoint, 'qwen2.5:1.5b', args.response.absolute(),
                        before_accept=read_probe(args.server))
    report = verify(args.bundle.absolute(), args.manifest_sha256, args.evidence.absolute(), extra_probe=probe)
    print(json.dumps({'passed': report['passed'], 'probe': report['additional_probe']}, indent=2))


if __name__ == '__main__':
    main()

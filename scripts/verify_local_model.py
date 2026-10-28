#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Exercise one explicit local Ollama model through an installed RunVerge worker.

Uses the owned-cluster native verifier. No paid provider, cloud login, automatic
retry, final billing assertion or independent-human-usability claim is made.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import time
from urllib.parse import urlsplit
from urllib.error import HTTPError

from verify_native_bundle import verify


def local_probe(endpoint, model, response_file, exercise_revision=False, before_accept=None, local_unmetered=False):
    url = urlsplit(endpoint)
    if (url.scheme != 'http' or url.hostname != '127.0.0.1' or not url.port
            or url.username or url.password or url.query or url.fragment
            or url.path != '/v1/chat/completions' or model != 'qwen2.5:1.5b'
            or endpoint != 'http://127.0.0.1:%d/v1/chat/completions' % url.port):
        raise RuntimeError('This acceptance probe requires the explicit local Qwen2.5 1.5B endpoint')
    initial_response = response_file.with_name(response_file.stem + '.initial' + response_file.suffix)
    if response_file.exists() or (exercise_revision and initial_response.exists()):
        raise RuntimeError('Response evidence path must be new')

    snapshots = {}

    def exercise(context):
        request, record = context['request'], context['record']
        # Restart only the test-owned worker so it receives this test-only local
        # placeholder. Ollama ignores the bearer value; it is not authentication.
        context['stop_child'](context['worker'])
        context['env']['PLATFORM_MODEL_KEY_LOCAL_OLLAMA'] = 'local-only-placeholder-not-an-api-secret'
        profile = {
            'id': 'local_ollama_qwen_1_5b',
            'organization_id': context['credentials']['organization_id'],
            'version': 1, 'model': model, 'provider_account': 'owned-local-ollama',
            'price_version': 'local-no-paid-api-test-v1', 'endpoint': endpoint,
            'credential_environment': 'PLATFORM_MODEL_KEY_LOCAL_OLLAMA',
            'max_charge_microusd': 0 if local_unmetered else 1, 'max_input_tokens': 4096,
            'input_microusd_per_million': 0, 'output_microusd_per_million': 0,
            'fixed_microusd': 0, 'max_output_tokens': 256,
            'max_input_bytes': 16384, 'max_output_bytes': 16384,
            'timeout_milliseconds': 60000,
            'bound_assumptions': 'Explicit owned local inference with cloud disabled; no paid API. One micro-USD is a conservative synthetic admission hold, not an invoice or a measurement of local electricity/hardware cost.',
            'token_limit_field': 'max_tokens', 'allow_insecure_loopback': True,
            'active': True,
        }
        if local_unmetered:
            profile['accounting_mode'] = 'operator_unmetered_local'
            profile['bound_assumptions'] = 'Trusted operator declares zero API tariff for owned local inference with cloud disabled. Hardware and electricity costs are not measured. Byte/token/request-time/lifetime attempt limits remain; no CPU/GPU quota or final invoice is inferred.'
        profile_file = context['private'] / 'local-model-profile.json'
        fd = os.open(profile_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w') as stream:
            json.dump(profile, stream)
        record('install_explicit_local_model_profile', [context['binary'], 'install-model', '--file', profile_file])
        if local_unmetered:
            models = request('/api/v1/models')
            selected = [item for item in models if item['id'] == profile['id']]
            if (len(selected) != 1 or selected[0].get('accounting_mode') != 'operator_unmetered_local'
                    or selected[0]['max_charge_microusd'] != 0
                    or any(selected[0].get(key) != profile[key] for key in ('max_input_tokens', 'max_output_bytes', 'timeout_milliseconds'))):
                raise RuntimeError('Installed catalogue lost explicit local mode or resource bounds')
        worker = context['process']('worker-local-model', [context['binary'], 'worker', '--concurrency', '1'])
        draft = request('/api/v1/agents', {
            'name': 'Local model release probe',
            'instructions': 'You help an internal IT team. Answer in three concise numbered steps. Use only the supplied task. Do not claim to have performed external actions.',
            'model': model, 'tools': [], 'budget_microusd': 1000,
        })
        agent = request('/api/v1/agents/' + draft['id'] + '/publish', {'expected_version': draft['version']})
        task = request('/api/v1/tasks', {
            'goal': 'Draft a three-step plan for a small team to pilot a local AI assistant. Include access control, a spending or resource limit, and human review of results. This is a draft planning task; do not execute anything.',
            'agent_id': agent['id'], 'budget_microusd': 1000,
            'execution_timeout_seconds': 180,
        })
        route = '/api/v1/tasks/' + task['id']
        started = time.monotonic()

        def read_candidate(expected_count):
            deadline = time.monotonic() + 90
            while time.monotonic() < deadline:
                current = request(route)
                if current['status'] == 'waiting_input':
                    break
                if current['status'] in ('blocked', 'failed', 'cancelled', 'succeeded') or worker.poll() is not None:
                    raise RuntimeError('Local model did not produce a reviewable candidate: ' + current['status'])
                time.sleep(0.25)
            if current['status'] != 'waiting_input':
                raise RuntimeError('Local model acceptance timed out; no automatic inference retry was made')
            candidates = request(route + '/candidates')
            artifacts = request(route + '/artifacts')
            selected = [item for item in candidates if item['id'] == current.get('candidate_id')]
            if len(candidates) != expected_count or len(selected) != 1:
                raise RuntimeError('Unexpected immutable local-model candidate history')
            selected = selected[0]
            matches = [artifact for artifact in artifacts if artifact['id'] == selected['artifact_id']
                       and artifact['version'] == selected['artifact_version']]
            if len(matches) != 1:
                raise RuntimeError('Exact local-model artifact is absent')
            text = matches[0]['content']
            if not isinstance(text, str) or not 40 <= len(text.encode()) <= 16384:
                raise RuntimeError('Local model did not return bounded text')
            hashed = hashlib.sha256(text.encode()).hexdigest()
            if hashed != selected['content_digest']:
                raise RuntimeError('Candidate content digest differs from exact output')
            return current, selected, text, hashed

        task, candidate, content, content_digest = read_candidate(1)
        before_spent, before_reserved = task['spent_microusd'], task['reserved_microusd']
        if before_spent != '0' or before_reserved != ('0' if local_unmetered else '1'):
            raise RuntimeError('Local model did not retain the explicitly synthetic admission hold')
        if local_unmetered:
            costs = request(route + '/costs?limit=1')
            if costs.get('unknown_reservation_count') != '1' or costs['unknown_microusd'] != '0' or costs['reservations'][0]['status'] != 'unknown':
                raise RuntimeError('Zero local amount concealed unknown accounting')
            artifacts = request(route + '/artifacts')
            bound = [item for item in artifacts if item['id'] == candidate['artifact_id'] and item['version'] == candidate['artifact_version']]
            if len(bound) != 1 or bound[0].get('provenance', {}).get('model_accounting_mode') != 'operator_unmetered_local':
                raise RuntimeError('Exact candidate provenance lost its pinned accounting mode')
        revision = {'requested': False, 'model_attempts': 1}
        if exercise_revision:
            initial_candidate = dict(candidate)
            runs_before = request(route + '/runs')
            run_ids_before = [item['id'] for item in runs_before]
            changed = request(route + '/review', {
                'expected_version': task['version'], 'candidate_id': candidate['id'],
                'artifact_id': candidate['artifact_id'], 'artifact_version': candidate['artifact_version'],
                'content_digest': content_digest, 'decision': 'request_changes',
                'reason': 'Refine the draft with an explicit designated human reviewer and revision before use. Scripted integration decision.',
            })
            if changed['status'] != 'blocked' or changed['reserved_microusd'] != before_reserved or [item['id'] for item in request(route + '/runs')] != run_ids_before:
                raise RuntimeError('Feedback created execution or changed the old admission hold')
            new_attempt = {
                'expected_version': changed['version'],
                'reason': 'Explicit new local-only attempt to address additional review requirements.',
                'feedback': 'Rewrite the plan with exactly these three numbered headings: 1. Access control; 2. Resource limits; 3. Human review. In item 3 require a designated human reviewer to check each answer before use, reject mistakes and request revision. Keep each item short. Do not claim to implement anything.',
                'acknowledge_new_cost': False,
            }
            try:
                request(route + '/retry', new_attempt)
            except HTTPError as error:
                if error.code != 400:
                    raise RuntimeError('Unexpected rejection of missing new-attempt consent') from None
            else:
                raise RuntimeError('New local-model attempt accepted without explicit cost consent')
            if [item['id'] for item in request(route + '/runs')] != run_ids_before or request(route)['reserved_microusd'] != before_reserved:
                raise RuntimeError('Rejected new-attempt command changed execution/accounting')
            response_file.parent.mkdir(parents=True, exist_ok=True)
            with initial_response.open('x', encoding='utf-8') as stream:
                stream.write(content)
            new_attempt['acknowledge_new_cost'] = True
            request(route + '/retry', new_attempt)
            task, candidate, content, content_digest = read_candidate(2)
            before_spent, before_reserved = task['spent_microusd'], task['reserved_microusd']
            old = [item for item in request(route + '/candidates') if item['id'] == initial_candidate['id']]
            if before_spent != '0' or before_reserved != ('0' if local_unmetered else '2') or old != [initial_candidate] or len(request(route + '/runs')) != 2:
                raise RuntimeError('Revision lost old candidate, run history or unresolved admission hold')
            if local_unmetered:
                costs = request(route + '/costs?limit=1')
                if (costs.get('unknown_reservation_count') != '2' or costs['unknown_microusd'] != '0'
                        or len(costs['reservations']) != 1 or not costs.get('next_reservation_cursor')):
                    raise RuntimeError('Paginated local attempts lost their full uncertainty count')
            lower = content.lower()
            if not all(word in lower for word in ('access', 'limit', 'human', 'review')):
                raise RuntimeError('Revised sample still omits required topic words; do not accept it automatically')
            revision = {'requested': True, 'model_attempts': 2,
                        'initial_candidate_id': initial_candidate['id'],
                        'initial_content_sha256': initial_candidate['content_digest'],
                        'feedback_created_no_new_run': True,
                        'missing_cost_consent_rejected': True,
                        'old_candidate_preserved': True,
                        'sample_required_topic_words_present': True,
                        'topic_check_is_semantic_quality_proof': False}
        tool_observation = None
        if before_accept is not None:
            tool_observation = before_accept(context, task, candidate)
            task = request(route)
            if (task['status'] != 'waiting_input' or task['candidate_id'] != candidate['id']
                    or task['spent_microusd'] != before_spent
                    or task['reserved_microusd'] != before_reserved):
                raise RuntimeError('Read probe changed candidate identity or model accounting')
        accepted = request(route + '/review', {
            'expected_version': task['version'], 'candidate_id': candidate['id'],
            'artifact_id': candidate['artifact_id'], 'artifact_version': candidate['artifact_version'],
            'content_digest': content_digest, 'decision': 'accept',
            'reason': 'Scripted owner API acceptance of this exact local-model artifact; not independent human usability or factual validation.',
        })
        if accepted['status'] != 'succeeded' or accepted['spent_microusd'] != before_spent or accepted['reserved_microusd'] != before_reserved:
            raise RuntimeError('Result acceptance changed financial facts or failed')
        snapshots['route'] = route
        snapshots['task'] = request(route)
        for name in ('candidates', 'artifacts', 'costs', 'decisions'):
            snapshots[name] = request(route + '/' + name)
        response_file.parent.mkdir(parents=True, exist_ok=True)
        with response_file.open('x', encoding='utf-8') as stream:
            stream.write(content)
        context['stop_child'](worker)
        return {
            'scope': 'one_real_local_model_task_with_scripted_owner_acceptance',
            'model': model, 'passed': True, 'task_id': task['id'],
            'candidate_id': candidate['id'], 'artifact_id': candidate['artifact_id'],
            'artifact_version': candidate['artifact_version'], 'content_sha256': content_digest,
            'output_bytes': len(content.encode()), 'duration_seconds': round(time.monotonic() - started, 3),
            'final_status': accepted['status'], 'spent_microusd': before_spent,
            'reserved_microusd': before_reserved, 'reservation_is_synthetic': not local_unmetered,
            'accounting_mode': 'operator_unmetered_local' if local_unmetered else 'metered_synthetic_test_hold',
            'hardware_cost_measured': False,
            'unknown_reservation_count': snapshots['costs'].get('unknown_reservation_count'),
            'paid_api_used': False, 'final_billing_asserted': False,
            'review_method': 'scripted_owner_api', 'independent_human_acceptance': False,
            'quality_benchmark': False,
            'revision': revision,
            'additional_read_observation': tool_observation,
            'restart_verified': False,
        }

    def verify_restart(request):
        route = snapshots['route']
        if request(route) != snapshots['task']:
            raise RuntimeError('Local-model task changed after owned database/API/worker restart')
        for name in ('candidates', 'artifacts', 'costs', 'decisions'):
            if request(route + '/' + name) != snapshots[name]:
                raise RuntimeError('Local-model persisted snapshot changed on restart: ' + name)
        if before_accept is not None and callable(getattr(before_accept, 'verify_restart', None)):
            before_accept.verify_restart(request)
        return True

    exercise.verify_restart = verify_restart
    return exercise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle', type=Path, required=True)
    parser.add_argument('--manifest-sha256', required=True)
    parser.add_argument('--endpoint', required=True)
    parser.add_argument('--model', default='qwen2.5:1.5b')
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--response', type=Path, required=True)
    parser.add_argument('--exercise-revision', action='store_true', help='Explicitly test feedback and a separately consented second local attempt')
    parser.add_argument('--local-unmetered', action='store_true', help='Use the explicit API0.14 operator-declared local zero tariff; hardware costs remain unmeasured')
    args = parser.parse_args()
    try:
        probe = local_probe(args.endpoint, args.model, args.response.absolute(), args.exercise_revision, local_unmetered=args.local_unmetered)
        report = verify(args.bundle.absolute(), args.manifest_sha256,
                        args.evidence.absolute(), extra_probe=probe)
    except Exception as error:
        raise SystemExit(str(error) if isinstance(error, RuntimeError) else 'Local model verification failed: ' + type(error).__name__) from None
    print(json.dumps({'passed': report['passed'], 'local_probe': report['additional_probe']}, indent=2))


if __name__ == '__main__':
    main()

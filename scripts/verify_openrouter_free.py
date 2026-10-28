#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Check a free OpenRouter model through a new disposable installed RunVerge.

Explicit opt-in only: two new model attempts, zero provider price caps, no fallback.
Never uses a saved database. Requires PostgreSQL initdb/pg_ctl on the operator host.
"""
import argparse
from decimal import Decimal
import hashlib
import json
from pathlib import Path
import re
import time
from urllib.error import HTTPError
from urllib.request import build_opener, ProxyHandler, HTTPRedirectHandler

from verify_native_bundle import verify
from model_key import load_key


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def check_free_catalog(model):
    if not re.fullmatch(r'[a-z0-9][a-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*:free', model) or model.startswith('openrouter/'):
        raise ValueError('Select one fixed :free model, not a router')
    opener = build_opener(ProxyHandler({}), NoRedirect())
    with opener.open('https://openrouter.ai/api/v1/models', timeout=20) as response:
        raw = response.read(8 * 1024 * 1024 + 1)
    if len(raw) > 8 * 1024 * 1024:
        raise ValueError('Catalog size limit exceeded')
    catalog = json.loads(raw, parse_float=Decimal)
    matches = [item for item in catalog['data'] if item['id'] == model]
    if len(matches) != 1:
        raise ValueError('Fixed free model is absent from the current catalog')
    price = matches[0].get('pricing', {})
    if not {'prompt', 'completion'} <= set(price) or any(Decimal(str(value)) != 0 for value in price.values()):
        raise ValueError('Catalog does not declare zero pricing for this model')


class FreeProbe:
    def __init__(self, key, model, data_policy):
        self.key, self.model, self.data_policy = key, model, data_policy
        self.snapshots = {}
        self.last_task = {}

    def __call__(self, context):
        if context.get('register_sensitive'):
            context['register_sensitive'](self.key)
        request, record, binary = context['request'], context['record'], context['binary']
        profile = context['private'] / 'free-model.json'
        record('prepare_free_openrouter_profile_without_key_access', [binary, 'prepare-model',
               '--provider', 'openrouter', '--organization-id', context['credentials']['organization_id'],
               '--model', self.model, '--provider-account', 'operator-free-test', '--price-version', 'zero-filter-v1',
               '--input-usd-per-million', '0', '--output-usd-per-million', '0', '--max-call-usd', '0.000001',
               '--max-output-tokens', '2048', '--data-collection', self.data_policy, '--out', profile])
        record('install_free_openrouter_profile', [binary, 'install-model', '--file', profile])
        context['stop_child'](context['worker'])
        # Popen receives its own environment. Remove the key immediately after
        # spawning the worker, before any API/restart child can inherit it.
        context['env']['PLATFORM_MODEL_KEY_OPENROUTER'] = self.key
        try:
            worker = context['process']('free-openrouter-worker', [binary, 'worker', '--concurrency', '1'])
        finally:
            context['env'].pop('PLATFORM_MODEL_KEY_OPENROUTER', None)
        phase = 'create_agent'
        try:
            draft = request('/api/v1/agents', {'name': 'Free OpenRouter integration check',
                'instructions': 'Write a short draft using only the supplied public test text. Do not execute external actions.',
                'model': self.model, 'tools': [], 'budget_microusd': 1000})
            phase = 'publish_agent'
            agent = request('/api/v1/agents/' + draft['id'] + '/publish', {'expected_version': draft['version']})
            phase = 'create_task'
            task = request('/api/v1/tasks', {'agent_id': agent['id'], 'budget_microusd': 1000,
                'max_runs': 2, 'execution_timeout_seconds': 240,
                'goal': 'Draft three short steps for piloting an AI assistant in a fictional team: access control, resource limits and human review. This is public synthetic test data.'})
            route = '/api/v1/tasks/' + task['id']

            def candidate(count):
                nonlocal phase
                phase = 'read_candidate_' + str(count)
                deadline = time.monotonic() + 90
                while time.monotonic() < deadline:
                    current = request(route)
                    self.last_task = current
                    if current['status'] == 'waiting_input':
                        break
                    if current['status'] in ('blocked', 'failed', 'cancelled', 'succeeded') or worker.poll() is not None:
                        raise RuntimeError('Free provider did not produce a candidate; no inference retry was made')
                    time.sleep(0.25)
                if current['status'] != 'waiting_input':
                    raise RuntimeError('Free provider candidate timeout; no inference retry was made')
                items = request(route + '/candidates')
                selected = [item for item in items if item['id'] == current.get('candidate_id')]
                if len(items) != count or len(selected) != 1:
                    raise RuntimeError('Candidate history mismatch')
                selected = selected[0]
                artifacts = [item for item in request(route + '/artifacts') if item['id'] == selected['artifact_id'] and item['version'] == selected['artifact_version']]
                if len(artifacts) != 1:
                    raise RuntimeError('Exact provider artifact absent')
                artifact = artifacts[0]
                content = artifact['content']
                if not isinstance(content, str) or not 20 <= len(content.encode()) <= 16384 or self.key in content:
                    raise RuntimeError('Invalid bounded candidate output')
                if hashlib.sha256(content.encode()).hexdigest() != selected['content_digest']:
                    raise RuntimeError('Candidate digest mismatch')
                costs = request(route + '/costs')
                if current['spent_microusd'] != '0' or current['reserved_microusd'] != str(count) or costs['unknown_reservation_count'] != str(count):
                    raise RuntimeError('Unsettled admission hold changed')
                if artifact['provenance'].get('estimated_microusd') != '0' or not artifact['provenance'].get('provider_request_id'):
                    raise RuntimeError('Provider provenance mismatch')
                return current, selected

            task, first = candidate(1)
            old_runs = request(route + '/runs')
            phase = 'request_changes'
            changed = request(route + '/review', {'expected_version': task['version'], 'candidate_id': first['id'],
                'artifact_id': first['artifact_id'], 'artifact_version': first['artifact_version'],
                'content_digest': first['content_digest'], 'decision': 'request_changes', 'reason': 'Scripted integration review: require a named human reviewer.'})
            if changed['status'] != 'blocked' or request(route + '/runs') != old_runs:
                raise RuntimeError('Feedback triggered an unapproved model attempt')
            retry = {'expected_version': changed['version'], 'reason': 'Explicit second free integration attempt.',
                'feedback': 'Keep three short steps. Explicitly require a designated human reviewer to check answers, reject errors and request revision.', 'acknowledge_new_cost': False}
            try:
                request(route + '/retry', retry)
            except HTTPError as error:
                if error.code != 400:
                    raise RuntimeError('Unexpected new-attempt refusal') from None
            else:
                raise RuntimeError('Attempt without consent was admitted')
            if request(route + '/runs') != old_runs:
                raise RuntimeError('Denied attempt changed run history')
            retry['acknowledge_new_cost'] = True
            phase = 'explicit_second_attempt'
            request(route + '/retry', retry)
            task, second = candidate(2)
            if [item for item in request(route + '/candidates') if item['id'] == first['id']] != [first]:
                raise RuntimeError('First candidate changed during revision')
            phase = 'accept_exact_candidate'
            accepted = request(route + '/review', {'expected_version': task['version'], 'candidate_id': second['id'],
                'artifact_id': second['artifact_id'], 'artifact_version': second['artifact_version'],
                'content_digest': second['content_digest'], 'decision': 'accept', 'reason': 'Scripted exact-artifact API acceptance; not an independent human quality assessment.'})
            if accepted['status'] != 'succeeded' or accepted['spent_microusd'] != '0' or accepted['reserved_microusd'] != '2':
                raise RuntimeError('Acceptance changed financial facts')
            phase = 'snapshot_completed_workflow'
            self.snapshots = {'route': route, 'task': request(route), 'models': request('/api/v1/models')}
            for name in ('candidates', 'artifacts', 'costs', 'decisions', 'runs'):
                self.snapshots[name] = request(route + '/' + name)
            return {'scope': 'two_free_openrouter_candidates_with_scripted_review', 'passed': True,
                'model': self.model, 'data_collection': self.data_policy, 'successful_candidates': 2,
                'provider_price_caps_zero': True, 'paid_fallbacks': False, 'feedback_started_no_run': True,
                'missing_consent_rejected': True, 'prior_candidate_preserved': True,
                'spent_microusd': '0', 'reserved_microusd': '2', 'unknown_reservation_count': '2',
                'final_billing_asserted': False, 'independent_human_usability_proven': False}
        except HTTPError as error:
            # Only a fixed phase and numeric status; never the URL, body or headers.
            raise RuntimeError('Free model workflow HTTP rejection at ' + phase + ' (status ' + str(error.code) + ')') from None
        except RuntimeError as error:
            context['stop_child'](worker)
            details = failure_details(self.last_task, context['private'] / 'free-openrouter-worker.log')
            raise RuntimeError(str(error) + ' [phase=' + phase + '; ' + details + ']') from None
        finally:
            context['stop_child'](worker)

    def verify_restart(self, request):
        route = self.snapshots['route']
        if request(route) != self.snapshots['task'] or request('/api/v1/models') != self.snapshots['models']:
            raise RuntimeError('Restart changed task or model configuration')
        for name in ('candidates', 'artifacts', 'costs', 'decisions', 'runs'):
            if request(route + '/' + name) != self.snapshots[name]:
                raise RuntimeError('Restart changed immutable workflow evidence')
        return True


def failure_details(task, log_path):
    statuses = {'queued', 'running', 'waiting_input', 'blocked', 'failed', 'cancelled', 'succeeded'}
    reasons = {'model_outcome_unknown', 'model_output_rejected', 'model_input_rejected',
               'model_accounting_pending', 'model_attempt_requires_attention',
               'model_credentials_unavailable', 'model_profile_unavailable',
               'configuration_profile_unavailable', 'authorization_revoked',
               'execution_deadline', 'attempt_limit', 'budget_exceeded'}
    status = task.get('status')
    reason = task.get('waiting_reason')
    fields = ['status=' + (status if status in statuses else 'unknown'),
              'waiting_reason=' + (reason if reason in reasons else 'unknown')]
    try:
        with log_path.open('rb') as stream:
            stream.seek(0, 2)
            stream.seek(max(0, stream.tell() - 8192))
            body = stream.read(8192).decode('utf-8', errors='replace')
        codes = {'connection_not_established', 'transport_outcome_unknown', 'dispatch_permit_expired',
                 'provider_shared_pool_limited', 'output_budget_exhausted',
                 'provider_rejected', 'provider_rate_limited', 'provider_status_unknown',
                 'redirect_rejected', 'invalid_response_format', 'invalid_response_json',
                 'invalid_usage', 'invalid_completion', 'unsupported_completion',
                 'output_token_limit_exceeded', 'openrouter_identity_unknown',
                 'response_read_unknown', 'response_too_large'}
        found = [code for code in re.findall(r'\bgateway_code=([a-z_]+)\b', body) if code in codes]
        if found:
            fields.append('gateway_code=' + found[-1])
        statuses = re.findall(r'\bprovider_status=([1-5][0-9]{2})\b', body)
        if statuses:
            fields.append('provider_status=' + statuses[-1])
    except OSError:
        pass
    return '; '.join(fields)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle', type=Path, required=True)
    parser.add_argument('--manifest-sha256', required=True)
    parser.add_argument('--key-file', type=Path, required=True)
    parser.add_argument('--model', required=True)
    parser.add_argument('--data-collection', choices=['deny', 'allow'], default='deny')
    parser.add_argument('--evidence', type=Path, required=True)
    args = parser.parse_args()
    try:
        check_free_catalog(args.model)
        key = load_key(args.key_file)
        report = verify(args.bundle.absolute(), args.manifest_sha256, args.evidence.absolute(), FreeProbe(key, args.model, args.data_collection))
        if key in json.dumps(report):
            raise RuntimeError('Private value in verification evidence')
        print(json.dumps({'passed': report['passed'], 'restart_verified': report['additional_probe']['restart_verified'], 'successful_candidates': 2, 'provider_price_caps_zero': True, 'cleanup_complete': report['cleanup_complete']}))
    except Exception as error:
        raise SystemExit('Free OpenRouter verification did not pass: ' + type(error).__name__ + '. No automatic inference retry was made.') from None

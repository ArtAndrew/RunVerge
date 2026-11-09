#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Verify a synthetic signed GitHub event against an installed local package."""
import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import hmac
import json
from pathlib import Path
import secrets
import time
from urllib.error import URLError
from urllib.request import ProxyHandler, Request, build_opener

from verify_ci_triage import MODEL, model_identity
from verify_local_model import install_local_probe_model
from verify_native_bundle import unused_port, verify
from verify_native_recovery import private_json


class GitHubCIProbe:
    real_model_called = True

    def __init__(self, endpoint, browser=None):
        self.endpoint = endpoint
        self.browser = browser

    @contextmanager
    def receiver(self, label):
        context = self.context
        addr = '127.0.0.1:%d' % unused_port()
        base = 'http://' + addr
        child = context['process'](label, [context['binary'], 'serve', '--listen', addr,
                                          '--trusted-tls-proxy', '--github-ci-config', self.config])
        opener = build_opener(ProxyHandler({}))
        try:
            for _ in range(80):
                if child.poll() is not None:
                    break
                try:
                    with opener.open(base + '/readyz', timeout=1) as response:
                        if response.status == 200:
                            break
                except URLError:
                    time.sleep(0.15)
            else:
                raise RuntimeError('Installed webhook server did not become ready')
            if child.poll() is not None:
                raise RuntimeError('Installed webhook server exited during startup')
            yield base
        finally:
            context['stop_child'](child)

    def send(self, base, delivery):
        request = Request(base + '/hooks/github/ci', data=self.body, headers={
            'Content-Type': 'application/json', 'X-GitHub-Event': 'workflow_run',
            'X-GitHub-Delivery': delivery, 'X-Hub-Signature-256': 'sha256=' + self.signature,
        })
        with build_opener(ProxyHandler({})).open(request, timeout=5) as response:
            if response.status != 202 or response.read(1):
                raise RuntimeError('Signed fixture event did not receive an empty acceptance')

    def __call__(self, context):
        self.context = context
        request = context['request']
        install_local_probe_model(context, self.endpoint, MODEL, True)
        org = context['credentials']['organization_id']
        if self.browser:
            context['record']('install_github_ci_comment_tool', [
                context['binary'], 'install-github-ci-comment', '--organization', org, '--repository', 'octo-org/sample',
                '--credential-environment', 'PLATFORM_TOOL_KEY_CI_FIXTURE',
            ])
        account = request('/api/v1/service-accounts', {
            'display_name': 'Synthetic GitHub CI sender', 'role': 'member', 'reason': 'Native fixture',
        })
        key_file = context['private'] / 'ci-probe-key.json'
        context['record']('issue_owned_ci_service_key', [
            context['binary'], 'issue-service-token', '--organization', org, '--principal', account['principal_id'],
            '--role-ceiling', 'member', '--reason', 'Native CI fixture', '--out', key_file,
        ])
        token = json.loads(key_file.read_text())['token']
        context['register_sensitive'](token)
        key_file.unlink()
        agent = request('/api/v1/agents', {
            'name': 'Synthetic GitHub CI reviewer', 'instructions': 'Summarize only supplied evidence.',
            'model': MODEL, 'budget_microusd': 1000,
        })
        agent = request('/api/v1/agents/' + agent['id'] + '/publish', {'expected_version': agent['version']})
        secret = secrets.token_hex(32)
        context['register_sensitive'](secret)
        self.config = private_json(context['private'] / 'ci-probe-receiver.json', {
            'organization_id': org, 'service_token': token, 'webhook_secret': secret, 'repository': 'octo-org/sample',
            'repository_id': 10, 'workflow_id': 20, 'branch': 'main', 'agent_id': agent['id'],
            'agent_version': agent['version'], 'budget_microusd': 1000, 'max_per_hour': 1,
        })
        self.body = json.dumps({
            'action': 'completed', 'repository': {'id': 10, 'full_name': 'octo-org/sample'},
            'workflow_run': {'id': 4102, 'run_attempt': 1, 'workflow_id': 20, 'event': 'push', 'status': 'completed',
                             'conclusion': 'failure', 'head_branch': 'main', 'head_sha': 'a' * 40,
                             'head_repository': {'id': 10}, 'updated_at': datetime.now(timezone.utc).isoformat()},
        }).encode()
        self.signature = hmac.new(secret.encode(), self.body, hashlib.sha256).hexdigest()
        context['register_sensitive'](self.signature)
        worker = context['process']('worker-signed-ci', [context['binary'], 'worker', '--concurrency', '1'])
        try:
            with self.receiver('api-signed-ci') as base:
                self.send(base, 'synthetic-original')
                self.send(base, 'synthetic-redelivery')
            task = self.only_ci_task(request)
            route = '/api/v1/tasks/' + task['id']
            deadline = time.monotonic() + 45
            while task['status'] in ('queued', 'running') and worker.poll() is None and time.monotonic() < deadline:
                time.sleep(0.2)
                task = request(route)
            if task['status'] != 'waiting_input' or task['attempt_count'] != 1 or task['max_runs'] != 1:
                raise RuntimeError('Signed CI task did not produce a draft for human review')
            candidates = request(route + '/candidates')
            artifacts = request(route + '/artifacts')
            decisions = request(route + '/decisions')
            if (len(candidates) != 1 or len(artifacts) != 1 or decisions or candidates[0]['id'] != task['candidate_id']
                    or not any(row['id'] == task['id'] for row in request('/api/v1/tasks?view=attention'))):
                raise RuntimeError('Draft or queue binding differs from the admitted CI attempt')
            comment = None
            if self.browser:
                node, playwright, chromium = self.browser
                config = private_json(context['private'] / 'ci-comment-browser.json', {
                    'base_url': context['base_url'], 'credentials': context['credentials'], 'task_id': task['id'],
                    'draft': artifacts[0]['content'], 'playwright': str(playwright), 'chromium': str(chromium),
                })
                try:
                    env = {key: context['env'][key] for key in ('PATH', 'HOME', 'TMPDIR', 'LANG')}
                    comment = json.loads(context['record']('prepare_ci_comment_through_installed_workspace', [
                        node, Path(__file__).with_name('ci_comment_browser.mjs'), config,
                    ], env))
                finally:
                    config.unlink()
                task = request(route)
                actions = request(route + '/tool-executions')
                if (comment != {'exact_preview': True, 'prepared_only': True, 'mutations': 1} or len(actions) != 1
                        or actions[0]['status'] != 'awaiting_approval' or actions[0]['content_digest'] != candidates[0]['content_digest']):
                    raise RuntimeError('Installed Workspace did not prepare the exact CI comment')
            self.saved = {route: task, route + '/candidates': candidates, route + '/artifacts': artifacts,
                          route + '/decisions': decisions, route + '/costs': request(route + '/costs')}
            if self.browser:
                self.saved[route + '/tool-executions'] = actions
            return {'synthetic_deliveries_before_restart': 2, 'one_durable_task': True, 'model_attempts': 1,
                    'attention_queue_contains_draft': True, 'draft_sha256': candidates[0]['content_digest'],
                    'human_decisions': 0, 'local_model_only': True, 'comment_preview': comment}
        finally:
            context['stop_child'](worker)

    @staticmethod
    def only_ci_task(request):
        tasks = [row for row in request('/api/v1/tasks') if row['id'].startswith('tsk_ci_')]
        if len(tasks) != 1:
            raise RuntimeError('Expected exactly one durable CI task')
        return tasks[0]

    def verify_restart(self, request):
        with self.receiver('api-signed-ci-restarted') as base:
            self.send(base, 'synthetic-redelivery-after-restart')
        self.only_ci_task(request)
        if any(request(route) != snapshot for route, snapshot in self.saved.items()):
            raise RuntimeError('Restart or replay changed task, draft, decisions or accounting')
        return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle', type=Path, required=True)
    parser.add_argument('--manifest-sha256', required=True)
    parser.add_argument('--ollama-port', type=int, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--browser-node', type=Path)
    parser.add_argument('--playwright', type=Path)
    parser.add_argument('--chromium', type=Path)
    args = parser.parse_args()
    try:
        if not 1 <= args.ollama_port <= 65535:
            raise RuntimeError('Invalid local model port')
        base = 'http://127.0.0.1:%d' % args.ollama_port
        model_identity(base)
        paths = (args.browser_node, args.playwright, args.chromium)
        if any(paths) and not all(paths):
            raise RuntimeError('Provide all three browser paths')
        browser = tuple(path.resolve(strict=True) for path in paths) if all(paths) else None
        report = verify(args.bundle.resolve(), args.manifest_sha256, args.evidence.resolve(),
                        extra_probe=GitHubCIProbe(base + '/v1/chat/completions', browser))
        print(json.dumps(report['additional_probe']))
    except Exception as error:
        raise SystemExit(str(error) if isinstance(error, RuntimeError) else type(error).__name__) from None


if __name__ == '__main__':
    main()

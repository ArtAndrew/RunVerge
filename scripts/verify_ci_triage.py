#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Produce CI triage drafts and optionally exercise a scripted Workspace repair."""
import argparse
from contextlib import contextmanager, nullcontext
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
from threading import Event, Lock, Thread
import time
from urllib.error import URLError
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from verify_local_model import install_local_probe_model
from verify_native_bundle import verify
from verify_native_recovery import private_json


MODEL = 'qwen2.5:1.5b'
MODEL_DIGEST = '8d97bd018f488e5b42ad1252376955174894338ddef7f5a9a931f908620cef4e'
ENGINE_VERSION = '0.40.11'
SYSTEM_PROMPT = 'Draft a short CI triage comment using the task evidence. Do not use tools or publish anything.'
FIXTURES = Path(__file__).resolve().parents[1] / 'examples/ci-triage'
CORRECTED_DISK_DRAFT = (
    'Finding: The runner ran out of disk space during cache restore. The test step did not run.\n'
    'Evidence: https://ci.example.invalid/runs/4102#L19-L21\n'
    'Next check: Inspect runner disk usage and cache size, reclaim space, then rerun the job.'
)


def require(condition, message):
    if not condition:
        raise RuntimeError('CI triage pilot: ' + message)


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


def model_identity(base):
    opener = build_opener(ProxyHandler({}))
    def get(route):
        with opener.open(base + route, timeout=3) as response:
            data = response.read(1024 * 1024 + 1)
        require(len(data) <= 1024 * 1024, 'local model metadata exceeds limit')
        return json.loads(data)

    require(get('/api/version')['version'] == ENGINE_VERSION, 'unexpected local engine revision')
    found = [model for model in get('/api/tags')['models'] if model['name'] == MODEL]
    require(len(found) == 1 and found[0]['digest'].removeprefix('sha256:') == MODEL_DIGEST,
            'local model digest differs from the reviewed registration')


@contextmanager
def gate_fixture_requests(endpoint, goals):
    """Hold exact synthetic requests at a local test boundary, then forward each once."""
    payloads = {name: {'model': MODEL, 'messages': [
        {'role': 'system', 'content': SYSTEM_PROMPT}, {'role': 'user', 'content': goal},
    ], 'stream': False, 'n': 1, 'max_tokens': 256} for name, goal in goals.items()}
    arrived = {name: Event() for name in goals}
    release = {name: Event() for name in goals}
    release['01-auth-regression'].set()
    stopping = Event()
    lock = Lock()

    class NoRedirect(HTTPRedirectHandler):
        def redirect_request(self, *args, **kwargs):
            return None

    class FixtureHandler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            self.connection.settimeout(3)
            try:
                size = int(self.headers.get('Content-Length', '0'))
                if self.path != '/v1/chat/completions' or not 0 < size <= 16384:
                    self.send_error(400)
                    return
                data = json.loads(self.rfile.read(size))
                name = next((key for key, payload in payloads.items() if data == payload), None)
                with lock:
                    if name is None or arrived[name].is_set():
                        self.send_error(400)
                        return
                    arrived[name].set()
                if not release[name].wait(42) or stopping.is_set():
                    self.send_error(503)
                    return
                request = Request(endpoint, json.dumps(payloads[name]).encode(),
                                  {'Content-Type': 'application/json'})
                opener = build_opener(ProxyHandler({}), NoRedirect())
                with opener.open(request, timeout=15) as response:
                    raw = response.read(16385)
                if len(raw) > 16384:
                    self.send_error(502)
                    return
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
            except (OSError, ValueError, URLError):
                self.close_connection = True

    class FixtureServer(ThreadingHTTPServer):
        daemon_threads = False

    with FixtureServer(('127.0.0.1', 0), FixtureHandler) as server:
        thread = Thread(target=server.serve_forever)
        thread.start()
        try:
            yield ('http://127.0.0.1:%d/v1/chat/completions' % server.server_port, arrived, release)
        finally:
            stopping.set()
            for event in release.values():
                event.set()
            server.shutdown()
            thread.join()


class CITriageProbe:
    real_model_called = True

    def __init__(self, endpoint, browser=None, parallel=False):
        self.endpoint = endpoint
        self.browser = browser
        self.parallel = parallel
        self.saved = []
        self.repair = None

    def __call__(self, context):
        goals = {fixture.stem: fixture.read_text().strip() for fixture in sorted(FIXTURES.glob('*.txt'))}
        require(set(goals) == {'01-auth-regression', '02-runner-disk', '03-ambiguous-timeout'},
                'expected three frozen inputs')
        gate = gate_fixture_requests(self.endpoint, goals) if self.parallel else nullcontext((self.endpoint, None, None))
        with gate as (endpoint, arrived, release):
            return self.run(context, goals, endpoint, arrived, release)

    def run(self, context, goals, endpoint, arrived, release):
        request = context['request']
        profile = install_local_probe_model(context, endpoint, MODEL, True)
        worker = context['process']('worker-ci-triage',
                                    [context['binary'], 'worker', '--concurrency', '2' if self.parallel else '1'])
        draft = request('/api/v1/agents', {
            'name': 'CI triage pilot', 'instructions': SYSTEM_PROMPT,
            'model': MODEL, 'tools': [], 'budget_microusd': 1000,
        })
        agent = request('/api/v1/agents/' + draft['id'] + '/publish', {'expected_version': draft['version']})

        def submit(goal):
            start = time.monotonic()
            task = request('/api/v1/tasks', {
                'goal': goal, 'agent_id': agent['id'], 'budget_microusd': 1000,
                'max_runs': 1, 'execution_timeout_seconds': 180,
            })
            return start, '/api/v1/tasks/' + task['id'], task

        samples = []
        pending = None
        parallel_proof = None
        for name, goal in goals.items():
            start, route, task = pending if pending and name == '03-ambiguous-timeout' else submit(goal)
            if self.parallel and name == '02-runner-disk':
                pending = submit(goals['03-ambiguous-timeout'])
                require(arrived[name].wait(10) and arrived['03-ambiguous-timeout'].wait(10),
                        'both workers did not reach the gate')
                foreground = request(route)
                background = request(pending[1])
                require(all(item['status'] == 'running' and item['attempt_count'] == 1 and item.get('current_run_id')
                            for item in (foreground, background))
                        and foreground['current_run_id'] != background['current_run_id'],
                        'two concurrent attempts were not observed')
                release[name].set()
            while task['status'] in ('queued', 'running') and worker.poll() is None and time.monotonic() - start < 90:
                time.sleep(0.2)
                task = request(route)
            result = {
                'case': name, 'input_sha256': sha(goal),
                'seconds_to_terminal_observation': round(time.monotonic() - start, 3),
                'task_status': task['status'], 'waiting_reason': task.get('waiting_reason'),
            }
            samples.append(result)
            if self.parallel:
                require(task['status'] == 'waiting_input', 'a parallel pilot draft was not produced')
            if task['status'] != 'waiting_input':
                continue
            require(task['goal'] == goal, 'effective task input differs')
            artifacts = request(route + '/artifacts')
            candidates = request(route + '/candidates')
            decisions = request(route + '/decisions')
            require(len(candidates) == 1 and candidates[0]['id'] == task['candidate_id'] and decisions == [],
                    'draft count, binding or unreviewed state differs')
            candidate = candidates[0]
            exact = [item for item in artifacts if item['id'] == candidate['artifact_id']
                     and item['version'] == candidate['artifact_version']]
            require(len(exact) == 1 and sha(exact[0]['content']) == candidate['content_digest'],
                    'draft text does not match its digest')
            artifact = exact[0]
            observed = artifact['provenance']
            costs = request(route + '/costs')
            require(observed['verified'] is False and observed['model_accounting_mode'] == 'operator_unmetered_local'
                    and task['spent_microusd'] == '0' and task['reserved_microusd'] == '0'
                    and costs['unknown_reservation_count'] == '1', 'local cost or draft provenance differs')
            result.update(draft=artifact['content'], draft_sha256=candidate['content_digest'],
                          input_tokens=observed['input_tokens'], output_tokens=observed['output_tokens'],
                          api_tariff_microusd='0', operator_accepted=False)
            if self.browser and name == '02-runner-disk':
                task, candidates, artifacts = self.repair_disk_draft(
                    context, route, task, candidate, artifact, costs, background if pending else None)
                if pending:
                    current = request(pending[1])
                    require(current['status'] == 'running' and not current.get('candidate_id')
                            and current['current_run_id'] == background['current_run_id']
                            and current['attempt_count'] == 1, 'background attempt changed during the decision')
                    parallel_proof = {'both_attempts_running_before_review': True,
                                      'background_still_running_after_review': True,
                                      'artificial_response_hold': True}
                    release['03-ambiguous-timeout'].set()
            if self.parallel and name == '03-ambiguous-timeout':
                require(task['current_run_id'] == background['current_run_id'] and task['attempt_count'] == 1,
                        'background result did not finish on the original attempt')
                parallel_proof['background_finished_on_same_attempt'] = True
            self.saved.append((route, task, candidates, artifacts, request(route + '/decisions'), request(route + '/costs')))
        if self.browser:
            require(self.repair is not None, 'repair case did not reach review')
        return {'scope': 'one_real_local_model_three_synthetic_inputs', 'model': MODEL,
                'model_digest': MODEL_DIGEST, 'engine_version': ENGINE_VERSION, 'system_prompt': SYSTEM_PROMPT,
                'max_output_tokens': profile['max_output_tokens'], 'samples': samples,
                'operator_time_measured': False, 'automatic_acceptance': False,
                'scripted_workspace_repair': self.repair, 'parallel_execution': parallel_proof}

    def repair_disk_draft(self, context, route, original_task, original_candidate, original_artifact, before_costs, background):
        node, playwright, chromium = self.browser
        config = private_json(context['private'] / 'ci-repair-browser.json', {
            'base_url': context['base_url'], 'credentials': context['credentials'],
            'playwright': str(playwright), 'chromium': str(chromium), 'task_id': original_task['id'],
            'original': original_artifact['content'], 'corrected': CORRECTED_DISK_DRAFT,
            'background_task_id': background['id'] if background else None,
        })
        try:
            env = {key: context['env'][key] for key in ('PATH', 'HOME', 'TMPDIR', 'LANG')}
            raw = context['record']('ci_repair_through_installed_workspace',
                                    [node, Path(__file__).with_name('ci_repair_browser.mjs'), config], env)
            browser_result = json.loads(raw)
        finally:
            config.unlink()
        require(browser_result['passed'] and browser_result['mutations'] == 3, 'unexpected Workspace actions')
        request = context['request']
        task = request(route)
        candidates = request(route + '/candidates')
        artifacts = request(route + '/artifacts')
        costs = request(route + '/costs')
        decisions = request(route + '/decisions')
        accepted = next((candidate for candidate in candidates if candidate['id'] == task['candidate_id']), None)
        require(task['status'] == 'succeeded' and task['result'] == CORRECTED_DISK_DRAFT
                and task['current_run_id'] == original_task['current_run_id'] and task['attempt_count'] == 1,
                'review did not accept the exact repair on the original run')
        require(len(candidates) == 2 and original_candidate in candidates and original_artifact in artifacts
                and accepted is not None and accepted['source'] == 'material'
                and accepted['review_basis_candidate_id'] == original_candidate['id']
                and accepted['content_digest'] == sha(CORRECTED_DISK_DRAFT), 'original or corrected draft changed')
        require(len(decisions) == 1 and decisions[0]['candidate_id'] == accepted['id']
                and decisions[0]['content_digest'] == accepted['content_digest'], 'decision binding differs')
        require(all(costs[key] == before_costs[key] for key in
                    ('spent_microusd', 'reserved_microusd', 'unknown_reservation_count')), 'repair changed accounting')
        self.repair = {
            'case': '02-runner-disk', 'original_digest': original_candidate['content_digest'],
            'accepted_digest': accepted['content_digest'], 'same_model_attempt': True,
            'original_preserved': True, 'accounting_unchanged': True, **browser_result,
        }
        return task, candidates, artifacts

    def verify_restart(self, request):
        for route, task, candidates, artifacts, decisions, costs in self.saved:
            require(request(route) == task and request(route + '/candidates') == candidates
                    and request(route + '/artifacts') == artifacts and request(route + '/decisions') == decisions
                    and request(route + '/costs') == costs, 'restart changed pilot state')
        return len(self.saved) == 3


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle', type=Path, required=True)
    parser.add_argument('--manifest-sha256', required=True)
    parser.add_argument('--ollama-port', type=int, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--browser-node', type=Path)
    parser.add_argument('--playwright', type=Path)
    parser.add_argument('--chromium', type=Path)
    parser.add_argument('--exercise-parallel', action='store_true')
    args = parser.parse_args()
    try:
        require(1 <= args.ollama_port <= 65535, 'invalid loopback port')
        base = 'http://127.0.0.1:%d' % args.ollama_port
        model_identity(base)
        browser_paths = (args.browser_node, args.playwright, args.chromium)
        require(not any(browser_paths) or all(browser_paths), 'provide all three explicit browser paths')
        require(not args.exercise_parallel or all(browser_paths), 'parallel scenario requires the browser')
        browser = tuple(path.resolve(strict=True) for path in browser_paths) if all(browser_paths) else None
        report = verify(args.bundle.resolve(), args.manifest_sha256, args.evidence.resolve(),
                        extra_probe=CITriageProbe(base + '/v1/chat/completions', browser, args.exercise_parallel))
    except Exception as error:
        raise SystemExit(str(error) if isinstance(error, RuntimeError) else 'CI triage verification failed: ' + type(error).__name__) from None
    probe = report['additional_probe']
    print(json.dumps({'first_drafts': sum('draft' in sample for sample in probe['samples']),
                      'workspace_repair_verified': probe['scripted_workspace_repair'] is not None,
                      'parallel_execution_verified': probe['parallel_execution'] is not None,
                      'restart_verified': probe['restart_verified']}))
    if not probe['restart_verified']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()

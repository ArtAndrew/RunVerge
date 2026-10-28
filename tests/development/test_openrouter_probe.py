# SPDX-License-Identifier: Elastic-2.0
"""Check the probe against the public Task DTO and allowed review lifecycle."""
import json
import hashlib
from pathlib import Path
import sys
import tempfile
import unittest
from urllib.error import HTTPError

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from verify_openrouter_free import FreeProbe, failure_details


class ReachedTask(Exception):
    pass


class OpenRouterProbeContract(unittest.TestCase):
    def test_failure_diagnostics_copy_only_fixed_codes(self):
        with tempfile.TemporaryDirectory() as private:
            log = Path(private) / 'worker.log'
            log.write_text('private-key-and-body-sentinel gateway_code=provider_rejected provider_status=403\n')
            value = failure_details({'status': 'blocked', 'waiting_reason': 'model_outcome_unknown', 'goal': 'private-key-and-body-sentinel'}, log)
            self.assertIn('gateway_code=provider_rejected', value)
            self.assertIn('provider_status=403', value)
            self.assertNotIn('private-key', value)

    def test_task_uses_public_max_runs_and_worker_alone_receives_key(self):
        environment, task_payload, stopped = {}, [], []
        key = 'synthetic-not-a-real-key-' + 'x' * 32
        worker = object()

        def process(name, args):
            self.assertEqual(environment.get('PLATFORM_MODEL_KEY_OPENROUTER'), key)
            return worker

        def request(route, payload=None):
            self.assertNotIn('PLATFORM_MODEL_KEY_OPENROUTER', environment)
            if route == '/api/v1/agents':
                return {'id': 'agt_test', 'version': 1}
            if route == '/api/v1/agents/agt_test/publish':
                return {'id': 'agt_test', 'version': 1}
            if route == '/api/v1/tasks':
                task_payload.append(payload)
                raise ReachedTask()
            self.fail('Unexpected probe request')

        with tempfile.TemporaryDirectory() as private:
            context = {'request': request, 'record': lambda *args: None,
                'binary': Path('/unused/platform'), 'private': Path(private),
                'credentials': {'organization_id': 'org_test'}, 'worker': object(),
                'stop_child': stopped.append, 'env': environment, 'process': process}
            with self.assertRaises(ReachedTask):
                FreeProbe(key, 'example/fixed-model:free', 'deny')(context)
        schema = json.loads((ROOT / 'contracts/domain.schema.json').read_text())['$defs']['CreateTask']
        self.assertEqual(len(task_payload), 1)
        self.assertLessEqual(set(task_payload[0]), set(schema['properties']))
        self.assertLessEqual(set(schema['required']), set(task_payload[0]))
        self.assertEqual(task_payload[0]['max_runs'], 2)
        self.assertEqual(task_payload[0]['execution_timeout_seconds'], 240)
        self.assertNotIn('PLATFORM_MODEL_KEY_OPENROUTER', environment)
        self.assertIs(stopped[-1], worker)

    def test_review_can_close_old_run_but_cannot_create_execution(self):
        for unapproved in (False, True):
            with self.subTest(unapproved_run=unapproved), tempfile.TemporaryDirectory() as private:
                state = {'count': 1, 'status': 'waiting_input', 'version': 1}
                runs = [{'id': 'run_1', 'status': 'waiting_input'}]
                def task():
                    return {'id': 'tsk_test', 'status': state['status'], 'version': state['version'],
                        'candidate_id': 'candidate_' + str(state['count']), 'spent_microusd': '0',
                        'reserved_microusd': str(state['count'])}
                def candidates():
                    return [{'id': 'candidate_' + str(i), 'artifact_id': 'artifact_' + str(i),
                        'artifact_version': 1, 'content_digest': hashlib.sha256(('Public synthetic answer ' + str(i)).encode()).hexdigest()}
                        for i in range(1, state['count'] + 1)]
                def request(route, payload=None):
                    if route == '/api/v1/agents' or route.endswith('/agt_test/publish'):
                        return {'id': 'agt_test', 'version': 1}
                    if route == '/api/v1/tasks' or route == '/api/v1/tasks/tsk_test':
                        return task()
                    if route == '/api/v1/models':
                        return []
                    if route.endswith('/candidates'):
                        return candidates()
                    if route.endswith('/artifacts'):
                        return [{'id': 'artifact_' + str(i), 'version': 1,
                            'content': 'Public synthetic answer ' + str(i),
                            'provenance': {'estimated_microusd': '0', 'provider_request_id': 'gen_test_' + str(i)}}
                            for i in range(1, state['count'] + 1)]
                    if route.endswith('/costs'):
                        return {'unknown_reservation_count': str(state['count'])}
                    if route.endswith('/runs'):
                        return [dict(item) for item in runs]
                    if route.endswith('/review'):
                        state['version'] += 1
                        if payload['decision'] == 'request_changes':
                            state['status'] = 'blocked'
                            runs[0]['status'] = 'failed'  # The documented review transition.
                            if unapproved:
                                runs.append({'id': 'run_unapproved', 'status': 'queued'})
                        else:
                            state['status'] = 'succeeded'
                            runs[-1]['status'] = 'succeeded'
                        return task()
                    if route.endswith('/retry'):
                        if not payload['acknowledge_new_cost']:
                            raise HTTPError('http://fixture.invalid', 400, 'Consent required', {}, None)
                        state.update(count=2, status='waiting_input', version=3)
                        runs.append({'id': 'run_2', 'status': 'waiting_input'})
                        return task()
                    if route.endswith('/decisions'):
                        return []
                    self.fail('Unexpected probe route')
                context = {'request': request, 'record': lambda *args: None, 'binary': Path('/unused/platform'),
                    'private': Path(private), 'credentials': {'organization_id': 'org_test'},
                    'worker': object(), 'stop_child': lambda *args: None, 'env': {}, 'process': lambda *args: object()}
                probe = FreeProbe('synthetic-not-a-real-key-' + 'x' * 32, 'example/fixed-model:free', 'allow')
                if unapproved:
                    with self.assertRaisesRegex(RuntimeError, 'Feedback created a new run'):
                        probe(context)
                else:
                    report = probe(context)
                    self.assertTrue(report['passed'])
                    self.assertEqual(report['successful_candidates'], 2)
                    self.assertTrue(probe.verify_restart(request))

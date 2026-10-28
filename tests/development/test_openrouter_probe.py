# SPDX-License-Identifier: Elastic-2.0
"""Check the actual probe's Task payload against the generated public DTO."""
import json
from pathlib import Path
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from verify_openrouter_free import FreeProbe


class ReachedTask(Exception):
    pass


class OpenRouterProbeContract(unittest.TestCase):
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

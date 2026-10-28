# SPDX-License-Identifier: Apache-2.0
"""Run public examples as real processes against synthetic HTTP, not a fake SDK."""
import copy
import json
import os
from pathlib import Path
import queue
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from urllib.parse import urlsplit, parse_qs
from test_client import ContractServer

ROOT = Path(__file__).resolve().parents[2]
F = json.loads(Path(__file__).with_name('configuration-fixtures.json').read_text())


class ConfigurationExampleTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ContractServer()
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.node = os.getenv('WORKSPACE_NODE') or shutil.which('node')

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown(); cls.server.server_close(); cls.thread.join()

    def tearDown(self):
        self.assertTrue(self.server.requests.empty()); self.assertTrue(self.server.responses.empty())

    def reply(self, data, cursor=None, status=200):
        body = dict(data=data)
        if cursor: body['next_cursor'] = cursor
        self.server.responses.put((status, body, {}))

    def run_example(self, language, *arguments):
        self.assertIsNotNone(self.node, 'Node 24 is required for the public TypeScript example')
        command = [sys.executable, str(ROOT / 'examples/python/configuration.py')] if language == 'python' else [self.node, str(ROOT / 'examples/typescript/configuration.ts')]
        env = dict(os.environ, PYTHONPATH=str(ROOT / 'packages/python-sdk/src'), AGENT_PLATFORM_TOKEN='configuration-example-test-only', AGENT_PLATFORM_ORGANIZATION_ID='org')
        with tempfile.TemporaryDirectory(prefix='configuration-example-cwd-') as cwd:
            result = subprocess.run([*command, '--url', f'http://127.0.0.1:{self.server.server_port}', *arguments], cwd=cwd, env=env, text=True, capture_output=True, timeout=15)
        self.assertNotIn('configuration-example-test-only', result.stdout + result.stderr)
        requests = []
        while True:
            try: requests.append(self.server.requests.get_nowait())
            except queue.Empty: break
        return result, requests

    def test_default_is_readonly_in_both_real_processes_and_keeps_decimal_metadata_exact(self):
        for language in ('python', 'typescript'):
            with self.subTest(language=language):
                self.reply(F['catalog']); self.reply([F['destination']], 'opaque+/=')
                result, requests = self.run_example(language)
                self.assertEqual(result.returncode, 0, result.stderr)
                output = json.loads(result.stdout)
                self.assertEqual(output['catalog']['revision'], '9007199254740993')
                self.assertEqual(output['destinations'][0]['max_verify_charge_microusd'], '1000')
                self.assertEqual(output['next_cursor'], 'opaque+/=')
                self.assertEqual([r['method'] for r in requests], ['GET', 'GET'])
                self.assertTrue(all('Idempotency-Key' not in r['headers'] for r in requests))

    def test_explicit_plan_mode_pages_exact_pins_then_creates_and_validates_only(self):
        plan = copy.deepcopy(F['plan']); plan['grant_id'] = 'grant'; plan['operations'][0]['id'] = 'setup'; plan['operations'][0]['expected_resource_version'] = 7; plan['diff'][0].update(operation_id='setup', expected_version=7, new_version=8)
        validation = copy.deepcopy(F['validation']); validation['diff'] = plan['diff']
        for language in ('python', 'typescript'):
            with self.subTest(language=language):
                self.reply(F['catalog']); self.reply([], 'opaque+/='); self.reply([F['destination']]); self.reply(plan); self.reply(validation)
                result, requests = self.run_example(language, '--create-plan', 'destination', '--destination-version', '1', '--known-resource-version', '7', '--grant-id', 'grant', '--request-key', 'example-one', '--reason', 'Explicit static example plan')
                self.assertEqual(result.returncode, 0, result.stderr)
                output = json.loads(result.stdout)
                self.assertFalse(output['applied']); self.assertFalse(output['connection_checked'])
                self.assertEqual(output['obligations'], ['requires_approval'])
                self.assertEqual([r['method'] for r in requests], ['GET', 'GET', 'GET', 'POST', 'POST'])
                self.assertEqual(parse_qs(urlsplit(requests[2]['path']).query)['cursor'], ['opaque+/='])
                self.assertEqual(requests[3]['body'], dict(grant_id='grant', base_revision='9007199254740993', ttl_seconds=600, operations=[dict(id='setup', destination_id='destination', destination_version=1, expected_resource_version=7, active=True)], reason='Explicit static example plan'))
                self.assertEqual(requests[3]['headers']['Idempotency-Key'], 'example-one-plan')
                self.assertEqual(requests[4]['headers']['Idempotency-Key'], 'example-one-validate')
                self.assertEqual(urlsplit(requests[4]['path']).path, '/api/v1/configuration/plans/plan/validate')

    def test_invalid_plan_arguments_and_implicit_plan_options_do_no_HTTP(self):
        for language in ('python', 'typescript'):
            for arguments in (('--known-resource-version', '0'), ('--create-plan', 'destination'), ('--create-plan', 'destination', '--destination-version', '1', '--known-resource-version', '-1', '--request-key', 'bad', '--reason', 'Review')):
                with self.subTest(language=language, arguments=arguments):
                    result, requests = self.run_example(language, *arguments)
                    self.assertNotEqual(result.returncode, 0); self.assertEqual(requests, [])

    def test_lost_plan_response_stops_both_examples_without_validation_or_hidden_retry(self):
        for language in ('python', 'typescript'):
            with self.subTest(language=language):
                self.reply(F['catalog']); self.reply([F['destination']]); self.reply({}, status=0)
                result, requests = self.run_example(language, '--create-plan', 'destination', '--destination-version', '1', '--known-resource-version', '0', '--request-key', 'lost', '--reason', 'Explicit plan')
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual([r['method'] for r in requests], ['GET', 'GET', 'POST'])
                self.assertEqual(requests[-1]['headers']['Idempotency-Key'], 'lost-plan')
                self.assertIn('inspect', result.stderr)

    def test_not_found_pin_stops_at_twenty_pages_without_mutation(self):
        for language in ('python', 'typescript'):
            with self.subTest(language=language):
                self.reply(F['catalog'])
                for page in range(20): self.reply([], f'cursor-{page}')
                result, requests = self.run_example(language, '--create-plan', 'absent', '--destination-version', '1', '--known-resource-version', '0', '--request-key', 'bounded', '--reason', 'Find exact pin')
                self.assertNotEqual(result.returncode, 0); self.assertEqual(len(requests), 21)
                self.assertTrue(all(r['method'] == 'GET' for r in requests))


if __name__ == '__main__': unittest.main()

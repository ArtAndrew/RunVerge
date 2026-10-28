# SPDX-License-Identifier: Elastic-2.0
"""Exercise documented helper admission without credentials or business effects."""
from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
from types import SimpleNamespace
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[2]
VERSION = json.loads((ROOT / 'contracts/openapi.json').read_text())['info']['version']
JS_HELPERS = ('smoke-completion.mjs', 'smoke-automation-live.mjs',
              'smoke-automation-reopen.mjs', 'smoke-members-live.mjs')
PY_HELPERS = (('tests/sdk/run_membership_e2e.py', '--url'),
              ('tests/security/test_http_boundaries.py', '--base-url'),
              ('tests/security/test_inspection_boundaries.py', '--base-url'),
              ('tests/contracts/run_live.py', '--base-url'))


class LiveHelperPreflightTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='platform-helper-preflight-')
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.version = VERSION
        self.version_headers = [VERSION]
        self.redirect = False
        self.requests = []
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                fixture.requests.append((self.command, self.path, self.headers))
                if fixture.redirect:
                    self.send_response(302)
                    self.send_header('Location', '/must-not-follow')
                    self.end_headers()
                    return
                data = {'contract_version': fixture.version} if self.path == '/healthz' else {}
                wire = json.dumps({'data': data}).encode()
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(wire)))
                for value in fixture.version_headers:
                    self.send_header('X-Platform-API-Version', value)
                self.end_headers()
                self.wfile.write(wire)

            def do_POST(self):
                fixture.requests.append((self.command, self.path, self.headers))
                self.send_response(500)
                self.end_headers()

            def log_message(self, *_args): pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.close_server)
        self.origin = f'http://127.0.0.1:{self.server.server_port}'

    def close_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def execute(self, command):
        env = os.environ.copy()
        for name in ('AGENT_PLATFORM_TOKEN', 'DATABASE_URL', 'TEST_DATABASE_URL'):
            env.pop(name, None)
        env.update(AGENT_PLATFORM_URL=self.origin, WORKSPACE_URL=self.origin,
                   AGENT_PLATFORM_CREDENTIALS=str(self.directory / 'missing-credentials.json'),
                   WORKSPACE_BOOTSTRAP=str(self.directory / 'missing-credentials.json'),
                   WORKSPACE_PLAYWRIGHT_PATH=str(self.directory / 'missing-playwright'),
                   AGENT_PLATFORM_QA_OUTPUT=str(self.directory / 'must-not-create-output'),
                   PYTHONDONTWRITEBYTECODE='1', NO_PROXY='127.0.0.1')
        result = subprocess.run(command, cwd=self.directory, env=env,
                                capture_output=True, text=True, timeout=20)
        self.assertFalse((self.directory / 'must-not-create-output').exists())
        self.assertFalse((self.directory / 'missing-credentials.json').exists())
        self.assertTrue(all(method == 'GET' and path == '/healthz'
                            for method, path, _ in self.requests), 'Unexpected business read or mutation')
        self.assertTrue(all(headers.get('Authorization') is None for _, _, headers in self.requests))
        return result

    def node(self):
        executable = os.environ.get('NODE_BINARY') or shutil.which('node')
        self.assertIsNotNone(executable, 'Node.js24 is required for live helper preflight checks')
        return executable

    def assert_preflight_result(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(report['contract_version'], VERSION)
        self.assertEqual(report['mutations'], 0)
        self.assertIn('health version only', report['scope'])

    def test_javascript_helpers_current_version_without_credentials_browser_or_outputs(self):
        for name in JS_HELPERS:
            with self.subTest(helper=name):
                before = len(self.requests)
                result = self.execute([self.node(), str(ROOT / 'web/scripts' / name), '--preflight-only'])
                self.assert_preflight_result(result)
                self.assertEqual(len(self.requests), before + 1)

    def test_javascript_helpers_mismatch_stops_without_retry_or_credentials(self):
        self.version = 'unsupported-test-version'
        for name in JS_HELPERS:
            with self.subTest(helper=name):
                before = len(self.requests)
                result = self.execute([self.node(), str(ROOT / 'web/scripts' / name), '--preflight-only'])
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('no live fixture was created', result.stderr)
                self.assertEqual(len(self.requests), before + 1)

    def test_python_helpers_current_version_without_credentials_or_outputs(self):
        for name, option in PY_HELPERS:
            with self.subTest(helper=name):
                before = len(self.requests)
                self.assert_preflight_result(self.execute([sys.executable, str(ROOT / name),
                                                          option, self.origin, '--preflight-only']))
                self.assertEqual(len(self.requests), before + 1)

    def test_python_helpers_mismatch_stops_before_credentials_without_retry(self):
        self.version = 'unsupported-test-version'
        for name, option in PY_HELPERS:
            with self.subTest(helper=name):
                before = len(self.requests)
                result = self.execute([sys.executable, str(ROOT / name), option,
                                       self.origin, '--preflight-only'])
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('no live fixture was created', result.stderr)
                self.assertEqual(len(self.requests), before + 1)

    def test_health_redirect_is_not_followed(self):
        self.redirect = True
        commands = [[self.node(), str(ROOT / 'web/scripts' / JS_HELPERS[0]), '--preflight-only']]
        commands += [[sys.executable, str(ROOT / name), option, self.origin, '--preflight-only']
                     for name, option in PY_HELPERS]
        for command in commands:
            with self.subTest(helper=command[1]):
                before = len(self.requests)
                result = self.execute(command)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(len(self.requests), before + 1)

    def load_boundary(self):
        spec = importlib.util.spec_from_file_location('preflight_independent_boundary',
                    ROOT / 'tests/security/test_http_boundaries.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_stdlib_boundary_protected_request_has_exact_current_version(self):
        module = self.load_boundary()
        case = module.HTTPBoundaryTests('runTest')
        case.base = self.origin
        status, _, _ = case.request('GET', '/api/v1/me',
                                    dict(token='synthetic-only', organization_id='org-test'))
        self.assertEqual(status, 200)
        self.assertEqual(len(self.requests), 1)
        self.assertEqual(self.requests[0][2].get_all('X-Platform-API-Version'), [VERSION])
        for headers in ([VERSION, VERSION], [VERSION, 'other'], [], ['other']):
            with self.subTest(response_headers=headers):
                self.version_headers = headers
                before = len(self.requests)
                with self.assertRaisesRegex(RuntimeError, 'API response version'):
                    case.request('GET', '/api/v1/me', dict(token='synthetic-only', organization_id='org-test'))
                self.assertEqual(len(self.requests), before + 1, 'No automatic replay after incompatible response')

    def test_boundary_setup_version_gate_precedes_reading_credential_files(self):
        module = self.load_boundary()
        module.CONFIG = SimpleNamespace(base_url=self.origin,
                                        org_a=str(self.directory / 'missing-a.json'),
                                        org_b=str(self.directory / 'missing-b.json'))
        self.version = 'unsupported-test-version'
        with self.assertRaisesRegex(RuntimeError, 'no live fixture was created'):
            module.HTTPBoundaryTests.setUpClass()
        self.assertEqual(len(self.requests), 1)
        self.assertEqual(self.requests[0][1], '/healthz')


if __name__ == '__main__': unittest.main()

# SPDX-License-Identifier: Elastic-2.0
"""Read-only setup checks must fail before any live task or action is created."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / 'tests/sdk'))
spec = importlib.util.spec_from_file_location('development_live_runner', REPO / 'tests/sdk/run_platform_e2e.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class ReadOnlyFixturePreflight(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        build = self.root / 'packages/typescript-sdk/dist'
        build.mkdir(parents=True)
        (build / 'index.js').write_text('// preflight never executes this placeholder')
        fixtures = self.root / 'tests/fixtures'
        fixtures.mkdir(parents=True)
        (fixtures / 'tool-scenarios.json').write_bytes((REPO / 'tests/fixtures/tool-scenarios.json').read_bytes())
        self.credentials = self.root / 'credentials.json'
        self.credentials.write_text(json.dumps({'token': 'synthetic-preflight-token', 'organization_id': 'org_preflight'}))

    def execute(self, models, tools, version='0.14.0'):
        client = Mock()
        client.list_models.return_value = SimpleNamespace(data=models)
        client.tools.return_value = SimpleNamespace(data=tools)
        opener = Mock()
        opener.open.return_value = io.BytesIO(json.dumps({'data':{'contract_version':version}}).encode())
        output = io.StringIO()
        with patch.object(runner, 'ROOT', self.root), patch.object(runner, 'Client', return_value=client), patch.object(runner, 'build_opener', return_value=opener), patch.object(sys, 'argv', ['run_platform_e2e.py', '--credentials', str(self.credentials), '--preflight-only']), contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            try:
                result = runner.main()
            except SystemExit as error:
                result = error.code
        self.assertEqual([c[0] for c in client.method_calls], ['list_models', 'tools'] if version == '0.14.0' else [])
        self.assertFalse((self.root / '.local').exists(), 'Read-only preflight must not create a report directory')
        self.assertNotIn('synthetic-preflight-token', output.getvalue())
        return result, output.getvalue()

    @staticmethod
    def catalog():
        return ([{'model': 'native-local-fixture', 'max_charge_microusd': 1000}], [
            {'id': 'tool_local_action_fixture', 'active': True, 'effect': 'write', 'max_charge_microusd': '17'},
            {'id': 'tool_local_read_fixture', 'active': True, 'effect': 'read', 'max_charge_microusd': '9', 'automation_allowed': True},
        ])

    def test_ready_preflight_only_reads_and_returns_explicit_scope(self):
        result, output = self.execute(*self.catalog())
        self.assertEqual(result, 0)
        self.assertEqual(json.loads(output)['mutations'], 0)
        self.assertIn('connectivity', json.loads(output)['scope'])

    def test_missing_inactive_or_incompatible_fixture_fails_before_mutations(self):
        for failure in ('missing_model', 'wrong_model_ceiling', 'missing_tool', 'inactive_tool', 'wrong_effect', 'missing_read_opt_in'):
            with self.subTest(failure=failure):
                models, tools = self.catalog()
                if failure == 'missing_model':
                    models = []
                elif failure == 'wrong_model_ceiling':
                    models[0]['max_charge_microusd'] = 1001
                elif failure == 'missing_tool':
                    tools.pop()
                elif failure == 'inactive_tool':
                    tools[0]['active'] = False
                elif failure == 'wrong_effect':
                    tools[1]['effect'] = 'write'
                else:
                    tools[1]['automation_allowed'] = False
                result, output = self.execute(models, tools)
                self.assertEqual(result, 2)
                self.assertIn('before creating live SDK records', output)

    def test_unsupported_server_revision_stops_before_catalog_or_mutations(self):
        for version in ('0.10.0', '', '0.14.0, 0.14.0'):
            with self.subTest(version=version):
                result, output = self.execute(*self.catalog(),version=version)
                self.assertEqual(result,2)
                self.assertIn('no fixture was created',output)


if __name__ == '__main__':
    unittest.main()

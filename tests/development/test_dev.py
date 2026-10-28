# SPDX-License-Identifier: Elastic-2.0
"""Exercise explicit fixture launch and credential boundaries without real keys."""
import contextlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts'))
import dev


class FixtureCredentials(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.private = Path(self.temp.name) / '.local'
        self.private.mkdir(mode=0o700)

    def write(self, name='model-fixture-secrets.json', value=None):
        path = self.private / name
        path.write_text(json.dumps(value or {'credential_environment': dev.FIXTURE_CREDENTIALS[name], 'credential': 'synthetic-test-value-' + 'a' * 32}))
        path.chmod(0o600)
        return path

    def test_three_exact_references_are_loaded_without_mutating_input_or_output(self):
        for filename in dev.FIXTURE_CREDENTIALS:
            self.write(filename)
        original = {'UNCHANGED': 'yes'}
        output = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            result = dev.fixture_environment(original, self.private)
        self.assertEqual(original, {'UNCHANGED': 'yes'})
        self.assertEqual(set(result), {'UNCHANGED', *dev.FIXTURE_CREDENTIALS.values()})
        self.assertEqual(output.getvalue(), '')

    def test_missing_directory_and_empty_directory_fail(self):
        for directory in (self.private / 'missing', self.private):
            with self.subTest(directory=directory.name), self.assertRaises(SystemExit):
                dev.fixture_environment({}, directory)

    def test_unknown_file_is_not_interpreted(self):
        self.write()
        (self.private / 'arbitrary-secrets.json').write_text('not JSON or shell input')
        self.assertEqual(len(dev.fixture_environment({}, self.private)), 1)

    def test_wrong_name_or_value_fails_without_echoing_material(self):
        for reference, credential in [('PATH', 'x' * 40), ('PLATFORM_MODEL_KEY_NATIVE_FIXTURE', 'x\ny' * 20), ('PLATFORM_MODEL_KEY_NATIVE_FIXTURE', 'tiny'), ('PLATFORM_MODEL_KEY_NATIVE_FIXTURE', 42), ('PLATFORM_MODEL_KEY_NATIVE_FIXTURE', 'x' * 513)]:
            with self.subTest(reference=reference, value_type=type(credential).__name__):
                self.write(value={'credential_environment': reference, 'credential': credential})
                with self.assertRaises(SystemExit) as raised:
                    dev.fixture_environment({}, self.private)
                if isinstance(credential, str):
                    self.assertNotIn(credential, str(raised.exception))

    def test_ambiguous_json_and_extra_fields_fail(self):
        path = self.write()
        for raw in ('{"credential_environment":"PLATFORM_MODEL_KEY_NATIVE_FIXTURE","credential":"' + 'a' * 40 + '","credential":"' + 'b' * 40 + '"}', '{"credential_environment":"PLATFORM_MODEL_KEY_NATIVE_FIXTURE","credential":"' + 'a' * 40 + '","command":"anything"}', '[]', '\ufffd', ' ' * 4097):
            with self.subTest(size=len(raw)):
                path.write_text(raw)
                with self.assertRaises(SystemExit):
                    dev.fixture_environment({}, self.private)

    def test_small_deeply_nested_configuration_is_rejected_with_controlled_error(self):
        path = self.write()
        path.write_text('[' * 1200 + '0' + ']' * 1200)
        with self.assertRaises(SystemExit) as raised:
            dev.fixture_environment({}, self.private)
        self.assertEqual(str(raised.exception), 'Invalid or conflicting private fixture credentials')

    def test_symlink_file_and_directory_are_rejected(self):
        path = self.write()
        target = self.private / 'original'
        path.rename(target)
        path.symlink_to(target)
        with self.assertRaises(SystemExit):
            dev.fixture_environment({}, self.private)
        link = Path(self.temp.name) / 'linked'
        link.symlink_to(self.private, target_is_directory=True)
        with self.assertRaises(SystemExit):
            dev.fixture_environment({}, link)

    def test_public_permissions_directory_file_and_fifo_are_rejected(self):
        path = self.write()
        self.private.chmod(0o755)
        with self.assertRaises(SystemExit):
            dev.fixture_environment({}, self.private)
        self.private.chmod(0o700)
        path.chmod(0o644)
        with self.assertRaises(SystemExit):
            dev.fixture_environment({}, self.private)
        path.unlink()
        os.mkfifo(path, 0o600)
        with self.assertRaises(SystemExit):
            dev.fixture_environment({}, self.private)

    def test_inherited_conflict_is_rejected_and_identical_value_is_allowed(self):
        path = self.write()
        value = json.loads(path.read_text())
        expected = value['credential_environment']
        with self.assertRaises(SystemExit):
            dev.fixture_environment({expected: 'another-value-' + 'b' * 40}, self.private)
        self.assertEqual(dev.fixture_environment({expected: value['credential']}, self.private)[expected], value['credential'])


class FixtureCommands(unittest.TestCase):
    def test_worker_child_receives_secret_in_environment_and_never_arguments(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            private = root / '.local'
            private.mkdir(mode=0o700)
            secret = private / 'model-fixture-secrets.json'
            secret.write_text(json.dumps({'credential_environment': 'PLATFORM_MODEL_KEY_NATIVE_FIXTURE', 'credential': 'synthetic-private-' + 'z' * 40}))
            secret.chmod(0o600)
            (root / 'bin').mkdir()
            binary = root / 'bin/platform'
            binary.write_text('#!' + sys.executable + '\n' + '''import json,os,pathlib,sys
assert sys.argv[1:]==['worker','--concurrency','2']
value=os.environ['PLATFORM_MODEL_KEY_NATIVE_FIXTURE']
assert len(value)==58 and all(value not in a for a in sys.argv)
pathlib.Path('child-result.json').write_text(json.dumps({'environment_received':True,'credential_in_arguments':False}))
''')
            binary.chmod(0o700)
            load = dev.fixture_environment
            with patch.object(sys, 'argv', ['dev.py', 'worker', '--fixture-credentials', '--concurrency', '2']), patch.object(dev, 'ROOT', root), patch.object(dev, 'environment', return_value={}), patch.object(dev, 'fixture_environment', side_effect=lambda env: load(env, private)):
                dev.main()
            self.assertEqual(json.loads((root / 'child-result.json').read_text()), {'environment_received': True, 'credential_in_arguments': False})

    def test_fixture_launch_has_fixed_script_and_repo_without_server_dependencies(self):
        for name, script in [('model', 'native_model_peer.py'), ('action', 'tool_action_peer.py'), ('read', 'read_lookup_peer.py')]:
            with self.subTest(name=name), patch.object(sys, 'argv', ['dev.py', 'fixture', name]), patch.object(dev, 'call') as call, patch.object(dev, 'environment', side_effect=AssertionError('must not need Go/PG')):
                dev.main()
                call.assert_called_once_with([sys.executable, dev.ROOT / 'tests/fixtures' / script, '--repo', dev.ROOT])

    def test_fixture_launch_rejects_unrecognized_inputs_before_spawning(self):
        for arguments in ([], ['anything'], ['model', '--repo', '/elsewhere'], ['model', ';echo']):
            with self.subTest(arguments=arguments), patch.object(sys, 'argv', ['dev.py', 'fixture', *arguments]), patch.object(dev, 'call') as call:
                with self.assertRaises(SystemExit):
                    dev.main()
                call.assert_not_called()

    def test_worker_is_opt_in_and_flag_is_not_forwarded_to_binary(self):
        for enabled in (False, True):
            arguments = ['dev.py', 'worker', '--concurrency', '2'] + (['--fixture-credentials'] if enabled else [])
            with self.subTest(enabled=enabled), patch.object(sys, 'argv', arguments), patch.object(dev, 'environment', return_value={'BASE': 'yes'}), patch.object(dev, 'fixture_environment', return_value={'LOADED': 'yes'}) as load, patch.object(dev, 'call') as call:
                dev.main()
                call.assert_called_once_with([dev.ROOT / 'bin/platform', 'worker', '--concurrency', '2'], {'LOADED': 'yes'} if enabled else {'BASE': 'yes'})
                self.assertEqual(load.call_count, int(enabled))

    def test_credentials_flag_is_rejected_outside_worker_or_if_duplicated(self):
        for arguments in (['serve', '--fixture-credentials'], ['install-tool', '--fixture-credentials'], ['worker', '--fixture-credentials', '--fixture-credentials']):
            with self.subTest(arguments=arguments), patch.object(sys, 'argv', ['dev.py', *arguments]), patch.object(dev, 'call') as call:
                with self.assertRaises(SystemExit):
                    dev.main()
                call.assert_not_called()


if __name__ == '__main__':
    unittest.main()

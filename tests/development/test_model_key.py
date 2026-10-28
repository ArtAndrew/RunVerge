# SPDX-License-Identifier: Elastic-2.0
import importlib.util
import os
from pathlib import Path
import stat
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('model_key', Path(__file__).resolve().parents[2] / 'scripts/model_key.py')
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


class ModelKeyTest(unittest.TestCase):
    def test_private_new_file_and_no_shell_injection(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'key.env'
            key = 'synthetic-not-a-real-credential-' + 'x' * 32
            helper.save_key(path, key)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            before = path.read_bytes()
            self.assertEqual(before, ('PLATFORM_MODEL_KEY_OPENROUTER=' + key + '\n').encode())
            with self.assertRaises(FileExistsError):
                helper.save_key(path, key)
            self.assertEqual(path.read_bytes(), before)
            for value in (key + '\n', key + ';echo', key + '$(id)', key + '"'):
                with self.assertRaises(ValueError):
                    helper.save_key(Path(root) / 'other.env', value)
            link = Path(root) / 'symlink.env'
            link.symlink_to(path)
            with self.assertRaises(FileExistsError):
                helper.save_key(link, key)
            os.chmod(root, 0o755)
            try:
                with self.assertRaises(ValueError):
                    helper.save_key(Path(root) / 'other.env', key)
            finally:
                os.chmod(root, 0o700)

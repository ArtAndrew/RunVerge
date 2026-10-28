# SPDX-License-Identifier: Elastic-2.0
import importlib.util
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('server_setup', Path(__file__).resolve().parents[2] / 'scripts/server_setup.py')
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


class ServerSetupTest(unittest.TestCase):
    def test_private_files_and_secret_free_result(self):
        with tempfile.TemporaryDirectory() as root:
            directory = Path(root) / 'private'
            result = setup.initialize(directory)
            password = (directory / 'db.env').read_text().strip().split('=', 1)[1]
            self.assertGreaterEqual(len(password), 32)
            self.assertNotIn(password, json.dumps(result))
            self.assertIn(password, (directory / 'runtime.env').read_text())
            self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o700)
            for name in result['files']:
                self.assertEqual(stat.S_IMODE((directory / name).stat().st_mode), 0o600)
            before = (directory / 'db.env').read_bytes()
            with self.assertRaises(FileExistsError):
                setup.initialize(directory)
            self.assertEqual((directory / 'db.env').read_bytes(), before)

    def test_refuses_symlink_and_shared_parent(self):
        with tempfile.TemporaryDirectory() as root:
            directory = Path(root) / 'private'
            directory.symlink_to(Path(root), target_is_directory=True)
            with self.assertRaises(FileExistsError):
                setup.initialize(directory)
            self.assertFalse((Path(root) / 'db.env').exists())
            directory.unlink()
            os.chmod(root, 0o777)
            try:
                with self.assertRaises(ValueError):
                    setup.initialize(directory)
            finally:
                os.chmod(root, 0o700)

# SPDX-License-Identifier: Elastic-2.0
import importlib.util
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest
from urllib.parse import urlparse

spec = importlib.util.spec_from_file_location('server_setup', Path(__file__).resolve().parents[2] / 'scripts/server_setup.py')
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


class ServerSetupTest(unittest.TestCase):
    def test_private_files_and_secret_free_result(self):
        with tempfile.TemporaryDirectory() as root:
            directory = Path(root) / 'private'
            result = setup.initialize(directory)
            db = dict(line.split('=', 1) for line in (directory / 'db.env').read_text().splitlines())
            admin = db['POSTGRES_PASSWORD']
            runtime_text = (directory / 'runtime.env').read_text()
            runtime = urlparse(runtime_text.strip().split('=', 1)[1]).password
            self.assertTrue(len(admin) >= 32 and len(runtime) >= 32 and admin != runtime)
            self.assertFalse(admin in runtime_text or runtime in (directory / 'db.env').read_text())
            self.assertFalse(admin in json.dumps(result) or runtime in json.dumps(result))
            self.assertTrue(db['RUNVERGE_APP_VERIFIER'].startswith("'SCRAM-SHA-256$4096:") and db['RUNVERGE_APP_VERIFIER'].endswith("'"))
            self.assertEqual(len(db['RUNVERGE_CREDENTIAL_PROFILE']), 32)
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

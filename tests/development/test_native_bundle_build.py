# SPDX-License-Identifier: Elastic-2.0
"""Check native builder isolation and no-overwrite before running build tools."""
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts'))
import build_native_bundle as bundle


class NativeBundleBuilder(unittest.TestCase):
    def test_build_environment_does_not_inherit_private_configuration_or_download_modules(self):
        values = {'DATABASE_URL': 'private-sentinel', 'TEST_DATABASE_URL': 'private-sentinel',
                  'PLATFORM_MODEL_KEY_REAL': 'private-sentinel', 'GITHUB_TOKEN': 'private-sentinel',
                  'HTTP_PROXY': 'private-sentinel', 'HTTPS_PROXY': 'private-sentinel',
                  'GOFLAGS': '-modfile=private-sentinel', 'NODE_OPTIONS': 'private-sentinel'}
        with patch.dict(os.environ, values), patch.object(bundle.dev, 'executable', return_value='/tools/selected-tool'):
            env = bundle.build_environment()
        self.assertFalse(set(values) & set(env))
        self.assertNotIn('private-sentinel', str(env))
        self.assertEqual(env['GOPROXY'], 'off')
        self.assertEqual(env['CGO_ENABLED'], '0')

    def test_existing_output_symlink_and_source_checkout_are_rejected_before_build(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(bundle, 'source_inputs') as inputs:
            existing = Path(tmp) / 'existing'; existing.mkdir()
            sentinel = existing / 'keep'; sentinel.write_text('preserve')
            link = Path(tmp) / 'link'; link.symlink_to(existing)
            for output in (existing, link, bundle.ROOT / 'new-output'):
                with self.subTest(output=output.name), self.assertRaises(RuntimeError):
                    bundle.build(output, '0.14.0-dev.test')
            self.assertEqual(sentinel.read_text(), 'preserve')
            inputs.assert_not_called()

    def test_missing_notices_and_linked_license_are_not_silently_distributed(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            with self.assertRaises(RuntimeError):
                bundle.license_files(root)
            outside = root / 'original'; outside.write_text('license')
            (root / 'LICENSE').symlink_to(outside)
            with self.assertRaises(RuntimeError):
                bundle.license_files(root)

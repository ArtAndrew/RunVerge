# SPDX-License-Identifier: Elastic-2.0
"""Prevent the local acceptance helper from routing to a cloud/paid endpoint."""
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts'))
from verify_local_model import local_probe
from verify_native_bundle import cleanup_owned


class LocalModelProbeBoundary(unittest.TestCase):
    def test_only_explicit_numeric_loopback_chat_endpoint_is_accepted(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / 'response.txt'
            accepted = local_probe('http://127.0.0.1:12345/v1/chat/completions', 'qwen2.5:1.5b', output)
            self.assertTrue(callable(accepted))
            self.assertFalse(output.exists())
            for endpoint in ('https://api.example.com/v1/chat/completions',
                             'http://localhost:12345/v1/chat/completions',
                             'http://127.0.0.1.evil.invalid:12345/v1/chat/completions',
                             'http://127.0.0.1:12345/v1/chat/completions?forward=cloud',
                             'http://127.0.0.1:12345/v1/chat/completions?',
                             'http://127.0.0.1:12345/v1/chat/completions#',
                             'http://@127.0.0.1:12345/v1/chat/completions',
                             ' http://127.0.0.1:12345/v1/chat/completions',
                             'http://127.0.0.1:012345/v1/chat/completions',
                             'http://user:private@127.0.0.1:12345/v1/chat/completions',
                             'http://127.0.0.1:0/v1/chat/completions',
                             'http://127.0.0.1:12345/other'):
                with self.subTest(endpoint=endpoint), self.assertRaises((RuntimeError, ValueError)):
                    local_probe(endpoint, 'qwen2.5:1.5b', output)
            self.assertFalse(output.exists())

    def test_cloud_model_and_existing_evidence_are_rejected_before_any_action(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / 'response.txt'
            for model in ('qwen2.5:cloud', 'other-model', ''):
                with self.subTest(model=model), self.assertRaises(RuntimeError):
                    local_probe('http://127.0.0.1:12345/v1/chat/completions', model, output)
            output.write_text('existing evidence')
            with self.assertRaises(RuntimeError):
                local_probe('http://127.0.0.1:12345/v1/chat/completions', 'qwen2.5:1.5b', output)
            self.assertEqual(output.read_text(), 'existing evidence')

    def test_revision_will_not_overwrite_an_existing_initial_draft(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / 'response.txt'
            initial = Path(tmp) / 'response.initial.txt'
            initial.write_text('preserve initial draft')
            with self.assertRaises(RuntimeError):
                local_probe('http://127.0.0.1:12345/v1/chat/completions', 'qwen2.5:1.5b', output, True)
            self.assertEqual(initial.read_text(), 'preserve initial draft')
            self.assertFalse(output.exists())


class OwnedCleanupBoundary(unittest.TestCase):
    def test_child_shutdown_error_does_not_skip_other_child_or_ambiguous_database_start(self):
        healthy = Mock()
        healthy.poll.side_effect = [None, 0]
        broken = Mock()
        broken.poll.return_value = None
        broken.terminate.side_effect = OSError('unconfirmed child')
        with patch('verify_native_bundle.subprocess.run', side_effect=[Mock(returncode=0), Mock(returncode=0), Mock(returncode=3)]) as run:
            complete, errors = cleanup_owned([healthy, broken], '/owned/pg_ctl', Path('/owned/cluster'), {}, True, [])
        self.assertFalse(complete)
        self.assertIn('owned_child_shutdown_unconfirmed', errors)
        healthy.terminate.assert_called_once()
        self.assertEqual([call.args[0][-1] for call in run.call_args_list], ['status', 'stop', 'status'])

    def test_database_status_error_does_not_authorize_directory_removal(self):
        with patch('verify_native_bundle.subprocess.run', return_value=Mock(returncode=4)) as run:
            complete, errors = cleanup_owned([], '/owned/pg_ctl', Path('/owned/cluster'), {}, True, [])
        self.assertFalse(complete)
        self.assertIn('owned_postgres_shutdown_unconfirmed', errors)
        self.assertEqual(run.call_count, 1)

    def test_confirmed_stopped_database_and_unstarted_cluster_are_safe_to_clean(self):
        with patch('verify_native_bundle.subprocess.run', return_value=Mock(returncode=3)) as run:
            self.assertEqual(cleanup_owned([], '/owned/pg_ctl', Path('/owned/cluster'), {}, True, []), (True, []))
            self.assertEqual(cleanup_owned([], '/owned/pg_ctl', Path('/owned/cluster'), {}, False, []), (True, []))
        self.assertEqual(run.call_count, 1)

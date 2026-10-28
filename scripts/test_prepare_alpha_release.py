# SPDX-License-Identifier: Elastic-2.0
"""Author checks; opt-in native cases use an explicitly selected local build."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import prepare_alpha_release as release


class MetadataFiles(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='runverge-metadata-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.contents = {'SHA256SUMS': b'fixture\n', 'release-manifest.json': b'{}\n',
                         'validation-report.json': b'{}\n'}

    def test_marker_last_on_partial_write_failure_and_no_reuse(self):
        output = self.root / 'metadata'
        original = release.write_file

        def fail(fd, name, raw):
            if name == 'validation-report.json':
                raise OSError('author injected disk failure')
            original(fd, name, raw)

        with patch.object(release, 'write_file', side_effect=fail), self.assertRaises(OSError):
            release.write_metadata(output, self.contents)
        self.assertTrue(output.is_dir())
        self.assertFalse((output / 'complete.json').exists())
        with self.assertRaises(FileExistsError):
            release.write_metadata(output, self.contents)
        with self.assertRaises(release.ReleaseError):
            release.verify_metadata(output, self.contents)

    def test_output_verification_rejects_corruption_extra_file_and_link(self):
        for alteration in ('content', 'extra', 'symlink', 'missing_marker'):
            with self.subTest(alteration=alteration):
                output = self.root / alteration
                release.write_metadata(output, self.contents)
                if alteration == 'content':
                    (output / 'SHA256SUMS').write_bytes(b'altered\n')
                elif alteration == 'extra':
                    (output / 'private.json').write_text('not for publication')
                elif alteration == 'missing_marker':
                    (output / 'complete.json').unlink()
                else:
                    target = output / 'SHA256SUMS'
                    target.unlink()
                    target.symlink_to(self.root / 'missing')
                with self.assertRaises((release.ReleaseError, OSError)):
                    release.verify_metadata(output, self.contents)

    def test_fifo_and_directory_links_are_rejected_without_blocking(self):
        fifo = self.root / 'fifo'
        os.mkfifo(fifo)
        with release.directory(self.root) as fd, self.assertRaises(release.ReleaseError):
            release.read_file(fd, 'fifo', 1024)
        link = self.root / 'linked-root'
        link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(release.ReleaseError), release.directory(link):
            self.fail('must not open linked root')

    def test_subprocess_environment_omits_private_variables_and_loader_hooks(self):
        completed = subprocess.CompletedProcess([], 0, b'{}', b'')
        with patch.dict(os.environ, {'DATABASE_URL': 'private-sentinel', 'GITHUB_TOKEN': 'private-sentinel',
                                     'DYLD_INSERT_LIBRARIES': 'private-sentinel', 'HTTPS_PROXY': 'private-sentinel'}), \
                patch.object(subprocess, 'run', return_value=completed) as run:
            release.invoke('/trusted/platform', ['version'])
        self.assertEqual(run.call_args.kwargs['env'], {'PATH': os.defpath, 'LANG': 'C', 'LC_ALL': 'C'})
        self.assertNotIn('private-sentinel', str(run.call_args))

    def test_error_does_not_reprint_verifier_output(self):
        completed = subprocess.CompletedProcess([], 2, b'private-sentinel', b'private-sentinel')
        with patch.object(subprocess, 'run', return_value=completed):
            with self.assertRaisesRegex(release.ReleaseError, '^Trusted native verifier rejected the candidate$'):
                release.invoke('/trusted/platform', ['version'])

    def test_existing_or_linked_output_is_not_overwritten(self):
        existing = self.root / 'existing'; existing.mkdir()
        sentinel = existing / 'preserve'; sentinel.write_bytes(b'original')
        linked = self.root / 'linked'; linked.symlink_to(existing, target_is_directory=True)
        for output in (existing, linked):
            with self.subTest(kind=output.name), self.assertRaises(FileExistsError):
                release.write_metadata(output, self.contents)
        self.assertEqual(sentinel.read_bytes(), b'original')

    def test_malformed_source_json_values_fail_with_fixed_errors(self):
        identity = {'source_sha256': '0' * 64, 'migrations': []}
        for value in ([], None, {}, {'a.py': 7}, {'a.py': []}, {'../escape': '0' * 64},
                      {'private/key.json': '0' * 64}, {'a.py': 'not-a-digest'}):
            with self.subTest(kind=type(value).__name__), self.assertRaises(release.ReleaseError):
                release.validate_sources(json.dumps(value).encode(), identity)
        with self.assertRaises(release.ReleaseError):
            release.strict_json(b'{"duplicate":1,"duplicate":2}')

    def test_malformed_component_types_fail_with_fixed_errors(self):
        base = {'format': 'runverge-component-inventory-v1', 'scope': 'linked-server-and-production-web',
                'standard_sbom': False, 'build_tools': [], 'components': [
                    {'kind': 'runtime', 'name': 'Go', 'version': 'go1.27.3',
                     'license_declared': 'BSD-3-Clause', 'notices': ['notices/000/LICENSE']}]}
        for field, value in (('name', []), ('version', None), ('kind', {}), ('notices', [7]),
                             ('notices', [['nested']]), ('notices', None), ('license_declared', {})):
            data = json.loads(json.dumps(base)); data['components'][0][field] = value
            with self.subTest(field=field, value_type=type(value).__name__), self.assertRaises(release.ReleaseError):
                release.validate_components(json.dumps(data).encode(), {'notices/000/LICENSE'})


@unittest.skipUnless(os.environ.get('RUNVERGE_TEST_BUNDLE') and os.environ.get('RUNVERGE_TEST_MANIFEST_SHA256'),
                     'requires explicitly trusted local native bundle and manifest SHA256')
class NativeCandidate(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.original = Path(os.environ['RUNVERGE_TEST_BUNDLE'])
        cls.manifest_sha = os.environ['RUNVERGE_TEST_MANIFEST_SHA256']
        cls.verifier = cls.original / 'bin/platform'
        if hashlib.sha256((cls.original / 'bundle.json').read_bytes()).hexdigest() != cls.manifest_sha:
            raise RuntimeError('Explicit fixture manifest checksum mismatch')
        manifest = json.loads((cls.original / 'bundle.json').read_bytes())
        cls.verifier_sha = next(entry['sha256'] for entry in manifest['files'] if entry['path'] == 'bin/platform')
        if hashlib.sha256(cls.verifier.read_bytes()).hexdigest() != cls.verifier_sha:
            raise RuntimeError('Pinned fixture executable checksum mismatch')
        cls.version = manifest['version']
        cls.api = manifest['identity']['api_version']

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='runverge-release-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bundle = self.root / 'bundle'
        shutil.copytree(self.original, self.bundle)
        self.output = self.root / 'metadata'
        self.current_sha = self.manifest_sha

    def call(self, mode='prepare', **overrides):
        options = dict(mode=mode, bundle=self.bundle, verifier=self.verifier, verifier_sha=self.verifier_sha,
                       manifest_sha=self.current_sha, version=self.version, api_version=self.api,
                       target_os='darwin', arch='arm64', output=self.output)
        options.update(overrides)
        return release.prepare(**options)

    def update_manifest(self, change):
        path = self.bundle / 'bundle.json'
        manifest = json.loads(path.read_bytes())
        change(manifest)
        raw = release.dump(manifest)
        path.write_bytes(raw)
        self.current_sha = hashlib.sha256(raw).hexdigest()

    def rehash_file(self, name):
        raw = (self.bundle / name).read_bytes()
        def update(manifest):
            for entry in manifest['files']:
                if entry['path'] == name:
                    entry['sha256'] = hashlib.sha256(raw).hexdigest()
                    entry['size'] = len(raw)
        self.update_manifest(update)

    def test_exact_bundle_metadata_is_reproducible_and_independently_checksummed(self):
        first = self.call()
        other = self.root / 'metadata-two'
        second = self.call(output=other)
        self.assertEqual(first, second)
        self.assertEqual(set(os.listdir(self.output)), release.OUTPUT_NAMES)
        for name in release.OUTPUT_NAMES:
            self.assertEqual((self.output / name).read_bytes(), (other / name).read_bytes())
        for line in (self.output / 'SHA256SUMS').read_text().splitlines():
            digest, name = line.split('  ', 1)
            self.assertEqual(hashlib.sha256((self.bundle / name).read_bytes()).hexdigest(), digest)
        validation = json.loads((self.output / 'validation-report.json').read_bytes())
        for field in ('standard_sbom', 'publisher_authenticated', 'complete_secret_scan', 'release_ready'):
            self.assertIs(validation[field], False)
        self.assertNotIn(str(self.root), ''.join(item.read_text() for item in self.output.iterdir()))
        self.assertEqual(self.call(mode='verify')['result'], 'passed')

    def test_tampered_payload_rejected_and_candidate_binary_never_executed(self):
        sentinel = self.root / 'executed'
        (self.bundle / 'bin/platform').write_text('#!/bin/sh\ntouch "' + str(sentinel) + '"\n')
        with self.assertRaises(release.ReleaseError):
            self.call()
        self.assertFalse(sentinel.exists())
        self.assertFalse(self.output.exists())

    def test_wrong_version_api_platform_or_checksums_rejected(self):
        for overrides in ({'version': '0.14.0-alpha.wrong'}, {'api_version': '0.15.0'},
                          {'target_os': 'linux'}, {'arch': 'amd64'},
                          {'manifest_sha': '0' * 64}, {'verifier_sha': '0' * 64}):
            with self.subTest(overrides=overrides), self.assertRaises(release.ReleaseError):
                self.call(**overrides)
            self.assertFalse(self.output.exists())

    def test_manifest_traversal_is_rejected_by_real_native_verifier(self):
        self.update_manifest(lambda manifest: manifest['files'][0].update(path='../escaped'))
        with self.assertRaises(release.ReleaseError):
            self.call()
        self.assertFalse(self.output.exists())

    def test_linked_payload_rejected_even_with_identical_bytes(self):
        original = self.bundle / 'RUNNING.md'
        outside = self.root / 'same-bytes'; outside.write_bytes(original.read_bytes()); outside.chmod(0o644)
        original.unlink(); original.symlink_to(outside)
        with self.assertRaises(release.ReleaseError):
            self.call()
        self.assertFalse(self.output.exists())

    def test_unlisted_private_file_rejected(self):
        (self.bundle / '.env').write_text('private-sentinel')
        with self.assertRaises(release.ReleaseError):
            self.call()
        self.assertFalse(self.output.exists())

    def test_even_manifest_listed_private_payload_is_rejected(self):
        raw = b'private-sentinel'
        (self.bundle / 'private.json').write_bytes(raw)
        (self.bundle / 'private.json').chmod(0o644)
        def add(manifest):
            manifest['files'].append({'path': 'private.json', 'sha256': hashlib.sha256(raw).hexdigest(),
                                      'size': len(raw), 'mode': 0o644})
            manifest['files'].sort(key=lambda item: item['path'])
        self.update_manifest(add)
        with self.assertRaisesRegex(release.ReleaseError, 'Unsupported or private development payload'):
            self.call()
        self.assertFalse(self.output.exists())

    def test_component_inventory_semantic_inconsistency_rejected(self):
        path = self.bundle / 'components.json'
        inventory = json.loads(path.read_bytes())
        inventory['components'][0]['notices'] = ['notices/999/LICENSE']
        path.write_bytes(release.dump(inventory)); self.rehash_file('components.json')
        with self.assertRaisesRegex(release.ReleaseError, 'notice reference'):
            self.call()

    def test_source_inventory_semantic_inconsistency_rejected(self):
        path = self.bundle / 'source-files.json'
        inventory = json.loads(path.read_bytes())
        inventory[next(iter(inventory))] = '0' * 64
        path.write_bytes(release.dump(inventory)); self.rehash_file('source-files.json')
        with self.assertRaisesRegex(release.ReleaseError, 'Source inventory does not match'):
            self.call()

    def test_verifier_original_swap_after_copy_cannot_change_executed_bytes(self):
        trusted = self.root / 'trusted'; trusted.mkdir()
        original = trusted / 'platform'; shutil.copyfile(self.verifier, original); original.chmod(0o755)
        with release.trusted_verifier(original, self.verifier_sha) as executable:
            original.write_bytes(b'no longer executable code')
            self.assertEqual(hashlib.sha256(executable.read_bytes()).hexdigest(), self.verifier_sha)
            self.assertEqual(release.invoke(executable, ['version'])['version'], self.version)

    def test_output_inside_candidate_is_rejected_without_writes(self):
        with self.assertRaises(release.ReleaseError):
            self.call(output=self.bundle / 'release')
        self.assertFalse((self.bundle / 'release').exists())

    def test_cli_malformed_inventory_returns_fixed_error_without_path_or_traceback(self):
        path = self.bundle / 'components.json'
        inventory = json.loads(path.read_bytes())
        inventory['components'][0]['notices'] = [42]
        path.write_bytes(release.dump(inventory)); self.rehash_file('components.json')
        result = subprocess.run([sys.executable, '-B', str(Path(release.__file__)), 'prepare',
                                 '--bundle', str(self.bundle), '--verifier', str(self.verifier),
                                 '--verifier-sha256', self.verifier_sha, '--manifest-sha256', self.current_sha,
                                 '--version', self.version, '--api-version', self.api,
                                 '--os', 'darwin', '--arch', 'arm64', '--output', str(self.output)],
                                capture_output=True, text=True, env={'PATH': os.defpath, 'LANG': 'C'})
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, '')
        self.assertEqual(result.stderr, 'Invalid or duplicated notice reference\n')
        self.assertNotIn(str(self.root), result.stderr)
        self.assertNotIn('Traceback', result.stderr)


if __name__ == '__main__':
    unittest.main(verbosity=2)

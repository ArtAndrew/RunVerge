#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Prepare or verify offline metadata for an explicitly trusted native candidate.

This is a release-operator helper, not an installer, publisher or signing tool.
SHA256SUMS paths are relative to the input bundle, not the metadata directory.
"""
import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile


FORMAT = 'runverge-alpha-release-metadata-v1'
HASH = re.compile(r'[a-f0-9]{64}\Z')
VERSION = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+-[A-Za-z0-9][A-Za-z0-9.-]{0,63}\Z')
COMPONENT = re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z')
SOURCE_PART = re.compile(r'[A-Za-z0-9_][A-Za-z0-9._-]{0,127}\Z')
FIXED_PAYLOAD = {'LICENSE', 'LICENSING.md', 'LOCAL-MODELS.md', 'RUNNING.md',
                 'bin/platform', 'components.json', 'source-files.json', 'web/index.html'}
OPTIONAL_GUIDES = {'QUICKSTART-ALPHA.md', 'MCP-READ.md', 'OPENROUTER.md', 'helpers/model_key.py'}
ASSET = re.compile(r'web/assets/[A-Za-z0-9][A-Za-z0-9_-]{0,127}-[A-Za-z0-9_-]{6,64}\.(js|css)\Z')
NOTICE = re.compile(r'notices/[0-9]{3}/(?:LICENSE|LICENCE|COPYING|NOTICE)(?:[._-][A-Za-z0-9._-]+)?\Z', re.I)
PRIVATE_PARTS = {'.git', '.local', '.env', 'node_modules', 'work', 'private', 'secrets', '__pycache__'}
OUTPUT_NAMES = {'SHA256SUMS', 'release-manifest.json', 'validation-report.json', 'complete.json'}
MAX_JSON = 1024 * 1024
MAX_EXECUTABLE = 256 * 1024 * 1024


class ReleaseError(RuntimeError):
    """Only fixed, non-sensitive messages may cross the command boundary."""


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def dump(value):
    return (json.dumps(value, ensure_ascii=True, sort_keys=True, indent=2) + '\n').encode()


def strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ReleaseError('Duplicate JSON field')
            result[key] = value
        return result

    def bad_constant(_):
        raise ReleaseError('Invalid JSON constant')

    try:
        return json.loads(raw, object_pairs_hook=pairs, parse_constant=bad_constant)
    except (ValueError, UnicodeError, RecursionError):
        raise ReleaseError('Invalid release JSON') from None


def same_file(left, right):
    return (left.st_dev, left.st_ino, left.st_mode, left.st_size,
            left.st_mtime_ns, left.st_ctime_ns) == (
                right.st_dev, right.st_ino, right.st_mode, right.st_size,
                right.st_mtime_ns, right.st_ctime_ns)


@contextmanager
def directory(path):
    """Reject a linked leaf and writable directories; owner mutation is trusted."""
    before = os.lstat(path)
    if not stat.S_ISDIR(before.st_mode) or before.st_mode & 0o7022:
        raise ReleaseError('Directory must be an existing trusted directory')
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        after = os.fstat(fd)
        if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
            raise ReleaseError('Directory changed while opening')
        yield fd
    finally:
        os.close(fd)


def read_file(root_fd, name, limit, expected_mode=0o644, expected_sha=None):
    # Callers supply one fixed leaf name; no traversal through an input path.
    if not COMPONENT.fullmatch(name):
        raise ReleaseError('Invalid metadata filename')
    before = os.stat(name, dir_fd=root_fd, follow_symlinks=False)
    if (not stat.S_ISREG(before.st_mode) or stat.S_IMODE(before.st_mode) != expected_mode
            or before.st_size <= 0 or before.st_size > limit):
        raise ReleaseError('Invalid metadata file')
    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=root_fd)
    try:
        if not same_file(before, os.fstat(fd)):
            raise ReleaseError('Metadata changed while opening')
        with os.fdopen(fd, 'rb', closefd=False) as stream:
            raw = stream.read(limit + 1)
        if (len(raw) != before.st_size or not same_file(before, os.fstat(fd))
                or not same_file(before, os.stat(name, dir_fd=root_fd, follow_symlinks=False))):
            raise ReleaseError('Metadata changed while reading')
        if expected_sha is not None and sha(raw) != expected_sha:
            raise ReleaseError('Metadata checksum mismatch')
        return raw
    finally:
        os.close(fd)


def invoke(verifier, args):
    # No inherited DATABASE_URL, credentials, proxies, HOME, loader injection or
    # language-runtime hooks. The executable is a hash-verified private copy.
    try:
        result = subprocess.run([str(verifier)] + list(args), capture_output=True,
                                env={'PATH': os.defpath, 'LANG': 'C', 'LC_ALL': 'C'},
                                timeout=60, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise ReleaseError('Trusted verifier could not complete') from None
    if result.returncode != 0 or len(result.stdout) > MAX_JSON:
        raise ReleaseError('Trusted native verifier rejected the candidate')
    return strict_json(result.stdout)


@contextmanager
def trusted_verifier(path, expected_sha):
    """Never execute the candidate's path; pin a private copy of trusted bytes."""
    if not HASH.fullmatch(expected_sha):
        raise ReleaseError('A trusted verifier checksum is required')
    path = Path(os.path.abspath(path))
    with directory(path.parent) as parent_fd:
        raw = read_file(parent_fd, path.name, MAX_EXECUTABLE, 0o755, expected_sha)
    with tempfile.TemporaryDirectory(prefix='runverge-release-verifier-') as temporary:
        target = Path(temporary) / 'platform'
        fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o700)
        with os.fdopen(fd, 'wb') as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        # Bind what will execute; the random private 0700 parent is owned by us.
        if sha(target.read_bytes()) != expected_sha:
            raise ReleaseError('Trusted verifier copy changed')
        yield target


def release_payload(name):
    return name in FIXED_PAYLOAD or name in OPTIONAL_GUIDES or bool(ASSET.fullmatch(name) or NOTICE.fullmatch(name))


def validate_sources(raw, identity):
    sources = strict_json(raw)
    if not isinstance(sources, dict) or not 1 <= len(sources) <= 8192:
        raise ReleaseError('Invalid source inventory')
    for name, digest in sources.items():
        parts = name.split('/')
        if (len(name) > 512 or any(not SOURCE_PART.fullmatch(part) or part in ('.', '..') for part in parts)
                or any(part.lower() in PRIVATE_PARTS for part in parts)
                or not isinstance(digest, str) or not HASH.fullmatch(digest)):
            raise ReleaseError('Source inventory contains an unsupported path or checksum')
    if sha(dump(sources)) != identity['source_sha256']:
        raise ReleaseError('Source inventory does not match the native identity')
    for migration in identity['migrations']:
        if sources.get('internal/storage/' + migration['name']) != migration['sha256']:
            raise ReleaseError('Source inventory does not match embedded migrations')
    return len(sources)


def validate_components(raw, files):
    inventory = strict_json(raw)
    if (not isinstance(inventory, dict)
            or set(inventory) != {'format', 'scope', 'components', 'build_tools', 'standard_sbom'}
            or inventory['format'] != 'runverge-component-inventory-v1'
            or inventory['scope'] != 'linked-server-and-production-web'
            or inventory['standard_sbom'] is not False):
        raise ReleaseError('Unsupported component inventory format')
    components, build_tools = inventory['components'], inventory['build_tools']
    if not isinstance(components, list) or not 1 <= len(components) <= 1024 or not isinstance(build_tools, list) or len(build_tools) > 1024:
        raise ReleaseError('Invalid component inventory')
    notices, identities = set(), set()
    for component in components:
        if (not isinstance(component, dict)
                or set(component) != {'kind', 'name', 'version', 'license_declared', 'notices'}
                or component['kind'] not in ('runtime', 'go-module', 'npm')):
            raise ReleaseError('Invalid component record')
        for key in ('name', 'version'):
            value = component[key]
            if not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9@][A-Za-z0-9@./_+:-]{0,255}', value):
                raise ReleaseError('Invalid component identity')
        key = (component['kind'], component['name'], component['version'])
        if key in identities:
            raise ReleaseError('Duplicate component identity')
        identities.add(key)
        declared = component['license_declared']
        if declared is not None and (not isinstance(declared, str) or len(declared) > 256 or any(ord(c) < 32 for c in declared)):
            raise ReleaseError('Invalid declared license')
        entries = component['notices']
        if not isinstance(entries, list) or not 1 <= len(entries) <= 32:
            raise ReleaseError('Missing component notices')
        for name in entries:
            if not isinstance(name, str) or not NOTICE.fullmatch(name) or name not in files or name in notices:
                raise ReleaseError('Invalid or duplicated notice reference')
            notices.add(name)
    if notices != {name for name in files if NOTICE.fullmatch(name)}:
        raise ReleaseError('Component notice inventory mismatch')
    for tool in build_tools:
        if (not isinstance(tool, dict) or set(tool) != {'name', 'version'}
                or any(not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9@][A-Za-z0-9@./_+:-]{0,255}', value) for value in tool.values())):
            raise ReleaseError('Invalid build tooling record')
    return len(components), len(build_tools)


def metadata(bundle, verifier, verifier_sha, manifest_sha, version, api_version, target_os, arch):
    if not HASH.fullmatch(manifest_sha) or not VERSION.fullmatch(version):
        raise ReleaseError('Explicit candidate checksum and prerelease version are required')
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', api_version) or target_os != 'darwin' or arch != 'arm64':
        raise ReleaseError('This release profile supports macOS arm64 only')
    bundle = Path(os.path.abspath(bundle))
    with directory(bundle) as bundle_fd:
        version_report = invoke(verifier, ['version'])
        if not isinstance(version_report, dict):
            raise ReleaseError('Invalid trusted verifier identity report')
        report = invoke(verifier, ['bundle', 'inspect', '--source', str(bundle), '--manifest-sha256', manifest_sha])
        if (not isinstance(report, dict) or report.get('manifest_sha256') != manifest_sha
                or report.get('installed') is not False or report.get('publisher_authenticated') is not False
                or report.get('product') != 'RunVerge' or report.get('version') != version):
            raise ReleaseError('Native candidate report does not match the requested release')
        manifest_raw = read_file(bundle_fd, 'bundle.json', MAX_JSON, expected_sha=manifest_sha)
        manifest = strict_json(manifest_raw)
        identity = manifest['identity']
        if (manifest['version'] != version or report['identity'] != identity
                or identity['api_version'] != api_version or identity['os'] != target_os or identity['arch'] != arch
                or version_report.get('product') != 'RunVerge' or version_report.get('version') != version
                or version_report.get('identity') != identity):
            raise ReleaseError('Requested version, platform or verifier identity mismatch')
        files = {entry['path']: entry for entry in manifest['files']}
        if not FIXED_PAYLOAD.issubset(files) or any(not release_payload(name) for name in files):
            raise ReleaseError('Unsupported or private development payload in release candidate')
        component_raw = read_file(bundle_fd, 'components.json', MAX_JSON, expected_sha=files['components.json']['sha256'])
        source_raw = read_file(bundle_fd, 'source-files.json', MAX_JSON, expected_sha=files['source-files.json']['sha256'])
        component_count, tooling_count = validate_components(component_raw, files)
        source_count = validate_sources(source_raw, identity)
        if invoke(verifier, ['bundle', 'inspect', '--source', str(bundle), '--manifest-sha256', manifest_sha]) != report:
            raise ReleaseError('Candidate changed during release preparation')
        if read_file(bundle_fd, 'bundle.json', MAX_JSON, expected_sha=manifest_sha) != manifest_raw:
            raise ReleaseError('Candidate manifest changed during release preparation')
    sums = {entry['path']: entry['sha256'] for entry in manifest['files']}
    sums['bundle.json'] = manifest_sha
    checksums = ''.join('%s  %s\n' % (digest, name) for name, digest in sorted(sums.items())).encode('ascii')
    release = {'format': FORMAT, 'product': 'RunVerge', 'version': version, 'identity': identity,
               'bundle_manifest_sha256': manifest_sha, 'verifier_sha256': verifier_sha,
               'checksum_paths_relative_to': 'input-native-bundle-root',
               'bundle_files': manifest['files'], 'file_count': report['file_count'],
               'total_payload_bytes': report['total_bytes'],
               'components_sha256': files['components.json']['sha256'],
               'source_inventory_sha256': files['source-files.json']['sha256']}
    validation = {'format': FORMAT, 'result': 'passed', 'bundle_manifest_sha256': manifest_sha,
                  'verifier_sha256': verifier_sha, 'component_count': component_count,
                  'build_tool_count': tooling_count, 'source_file_count': source_count,
                  'checks': ['trusted-verifier-bytes', 'native-inventory-and-payload-integrity',
                             'exact-candidate-version-api-and-platform', 'release-payload-allowlist',
                             'component-notice-references', 'source-and-migration-identity'],
                  'publisher_authenticated': False, 'standard_sbom': False,
                  'complete_secret_scan': False, 'release_ready': False,
                  'network_or_database_used': False}
    return {'SHA256SUMS': checksums, 'release-manifest.json': dump(release),
            'validation-report.json': dump(validation)}


def completion(contents):
    return dump({'format': FORMAT, 'files': {name: sha(raw) for name, raw in sorted(contents.items())}})


def write_file(directory_fd, name, raw):
    fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory_fd)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(raw)
        stream.flush()
        os.fchmod(stream.fileno(), 0o644)
        os.fsync(stream.fileno())


def write_metadata(output, contents):
    if any(len(raw) > MAX_JSON for raw in contents.values()):
        raise ReleaseError('Release metadata exceeds the supported size')
    output = Path(os.path.abspath(output))
    if not COMPONENT.fullmatch(output.name):
        raise ReleaseError('Metadata output must be a new named directory')
    with directory(output.parent) as parent_fd:
        # Reserve once, never overwrite/reuse/delete a partial or existing output.
        os.mkdir(output.name, 0o700, dir_fd=parent_fd)
        fd = os.open(output.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd)
        try:
            for name, raw in sorted(contents.items()):
                write_file(fd, name, raw)
            os.fsync(fd)
            write_file(fd, 'complete.json', completion(contents))
            os.fsync(fd)
            os.fsync(parent_fd)
        finally:
            os.close(fd)


def verify_metadata(output, contents):
    with directory(output) as fd:
        if set(os.listdir(fd)) != OUTPUT_NAMES:
            raise ReleaseError('Release metadata is incomplete or has unexpected files')
        for name, expected in dict(contents, **{'complete.json': completion(contents)}).items():
            if read_file(fd, name, MAX_JSON) != expected:
                raise ReleaseError('Release metadata differs from the verified candidate')


def prepare(mode, bundle, verifier, verifier_sha, manifest_sha, version, api_version, target_os, arch, output):
    if mode not in ('prepare', 'verify'):
        raise ReleaseError('Unknown release metadata operation')
    bundle_path, output_path = Path(bundle).resolve(), Path(output).resolve()
    if bundle_path == output_path or bundle_path in output_path.parents or output_path in bundle_path.parents:
        raise ReleaseError('Metadata output must be separate from the input bundle')
    with trusted_verifier(verifier, verifier_sha) as executable:
        contents = metadata(bundle, executable, verifier_sha, manifest_sha, version, api_version, target_os, arch)
        if mode == 'prepare':
            write_metadata(output, contents)
        verify_metadata(output, contents)
    return {'format': FORMAT, 'result': 'passed', 'operation': mode,
            'version': version, 'bundle_manifest_sha256': manifest_sha,
            'metadata_sha256': {name: sha(raw) for name, raw in sorted(contents.items())},
            'completion_sha256': sha(completion(contents)), 'release_ready': False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=('prepare', 'verify'))
    for flag in ('bundle', 'verifier', 'output'):
        parser.add_argument('--' + flag, type=Path, required=True)
    for flag in ('verifier-sha256', 'manifest-sha256', 'version', 'api-version', 'os', 'arch'):
        parser.add_argument('--' + flag, required=True)
    args = parser.parse_args()
    try:
        result = prepare(args.operation, args.bundle, args.verifier, args.verifier_sha256,
                         args.manifest_sha256, args.version, args.api_version, args.os, args.arch, args.output)
        print(json.dumps(result, sort_keys=True, indent=2))
    except (ReleaseError, OSError, ValueError, KeyError, TypeError, RecursionError) as error:
        raise SystemExit(str(error) if isinstance(error, ReleaseError) else 'Release metadata operation failed') from None


if __name__ == '__main__':
    main()

#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Build an offline RunVerge native bundle. Does not install or publish it."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

import dev
from verify_foundation import source_manifest

ROOT = dev.ROOT
FORMAT = 'runverge-native-bundle-v1'
OPERATOR_GUIDES = {
    'RUNNING.md': 'docs/NATIVE-BUNDLE.md',
    'QUICKSTART-ALPHA.md': 'docs/QUICKSTART-ALPHA.md',
    'LOCAL-MODELS.md': 'docs/LOCAL-MODELS.md',
    'MCP-READ.md': 'docs/MCP-READ.md',
    'OPENROUTER.md': 'docs/OPENROUTER.md',
}


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def dump(value):
    return (json.dumps(value, ensure_ascii=True, sort_keys=True, indent=2) + '\n').encode()


def write(directory, name, raw, mode=0o644):
    target = directory / name
    target.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    with target.open('xb') as stream:
        stream.write(raw)
    target.chmod(mode)


def build_environment():
    # Build-time tool discovery may use this checkout's developer toolchain.
    # No local database settings, model keys, tokens or private files are read.
    result = {key: value for key, value in os.environ.items()
              if key in ('HOME', 'PATH', 'TMPDIR', 'LANG', 'LC_ALL', 'SYSTEMROOT')}
    result['PATH'] = os.pathsep.join((str(Path(dev.executable('go')).parent),
                                    str(Path(dev.executable('node')).parent), result.get('PATH', '')))
    result['GOCACHE'] = str(dev.TASK / 'work/go-cache')
    result['GOMODCACHE'] = str(dev.TASK / 'work/go-mod')
    result.update(GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local', GOENV='off', GOWORK='off', CGO_ENABLED='0')
    return result


def run(args, env, cwd=ROOT):
    completed = subprocess.run([str(arg) for arg in args], cwd=cwd, env=env,
                               capture_output=True, text=True)
    if completed.returncode:
        # Build input may contain private local directory names; do not embed
        # tool output in artifacts or emit it as an installer error.
        raise RuntimeError('A build step failed: ' + Path(str(args[0])).name)
    return completed.stdout


def json_stream(text):
    decoder, values, position = json.JSONDecoder(), [], 0
    while position < len(text):
        while position < len(text) and text[position].isspace():
            position += 1
        if position == len(text):
            break
        value, position = decoder.raw_decode(text, position)
        values.append(value)
    return values


def source_inputs():
    inputs = source_manifest()
    for name in ('LICENSE', 'docs/LICENSING.md', *OPERATOR_GUIDES.values()):
        inputs[name] = sha((ROOT / name).read_bytes())
    return dict(sorted(inputs.items()))


def license_files(directory):
    result = []
    for item in sorted(directory.iterdir()):
        if re.fullmatch(r'(LICENSE|LICENCE|COPYING|NOTICE)([._-][A-Za-z0-9._-]+)?', item.name, re.I):
            if item.is_symlink() or not item.is_file() or item.stat().st_size > 2 * 1024 * 1024:
                raise RuntimeError('Invalid dependency license file')
            result.append(item)
    if not result:
        raise RuntimeError('Missing dependency license notice')
    return result


def inventory(directory, env):
    # Include linked Go modules, the Go runtime and the production npm closure.
    # Tooling used only to build the web bundle is recorded as build tooling.
    packages = json_stream(run(['go', 'list', '-mod=readonly', '-deps', '-json', './cmd/platform'], env))
    modules = {}
    for package in packages:
        module = package.get('Module')
        if module and not module.get('Main'):
            if module.get('Replace'):
                raise RuntimeError('Native release builder does not permit module replacements')
            modules[module['Path']] = module
    components = []

    def add(kind, name, version, location, declared_license=None):
        index = len(components)
        notices = []
        for item in license_files(location):
            target = 'notices/%03d/%s' % (index, item.name)
            write(directory, target, item.read_bytes())
            notices.append(target)
        components.append({'kind': kind, 'name': name, 'version': version,
                           'license_declared': declared_license,
                           'notices': notices})

    go_root = Path(run(['go', 'env', 'GOROOT'], env).strip())
    add('runtime', 'Go', run(['go', 'env', 'GOVERSION'], env).strip(), go_root, 'BSD-3-Clause')
    for name, module in sorted(modules.items()):
        add('go-module', name, module['Version'], Path(module['Dir']))

    visited = set()

    def npm(name, parent):
        # Resolve the exact installed dependency, including pnpm's symlink graph.
        location = parent
        while True:
            candidate = location / 'node_modules' / name
            if (candidate / 'package.json').is_file():
                location = candidate.resolve()
                break
            if location == location.parent:
                raise RuntimeError('Missing production npm dependency')
            location = location.parent
        package = json.loads((location / 'package.json').read_text())
        key = (package['name'], package['version'])
        if key in visited:
            return
        visited.add(key)
        add('npm', key[0], key[1], location, package.get('license'))
        for dependency in sorted(package.get('dependencies', {})):
            npm(dependency, location)

    web_package = json.loads((ROOT / 'web/package.json').read_text())
    for name in sorted(web_package.get('dependencies', {})):
        npm(name, ROOT / 'web')
    tools = []
    for name in sorted(web_package.get('devDependencies', {})):
        package = json.loads((ROOT / 'web/node_modules' / name / 'package.json').read_text())
        tools.append({'name': package['name'], 'version': package['version']})
    write(directory, 'components.json', dump({'format': 'runverge-component-inventory-v1',
                                            'scope': 'linked-server-and-production-web',
                                            'components': components,
                                            'build_tools': tools,
                                            'standard_sbom': False}))


def build(output, version):
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+-[A-Za-z0-9][A-Za-z0-9.-]{0,63}', version):
        raise RuntimeError('An explicit prerelease version is required')
    output = output.absolute()
    if output.exists() or output.is_symlink() or not output.parent.is_dir():
        raise RuntimeError('Output must be new and its parent must exist')
    if ROOT == output or ROOT in output.parents:
        raise RuntimeError('Build artifacts must be outside the source checkout')
    before = source_inputs()
    source_hash = sha(dump(before))
    env = build_environment()
    target = json.loads(run(['go', 'env', '-json', 'GOOS', 'GOARCH'], env))
    api_version = re.search(r'const ContractVersion = "([0-9.]+)"', (ROOT / 'internal/domain/types.go').read_text()).group(1)
    migrations = [{'name': 'migrations/' + item.name, 'sha256': sha(item.read_bytes())}
                  for item in sorted((ROOT / 'internal/storage/migrations').glob('*.sql'))]
    identity = {'api_version': api_version, 'os': target['GOOS'], 'arch': target['GOARCH'],
                'source_sha256': source_hash, 'migrations': migrations}
    with tempfile.TemporaryDirectory(prefix='runverge-build-', dir=output.parent) as temporary:
        package = Path(temporary) / 'bundle'
        package.mkdir(mode=0o755)
        (package / 'bin').mkdir()
        flags = '-X main.nativeVersion=%s -X main.nativeSourceSHA256=%s' % (version, source_hash)
        run(['go', 'build', '-mod=readonly', '-trimpath', '-buildvcs=false', '-ldflags', flags,
             '-o', package / 'bin/platform', './cmd/platform'], env)
        (package / 'bin/platform').chmod(0o755)
        run([dev.executable('node'), 'node_modules/typescript/bin/tsc', '--noEmit'], env, ROOT / 'web')
        run([dev.executable('node'), '--input-type=module', '-e',
             "import {build} from 'vite'; await build({envDir:false,build:{outDir:process.argv[1]}});",
             package / 'web'], env, ROOT / 'web')
        for item in (package / 'web').rglob('*'):
            if item.is_symlink() or not (item.is_dir() or item.is_file()):
                raise RuntimeError('Invalid generated web payload')
            item.chmod(0o755 if item.is_dir() else 0o644)
        write(package, 'LICENSE', (ROOT / 'LICENSE').read_bytes())
        for target_name, source_name in OPERATOR_GUIDES.items():
            write(package, target_name, (ROOT / source_name).read_bytes())
        write(package, 'helpers/model_key.py', (ROOT / 'scripts/model_key.py').read_bytes())
        write(package, 'LICENSING.md', b'# RunVerge native distribution\n\n'
              b'Original server, CLI and Workspace: Elastic License 2.0; see LICENSE.\n'
              b'Dependencies retain their own licenses; see components.json and notices/.\n'
              b'Python/TypeScript SDKs and examples use Apache-2.0 and are distributed separately.\n'
              b'This inventory is not a legal review or a standard SBOM.\n')
        write(package, 'source-files.json', dump(before))
        inventory(package, env)
        if source_inputs() != before:
            raise RuntimeError('Source inputs changed during build; retry from a stable snapshot')
        files = []
        for item in sorted(package.rglob('*')):
            if item.is_file():
                files.append({'path': item.relative_to(package).as_posix(), 'sha256': sha(item.read_bytes()),
                              'size': item.stat().st_size, 'mode': item.stat().st_mode & 0o777})
        manifest = {'format': FORMAT, 'product': 'RunVerge', 'version': version, 'identity': identity, 'files': files}
        raw = dump(manifest)
        write(package, 'bundle.json', raw)
        manifest_hash = sha(raw)
        # Verification uses the actual bundled executable without DB variables.
        # This is a local compiler output, never an arbitrary downloaded binary.
        checked = json.loads(run([package / 'bin/platform', 'bundle', 'inspect', '--source', package,
                                  '--manifest-sha256', manifest_hash], env))
        if checked['identity'] != identity or checked['installed']:
            raise RuntimeError('Built artifact identity mismatch')
        # Reserve output, so an existing or concurrently created folder is never replaced.
        output.mkdir(mode=0o700)
        for item in sorted(package.iterdir()):
            if item.is_dir():
                shutil.copytree(item, output / item.name, symlinks=False)
            else:
                shutil.copyfile(item, output / item.name)
                (output / item.name).chmod(item.stat().st_mode & 0o777)
        checked = json.loads(run([output / 'bin/platform', 'bundle', 'inspect', '--source', output,
                                  '--manifest-sha256', manifest_hash], env))
        return checked


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--version', required=True)
    args = parser.parse_args()
    try:
        print(json.dumps(build(args.output, args.version), indent=2))
    except (RuntimeError, OSError, ValueError, KeyError) as error:
        # Fixed errors above are safe. Filesystem exceptions may contain local
        # paths, so only show their type rather than their full detail.
        raise SystemExit(str(error) if isinstance(error, RuntimeError) else 'Native build failed: ' + type(error).__name__) from None


if __name__ == '__main__':
    main()

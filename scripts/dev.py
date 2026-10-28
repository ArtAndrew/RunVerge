#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Local developer commands. Credentials never appear in command output."""
import json
import os
from pathlib import Path
import secrets
import shutil
import stat
import subprocess
import sys
from urllib.parse import quote, urlparse, unquote

ROOT = Path(__file__).resolve().parents[1]
TASK = ROOT.parents[1]
LOCAL = ROOT / '.local'

FIXTURE_CREDENTIALS = {
    'model-fixture-secrets.json': 'PLATFORM_MODEL_KEY_NATIVE_FIXTURE',
    'tool-fixture-secrets.json': 'PLATFORM_TOOL_KEY_LOCAL_FIXTURE',
    'read-fixture-secrets.json': 'PLATFORM_TOOL_KEY_LOCAL_READ_FIXTURE',
}

def fixture_environment(env, private=LOCAL):
    """Explicit development opt-in. Never interpret a file as shell commands."""
    try:
        directory = private.lstat()
    except OSError:
        raise SystemExit('Start the explicit synthetic peers before loading fixture credentials') from None
    if not stat.S_ISDIR(directory.st_mode) or directory.st_mode & 0o077 or directory.st_uid != os.getuid():
        raise SystemExit('Fixture credentials require a private, real .local directory')
    result = dict(env)
    loaded = 0
    for filename, expected in FIXTURE_CREDENTIALS.items():
        try:
            fd = os.open(private / filename, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        except FileNotFoundError:
            continue
        except OSError:
            raise SystemExit('Cannot safely open private fixture credentials') from None
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.getuid() or not 1 <= info.st_size <= 4096:
                raise SystemExit('Fixture credentials must be private, bounded regular files')
            with os.fdopen(fd, 'r', encoding='utf-8') as source:
                fd = None
                raw = source.read(4097)
            if len(raw) > 4096:
                raise ValueError('credential size limit')
            def exact(pairs):
                value = {}
                for key, item in pairs:
                    if key in value:
                        raise ValueError('duplicate field')
                    value[key] = item
                return value
            value = json.loads(raw, object_pairs_hook=exact)
            if not isinstance(value, dict) or set(value) != {'credential_environment', 'credential'}:
                raise ValueError('invalid credential object')
            credential = value['credential']
            if value['credential_environment'] != expected or not isinstance(credential, str) or not 32 <= len(credential) <= 512 or any(ord(c) < 33 or ord(c) > 126 for c in credential):
                raise ValueError('invalid credential reference or value')
            if expected in result and result[expected] != credential:
                raise ValueError('conflicting inherited credential')
            result[expected] = credential
            loaded += 1
        except (ValueError, UnicodeError, OSError, RecursionError):
            raise SystemExit('Invalid or conflicting private fixture credentials') from None
        finally:
            if fd is not None:
                os.close(fd)
    if not loaded:
        raise SystemExit('No private fixture credentials found; start the explicit synthetic peers first')
    return result

def executable(name):
    found = shutil.which(name)
    if found:
        return found
    choices = {
        'go': [TASK / 'work/toolchain/go/bin/go'],
        'node': [Path.home() / '.nvm/versions/node/v24.21.0/bin/node'],
        'initdb': [TASK / 'work/local-postgres/node_modules/@embedded-postgres/darwin-arm64/native/bin/initdb'],
        'pg_ctl': [TASK / 'work/local-postgres/node_modules/@embedded-postgres/darwin-arm64/native/bin/pg_ctl'],
    }
    for p in choices.get(name, []):
        if p.is_file():
            return str(p)
    raise SystemExit(f'{name} is missing. Install the documented development prerequisite.')

def environment():
    env = dict(os.environ)
    env['PATH'] = str(Path(executable('go')).parent) + os.pathsep + env.get('PATH', '')
    env['GOCACHE'] = str(TASK / 'work/go-cache')
    env['GOMODCACHE'] = str(TASK / 'work/go-mod')
    config = LOCAL / 'database.json'
    if config.exists() and 'DATABASE_URL' not in env:
        env['DATABASE_URL'] = json.loads(config.read_text())['url']
    return env

def call(args, env=None):
    return subprocess.run([str(a) for a in args], cwd=ROOT, env=env, check=True)

def db_init():
    LOCAL.mkdir(mode=0o700, exist_ok=True)
    if (LOCAL / 'postgres/PG_VERSION').exists():
        print('Local database already initialized')
        return
    config_file = LOCAL / 'database.json'
    if config_file.exists():
        password = unquote(urlparse(json.loads(config_file.read_text())['url']).password)
    else:
        password = secrets.token_urlsafe(32)
        config = {'url': f'postgres://agentplatform:{quote(password)}@127.0.0.1:55432/postgres?sslmode=disable'}
        with config_file.open('x') as f:
            os.chmod(config_file, 0o600)
            json.dump(config, f)
    pw = LOCAL / 'pg-password'
    try:
        with pw.open('x') as f:
            os.chmod(pw, 0o600)
            f.write(password)
        call([executable('initdb'), '-D', LOCAL / 'postgres', '-U', 'agentplatform', '--auth=scram-sha-256', '--pwfile', pw, '--encoding=UTF8', '--locale=C'])
        with (LOCAL / 'postgres/postgresql.conf').open('a') as f:
            f.write("\nlisten_addresses='127.0.0.1'\nport=55432\nunix_socket_directories=''\nmax_connections=100\n")
    finally:
        pw.unlink(missing_ok=True)

def main():
    if len(sys.argv)<2:
        raise SystemExit('Usage: dev.py doctor|db-init|db-start|db-stop|build|test|migrate|bootstrap|serve|worker|mcp-stdio|issue-token|issue-service-token|recover-login-policy|advance-event-replay|install-model|install-tool|fixture model|action|read')
    command = sys.argv[1]
    arguments = sys.argv[2:]
    if command == 'fixture':
        peers = {'model': 'native_model_peer.py', 'action': 'tool_action_peer.py', 'read': 'read_lookup_peer.py'}
        if len(arguments) != 1 or arguments[0] not in peers:
            raise SystemExit('Usage: dev.py fixture model|action|read')
        return call([sys.executable, ROOT/'tests/fixtures'/peers[arguments[0]], '--repo', ROOT])
    fixture_flag = '--fixture-credentials'
    use_fixtures = fixture_flag in arguments
    if use_fixtures:
        if command != 'worker' or arguments.count(fixture_flag) != 1:
            raise SystemExit('--fixture-credentials is supported once, for the development worker only')
        arguments = [a for a in arguments if a != fixture_flag]
    if command == 'doctor':
        for name in ['go', 'node', 'initdb', 'pg_ctl']:
            print(name + ': ' + executable(name))
        print('Database credentials: ' + ('configured' if (LOCAL/'database.json').exists() else 'not configured'))
        return
    if command == 'db-init':
        return db_init()
    if command == 'db-start':
        return call([executable('pg_ctl'), '-D', LOCAL/'postgres', '-l', LOCAL/'postgres.log', '-w', 'start'])
    if command == 'db-stop':
        return call([executable('pg_ctl'), '-D', LOCAL/'postgres', '-m', 'fast', '-w', 'stop'])
    env = environment()
    if use_fixtures:
        env = fixture_environment(env)
    if command == 'build':
        (ROOT/'bin').mkdir(exist_ok=True)
        return call(['go', 'build', '-trimpath', '-o', 'bin/platform', './cmd/platform'], env)
    if command == 'test':
        env['TEST_DATABASE_URL'] = env.get('DATABASE_URL', '')
        return call(['go', 'test', '-race', '-count=1', './...'], env)
    if command in ['migrate', 'bootstrap', 'serve', 'worker', 'mcp-stdio', 'issue-token', 'issue-service-token', 'recover-login-policy', 'advance-event-replay', 'install-model', 'install-tool']:
        return call([ROOT/'bin/platform', command, *arguments], env)
    raise SystemExit('Unknown developer command')

if __name__ == '__main__':
    try:
        main()
    except subprocess.CalledProcessError as exc:
        raise SystemExit(exc.returncode)

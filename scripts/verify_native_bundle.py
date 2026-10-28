#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Install a native bundle and exercise it with an owned disposable PostgreSQL cluster.

This checks package portability/restart, not real-model acceptance or backup/restore.
Never reads the saved development database or starts/stops its processes.
"""
import argparse
import contextlib
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import tempfile
import time
from urllib.parse import quote
from urllib.request import Request, build_opener, ProxyHandler
from urllib.error import URLError

import dev


def unused_port():
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        return listener.getsockname()[1]


def stop_owned_child(child):
    if child.poll() is None:
        child.terminate()
        try:
            child.wait(timeout=15)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=5)


def cleanup_owned(children, pg_ctl, cluster, pg_env, start_attempted, streams):
    """Confirm owned resources stopped even if startup/earlier cleanup was ambiguous.

    A false result requires retaining the private test directory. Every cleanup
    is attempted independently; an earlier exception cannot skip later children
    or a database that may have started before pg_ctl lost its acknowledgment.
    """
    errors = []
    for child in reversed(children):
        try:
            stop_owned_child(child)
            if child.poll() is None:
                errors.append('owned_child_shutdown_unconfirmed')
        except Exception:
            errors.append('owned_child_shutdown_unconfirmed')
    if start_attempted:
        try:
            args = [pg_ctl, '-D', str(cluster)]
            status = subprocess.run([*args, 'status'], env=pg_env, capture_output=True, timeout=15)
            if status.returncode == 0:
                subprocess.run([*args, '-m', 'fast', '-w', 'stop'], env=pg_env,
                               capture_output=True, timeout=30)
                status = subprocess.run([*args, 'status'], env=pg_env, capture_output=True, timeout=15)
            # pg_ctl's explicit "not running" status. Other errors do not prove
            # a stopped database and must never authorize directory removal.
            if status.returncode != 3:
                errors.append('owned_postgres_shutdown_unconfirmed')
        except Exception:
            errors.append('owned_postgres_shutdown_unconfirmed')
    for stream in streams:
        try:
            stream.close()
        except Exception:
            errors.append('owned_log_close_failed')
    return not errors, errors


def verify(bundle, manifest_hash, evidence, extra_probe=None):
    if not re.fullmatch('[a-f0-9]{64}', manifest_hash):
        raise RuntimeError('A canonical manifest checksum is required')
    if evidence.exists():
        raise RuntimeError('Evidence path must be new')
    manifest = json.loads((bundle / 'bundle.json').read_text())
    steps, sensitive, children, output_streams = [], [], [], []
    started = datetime.datetime.now(datetime.timezone.utc).isoformat()
    report = {'scope': 'native_package_install_and_restart_only', 'started_at': started,
              'manifest_sha256': manifest_hash, 'identity': manifest['identity'],
              'passed': False, 'steps': steps,
              'not_proven': ['named_paid_provider_interoperability_and_final_billing', 'external_mcp_acceptance',
                             'backup_restore', 'upgrade_downgrade', 'other_os_architectures',
                             'publisher_signature', 'full_alpha_or_production_readiness']}
    if extra_probe is None:
        report['not_proven'].append('real_model_acceptance')
    pg_started = False
    pg_start_attempted = False
    # An unsuccessful PostgreSQL shutdown must retain its files for diagnosis.
    # TemporaryDirectory would remove a still-live cluster on context exit.
    with contextlib.nullcontext(tempfile.mkdtemp(prefix='runverge-native-smoke-')) as tmp:
        root = Path(tmp)
        private = root / 'private'; private.mkdir(mode=0o700)
        cluster = private / 'postgres'
        cwd = root / 'unrelated-cwd'; cwd.mkdir()
        env = {'PATH': '/usr/bin:/bin', 'HOME': str(private), 'TMPDIR': str(root),
               'LANG': 'C', 'LC_ALL': 'C'}
        pg_env = dict(env)
        pg_ctl, initdb = dev.executable('pg_ctl'), dev.executable('initdb')

        def record(name, args, selected_env=env):
            begin = time.monotonic()
            result = subprocess.run([str(x) for x in args], cwd=cwd, env=selected_env,
                                    capture_output=True, text=True, timeout=90)
            step = {'name': name, 'exit_code': result.returncode,
                    'duration_seconds': round(time.monotonic() - begin, 3)}
            steps.append(step)
            if result.returncode:
                raise RuntimeError('Native smoke step failed: ' + name)
            return result.stdout

        def process(name, args):
            log = (private / (name + '.log')).open('x')
            os.chmod(log.name, 0o600); output_streams.append(log)
            child = subprocess.Popen([str(x) for x in args], cwd=cwd, env=env,
                                     stdout=log, stderr=log)
            children.append(child)
            return child

        def stop_child(child):
            stop_owned_child(child)

        try:
            # This process and the commands it launches have no developer runtimes
            # in PATH, no source cwd and no inherited model/API/database settings.
            binary = bundle / 'bin/platform'
            version = json.loads(record('version_without_database', [binary, 'version']))
            if version['identity'] != manifest['identity']:
                raise RuntimeError('Native version identity mismatch')
            record('inspect_without_database', [binary, 'bundle', 'inspect', '--source', bundle,
                                                '--manifest-sha256', manifest_hash])
            installed = root / 'installed'
            record('install_without_database', [binary, 'bundle', 'install', '--source', bundle,
                                                '--destination', installed, '--manifest-sha256', manifest_hash])
            binary = installed / 'bin/platform'
            record('verify_installed_without_database', [binary, 'bundle', 'verify', '--source', installed,
                                                         '--manifest-sha256', manifest_hash])
            password = secrets.token_urlsafe(32); sensitive.append(password)
            password_path = private / 'pg-password'
            with password_path.open('x') as stream:
                os.chmod(password_path, 0o600); stream.write(password)
            record('initialize_owned_postgres', [initdb, '-D', cluster, '-U', 'runverge',
                                                 '--auth=scram-sha-256', '--pwfile', password_path,
                                                 '--encoding=UTF8', '--locale=C'], pg_env)
            password_path.unlink()
            database_port = unused_port()
            with (cluster / 'postgresql.conf').open('a') as stream:
                stream.write("\nlisten_addresses='127.0.0.1'\nport=%d\nunix_socket_directories=''\nmax_connections=60\n" % database_port)
            pg_start_attempted = True
            record('start_owned_postgres', [pg_ctl, '-D', cluster, '-l', private / 'postgres.log', '-w', 'start'], pg_env)
            pg_started = True
            env['DATABASE_URL'] = 'postgres://runverge:%s@127.0.0.1:%d/postgres?sslmode=disable' % (quote(password), database_port)
            sensitive.append(env['DATABASE_URL'])
            record('migrate_owned_database', [binary, 'migrate'])
            record('repeat_migration_noop', [binary, 'migrate'])
            credentials_path = private / 'bootstrap.json'
            record('bootstrap_owned_organization', [binary, 'bootstrap', '--name', 'RunVerge native smoke', '--out', credentials_path])
            credentials = json.loads(credentials_path.read_text()); sensitive.append(credentials['token'])
            api_port = unused_port()
            api = process('api-first', [binary, 'serve', '--listen', '127.0.0.1:%d' % api_port])
            base = 'http://127.0.0.1:%d' % api_port
            opener = build_opener(ProxyHandler({}))

            def request(route, payload=None):
                headers = {'Authorization': 'Bearer ' + credentials['token'],
                           'X-Organization-ID': credentials['organization_id'],
                           'X-Platform-API-Version': manifest['identity']['api_version']}
                data = None
                if payload is not None:
                    data = json.dumps(payload).encode()
                    headers.update({'Content-Type': 'application/json', 'Idempotency-Key': secrets.token_hex(16)})
                with opener.open(Request(base + route, data=data, headers=headers), timeout=3) as response:
                    raw = response.read(2 * 1024 * 1024)
                    if route == '/':
                        return raw
                    return json.loads(raw)['data']

            def ready(child):
                deadline = time.monotonic() + 20
                while time.monotonic() < deadline:
                    if child.poll() is not None:
                        raise RuntimeError('Installed server exited before ready')
                    try:
                        if request('/readyz')['status'] == 'ready':
                            return
                    except (OSError, ValueError, URLError):
                        pass
                    time.sleep(0.15)
                raise RuntimeError('Installed server readiness timeout')

            ready(api)
            page = request('/')
            if b'<html' not in page.lower() or b'<script' not in page.lower():
                raise RuntimeError('Compiled Workspace not served from unrelated cwd')
            for asset in re.findall(rb'(?:src|href)="(/assets/[^"]+)"', page):
                with opener.open(base + asset.decode(), timeout=3) as response:
                    if response.status != 200 or not response.read(1):
                        raise RuntimeError('Compiled Workspace asset missing')
            steps.append({'name': 'installed_workspace_assets_from_unrelated_cwd', 'passed': True})
            worker = process('worker-first', [binary, 'worker', '--concurrency', '2'])
            draft = request('/api/v1/agents', {'name': 'Native package fixture',
                                             'instructions': 'Run the explicitly synthetic echo fixture.',
                                             'model': 'fixture:echo', 'tools': [], 'budget_microusd': 10000})
            agent = request('/api/v1/agents/' + draft['id'] + '/publish', {'expected_version': draft['version']})
            task = request('/api/v1/tasks', {'goal': 'Verify installed RunVerge packaging.',
                                            'agent_id': agent['id'], 'budget_microusd': 10000})
            route = '/api/v1/tasks/' + task['id']
            deadline = time.monotonic() + 25
            while time.monotonic() < deadline:
                task = request(route)
                if task['status'] == 'succeeded':
                    break
                if task['status'] in ('failed', 'cancelled') or worker.poll() is not None:
                    raise RuntimeError('Installed fixture execution failed')
                time.sleep(0.2)
            if task['status'] != 'succeeded':
                raise RuntimeError('Installed fixture execution timeout')
            artifacts = request(route + '/artifacts')
            costs = request(route + '/costs')
            if not artifacts or task['spent_microusd'] != '1000':
                raise RuntimeError('Installed fixture did not persist result and synthetic cost')
            steps.append({'name': 'installed_fixture_task_result_and_synthetic_cost', 'passed': True})
            if extra_probe is not None:
                # A trusted test extension may exercise an explicitly configured
                # local dependency in this same owned installation. The default
                # package test does not acquire a model or call one implicitly.
                report['additional_probe'] = extra_probe({
                    'binary': binary, 'private': private, 'env': env,
                    'credentials': credentials, 'request': request,
                    'record': record, 'process': process,
                    'worker': worker, 'stop_child': stop_child,
                })
            stop_child(worker); stop_child(api)
            record('stop_owned_postgres', [pg_ctl, '-D', cluster, '-m', 'fast', '-w', 'stop'], pg_env)
            pg_started = False
            record('restart_owned_postgres', [pg_ctl, '-D', cluster, '-l', private / 'postgres.log', '-w', 'start'], pg_env)
            pg_started = True
            api = process('api-second', [binary, 'serve', '--listen', '127.0.0.1:%d' % api_port]); ready(api)
            worker = process('worker-second', [binary, 'worker', '--concurrency', '2'])
            if request(route) != task or request(route + '/artifacts') != artifacts or request(route + '/costs') != costs:
                raise RuntimeError('Restart changed completed task, artifact or ledger')
            steps.append({'name': 'owned_postgres_api_worker_restart_preserves_result_and_ledger', 'passed': True})
            if extra_probe is not None and callable(getattr(extra_probe, 'verify_restart', None)):
                report['additional_probe']['restart_verified'] = extra_probe.verify_restart(request)
            record('verify_installation_after_runtime', [binary, 'bundle', 'verify', '--source', installed,
                                                        '--manifest-sha256', manifest_hash])
            report['passed'] = True
        except Exception as error:
            report['failure_type'] = type(error).__name__
            report['failure'] = str(error) if isinstance(error, RuntimeError) else 'Native verification failed; private raw diagnostics withheld'
            raise
        finally:
            cleanup_complete, cleanup_errors = cleanup_owned(
                children, pg_ctl, cluster, pg_env, pg_start_attempted, output_streams)
            if not cleanup_complete:
                report['passed'] = False
                report['cleanup_errors'] = cleanup_errors
                report['retained_private_test_root'] = str(root)
            report['finished_at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
            report['cleanup_complete'] = cleanup_complete
            serialized = json.dumps(report, indent=2) + '\n'
            for value in sensitive:
                if value in serialized:
                    raise RuntimeError('Private value appeared in evidence')
            evidence.parent.mkdir(parents=True, exist_ok=True)
            with evidence.open('x') as stream:
                stream.write(serialized)
            if cleanup_complete:
                shutil.rmtree(root)
    if not report['passed']:
        raise RuntimeError('Native bundle verification did not pass')
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle', type=Path, required=True)
    parser.add_argument('--manifest-sha256', required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    args = parser.parse_args()
    try:
        report = verify(args.bundle.absolute(), args.manifest_sha256, args.evidence.absolute())
    except Exception as error:
        raise SystemExit(str(error) if isinstance(error, RuntimeError) else 'Native verification failed: ' + type(error).__name__) from None
    print(json.dumps({'passed': report['passed'], 'steps': len(report['steps']),
                      'manifest_sha256': report['manifest_sha256']}))


if __name__ == '__main__':
    main()

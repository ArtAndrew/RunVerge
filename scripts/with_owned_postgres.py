#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Run trusted tests against a newly initialized, disposable PostgreSQL cluster.

Never reads the saved database configuration or inherits DATABASE_URL. The
supplied command gets only the new loopback cluster's private connection.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time
from urllib.parse import quote

import build_native_bundle as build
import dev
from verify_native_bundle import cleanup_owned, unused_port


def run(command, report_path):
    if not command or report_path.exists() or not report_path.parent.is_dir():
        raise RuntimeError('Command and a new report path in an existing directory are required')
    log_path = report_path.with_suffix('.log')
    if log_path.exists():
        raise RuntimeError('Test log path must be new')
    # mkdtemp owns a fresh0700 root; no operator/source directory is reused.
    private = Path(tempfile.mkdtemp(prefix='runverge-recovery-pg-'))
    cluster = private / 'postgres'
    password_file = private / 'pg-password'
    password = secrets.token_urlsafe(32)
    pg_env = {'PATH': '/usr/bin:/bin', 'HOME': str(private), 'TMPDIR': str(private),
              'LANG': 'C', 'LC_ALL': 'C'}
    pg_ctl, initdb = dev.executable('pg_ctl'), dev.executable('initdb')
    children, streams = [], []
    start_attempted = False
    result = {'scope': 'new_owned_postgresql_test_cluster', 'passed': False,
              'source_database_configuration_read': False, 'steps': []}
    sensitive = [password]
    started = time.monotonic()
    output = ''
    try:
        with password_file.open('x') as stream:
            os.chmod(password_file, 0o600)
            stream.write(password)
        initialized = subprocess.run([initdb, '-D', str(cluster), '-U', 'runverge',
                                      '--auth=scram-sha-256', '--pwfile', str(password_file),
                                      '--encoding=UTF8', '--locale=C'], env=pg_env,
                                     capture_output=True, timeout=90)
        result['steps'].append({'name': 'initialize_new_owned_cluster', 'exit_code': initialized.returncode})
        if initialized.returncode:
            raise RuntimeError('Owned PostgreSQL initialization failed')
        password_file.unlink()
        port = unused_port()
        with (cluster / 'postgresql.conf').open('a') as stream:
            stream.write("\nlisten_addresses='127.0.0.1'\nport=%d\nunix_socket_directories=''\nmax_connections=100\n" % port)
        start_attempted = True
        started_pg = subprocess.run([pg_ctl, '-D', str(cluster), '-l', str(private / 'postgres.log'),
                                     '-w', 'start'], env=pg_env, capture_output=True, timeout=90)
        result['steps'].append({'name': 'start_new_owned_cluster', 'exit_code': started_pg.returncode})
        if started_pg.returncode:
            raise RuntimeError('Owned PostgreSQL startup not confirmed')
        url = 'postgres://runverge:%s@127.0.0.1:%d/postgres?sslmode=disable' % (quote(password), port)
        sensitive.append(url)
        env = build.build_environment()
        env.update(DATABASE_URL=url, TEST_DATABASE_URL=url,
                   RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG='1', RUNVERGE_RECOVERY_OWNED_PG='1')
        # No inherited source DSN, synthetic credentials, model keys or proxy.
        child = subprocess.Popen(command, cwd=dev.ROOT, env=env, stdout=subprocess.PIPE,
                                 stderr=subprocess.STDOUT, text=True)
        children.append(child)
        output, _ = child.communicate()
        result['test_exit_code'] = child.returncode
        result['passed'] = child.returncode == 0
    except Exception as error:
        result['failure'] = str(error) if isinstance(error, RuntimeError) else type(error).__name__
    finally:
        complete, errors = cleanup_owned(children, pg_ctl, cluster, pg_env, start_attempted, streams)
        result['cleanup_complete'] = complete
        result['duration_seconds'] = round(time.monotonic() - started, 3)
        if not complete:
            result['passed'] = False
            result['cleanup_errors'] = errors
            result['retained_private_test_root'] = str(private)
        for value in sensitive:
            output = output.replace(value, '[REDACTED_TEST_CREDENTIAL]')
        serialized = json.dumps(result, sort_keys=True, indent=2) + '\n'
        if any(value in serialized for value in sensitive):
            raise RuntimeError('Private test credential appeared in report')
        with log_path.open('x') as stream:
            stream.write(output)
        with report_path.open('x') as stream:
            stream.write(serialized)
        if complete:
            shutil.rmtree(private)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--report', type=Path, required=True)
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    try:
        result = run(command, args.report)
    except (OSError, RuntimeError) as error:
        raise SystemExit(str(error) if isinstance(error, RuntimeError) else 'Owned test harness failed') from None
    print(json.dumps(result, sort_keys=True, indent=2))
    raise SystemExit(0 if result['passed'] else 1)


if __name__ == '__main__':
    main()

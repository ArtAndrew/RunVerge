#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Check Compose using a new disposable project and synthetic tasks only.

Never attaches to an existing project or loads saved deployment settings.
Only the sanitized evidence JSON is suitable for public CI artifacts.
"""
import argparse
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
from urllib.error import URLError
from urllib.request import Request, ProxyHandler, build_opener
from urllib.parse import urlparse

import server_setup
import verify_recovery_process

ROOT = Path(__file__).resolve().parents[1]


def verify(evidence, image):
    if evidence.exists() or evidence.is_symlink():
        raise ValueError('Evidence path must be new')
    project = 'runverge-check-' + secrets.token_hex(8)
    container = project + '-bootstrap'
    root = Path(tempfile.mkdtemp(prefix='runverge-compose-check-'))
    report = {'scope': 'disposable_linux_container_fixture_and_restart', 'passed': False,
              'steps': [], 'cleanup_complete': False,
              'not_proven': ['real_model_quality', 'named_cloud_provider', 'backup_restore',
                             'upgrades', 'multi_user_security', 'production_readiness']}
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    env = {key: value for key, value in os.environ.items() if key in ('PATH', 'HOME', 'DOCKER_CONFIG')}
    env.update(RUNVERGE_PRIVATE_DIR=str(root / 'private'), RUNVERGE_PORT=str(port), RUNVERGE_IMAGE=image)
    compose = ['docker', 'compose', '--project-name', project, '--file', str(ROOT / 'deploy/compose.yaml')]
    attempted = False
    phase = 'reserve_disposable_project'

    def command(args, timeout=180):
        result = subprocess.run(args, env=env, capture_output=True, text=True, timeout=timeout)
        if result.returncode:
            # Retain diagnostics privately; never print service logs or settings.
            path = root / ('command-' + secrets.token_hex(4) + '.log')
            with path.open('x') as stream:
                os.chmod(path, 0o600)
                stream.write(result.stdout + result.stderr)
            raise RuntimeError('Container command failed; private diagnostic retained')
        return result.stdout + result.stderr if args[:2] == ['docker', 'logs'] else result.stdout

    def step(name):
        report['steps'].append({'name': name, 'passed': True})

    def check_application_credentials(identifier):
        state = json.loads(command(['docker', 'inspect', identifier]))[0]
        entries = state['Config']['Env']
        if any(admin_password in entry or entry.startswith(('POSTGRES_PASSWORD=',
               'RUNVERGE_APP_VERIFIER=', 'RUNVERGE_CREDENTIAL_PROFILE=')) for entry in entries):
            raise RuntimeError('Database administrator settings reached an application container')
        if 'DATABASE_URL=' + runtime_url not in entries:
            raise RuntimeError('Application credential missing')

    def sql(role, password, statement, database_name='runverge'):
        # Password is passed over stdin, never argv, a shell expansion or a log.
        return subprocess.run(['docker', 'exec', '-i', database, 'sh', '-c',
            'IFS= read -r PGPASSWORD; export PGPASSWORD; export PGCONNECT_TIMEOUT=3; '
            'exec psql -h 127.0.0.1 -U "$1" -d "$2" -X -At --no-password '
            '-v ON_ERROR_STOP=1 -v VERBOSITY=verbose', 'sh', role, database_name],
            input=password + '\n' + statement + '\n', env=env, capture_output=True,
            text=True, timeout=15)

    def require_sql(role, password, statement, expected):
        result = sql(role, password, statement)
        if result.returncode or result.stdout.strip() != expected:
            raise RuntimeError('Database role check failed')

    def require_denied(statement):
        result = sql('runverge', runtime_password, statement)
        if not result.returncode or '42501' not in result.stderr:
            raise RuntimeError('Database privilege boundary not enforced')

    try:
        # Refuse any collision before acquiring authority to clean this project.
        for resource, listing in [('container', ['ps', '-aq']), ('volume', ['volume', 'ls', '-q'])]:
            if command(['docker', *listing, '--filter', 'label=com.docker.compose.project=' + project]).strip():
                raise RuntimeError('Disposable project collision: ' + resource)
        if command(['docker', 'ps', '-aq', '--filter', 'label=runverge.check.owner=' + project]).strip():
            raise RuntimeError('Disposable shutdown resource collision')
        if command(['docker', 'image', 'ls', '-q', project + '-process-check']).strip():
            raise RuntimeError('Disposable shutdown image collision')
        server_setup.initialize(root / 'private')
        db_settings = dict(line.split('=', 1) for line in (root / 'private/db.env').read_text().splitlines())
        admin_password = db_settings['POSTGRES_PASSWORD']
        runtime_url = (root / 'private/runtime.env').read_text().strip().split('=', 1)[1]
        runtime_password = urlparse(runtime_url).password
        if not runtime_password or admin_password == runtime_password:
            raise RuntimeError('Database credentials must be distinct')
        attempted = True
        phase = 'start_compose_services'
        command([*compose, 'up', '--no-build', '-d'])
        phase = 'bootstrap_disposable_owner'
        command([*compose, '--profile', 'admin', 'run', '--no-deps', '--name', container,
                 'admin', 'bootstrap', '--name', 'Container synthetic check', '--out', '/state/bootstrap.json'])
        credentials_path = root / 'bootstrap.json'
        phase = 'copy_disposable_credentials'
        command(['docker', 'cp', container + ':/state/bootstrap.json', str(credentials_path)])
        credentials_path.chmod(0o600)
        credentials = json.loads(credentials_path.read_text())
        check_application_credentials(container)
        command(['docker', 'rm', container])
        opener = build_opener(ProxyHandler({}))
        base = 'http://127.0.0.1:' + str(port)

        def request(route, payload=None):
            headers = {'Authorization': 'Bearer ' + credentials['token'],
                       'X-Organization-ID': credentials['organization_id'],
                       'X-Platform-API-Version': '0.14.0'}
            data = None
            if payload is not None:
                data = json.dumps(payload).encode()
                headers.update({'Content-Type': 'application/json', 'Idempotency-Key': secrets.token_hex(16)})
            with opener.open(Request(base + route, data=data, headers=headers), timeout=3) as response:
                raw = response.read(2 * 1024 * 1024)
                return raw if route == '/' else json.loads(raw)['data']

        def wait_ready():
            deadline = time.monotonic() + 90
            while time.monotonic() < deadline:
                try:
                    if request('/readyz')['status'] == 'ready':
                        return
                except (OSError, ValueError, URLError):
                    pass
                time.sleep(0.5)
            raise RuntimeError('Container API readiness timeout')

        phase = 'verify_workspace_and_isolation'
        wait_ready()
        page = request('/')
        if b'<html' not in page.lower() or b'<script' not in page.lower():
            raise RuntimeError('Workspace not served')
        for asset in re.findall(rb'(?:src|href)="(/assets/[^"]+)"', page):
            with opener.open(base + asset.decode(), timeout=3) as response:
                if response.status != 200 or not response.read(1):
                    raise RuntimeError('Workspace asset missing')
        step('workspace_and_database_readiness')
        for service in ('api', 'worker'):
            identifier = command([*compose, 'ps', '-q', service]).strip()
            check_application_credentials(identifier)
            state = json.loads(command(['docker', 'inspect', identifier]))[0]
            host = state['HostConfig']
            if state['Config']['User'] != '10001:10001' or not host['ReadonlyRootfs'] or 'ALL' not in host['CapDrop']:
                raise RuntimeError('Application isolation setting missing')
        database = command([*compose, 'ps', '-q', 'db']).strip()
        state = json.loads(command(['docker', 'inspect', database]))[0]
        if state['HostConfig'].get('PortBindings'):
            raise RuntimeError('Database must not expose a host port')
        step('non_root_read_only_application_and_private_database')
        phase = 'verify_database_credential_boundaries'
        check_application_credentials(command([*compose, 'ps', '-a', '-q', 'migrate']).strip())
        require_sql('runverge', runtime_password, """
SELECT 'ok' FROM pg_catalog.pg_roles r, pg_catalog.pg_database d
WHERE r.rolname=CURRENT_USER AND r.rolname='runverge' AND r.rolcanlogin
 AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole
 AND NOT r.rolinherit AND NOT r.rolreplication AND NOT r.rolbypassrls
 AND d.datname=CURRENT_DATABASE() AND d.datdba=r.oid
 AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members m WHERE m.member=r.oid);
""", 'ok')
        for role, wrong_password in [('runverge_admin', runtime_password), ('runverge', admin_password)]:
            result = sql(role, wrong_password, 'SELECT 1;')
            if not result.returncode or 'password authentication failed' not in result.stderr.lower():
                raise RuntimeError('Wrong database password was not rejected by authentication')
        for statement in ["SET ROLE runverge_admin;",
                          "SELECT pg_catalog.pg_read_file('/etc/hostname', 0, 128);",
                          "COPY (SELECT 'synthetic') TO PROGRAM 'true';"]:
            require_denied(statement)
        step('distinct_database_passwords_and_actual_privilege_denials')
        phase = 'install_offline_openrouter_profile'
        command([*compose, '--profile', 'admin', 'run', '--rm', '--no-deps', 'admin', 'prepare-model',
                 '--provider', 'openrouter', '--organization-id', credentials['organization_id'],
                 '--model', 'example/fixed-model:free', '--provider-account', 'ci-no-credential',
                 '--price-version', 'zero-filter-v1', '--input-usd-per-million', '0',
                 '--output-usd-per-million', '0', '--max-call-usd', '0.000001', '--out', '/state/openrouter.json'])
        command([*compose, '--profile', 'admin', 'run', '--rm', '--no-deps', 'admin', 'install-model', '--file', '/state/openrouter.json'])
        models = request('/api/v1/models')
        selected = [model for model in models if model['id'] == 'openrouter_text']
        if len(selected) != 1 or selected[0]['model'] != 'example/fixed-model:free' or selected[0]['accounting_mode'] != 'metered' or selected[0]['max_charge_microusd'] != 1:
            raise RuntimeError('Offline OpenRouter profile installation changed limits')
        step('offline_openrouter_profile_install_and_catalogue_without_model_request')
        phase = 'execute_synthetic_task'
        draft = request('/api/v1/agents', {'name': 'Container fixture', 'instructions': 'Synthetic echo fixture.',
                        'model': 'fixture:echo', 'tools': [], 'budget_microusd': 10000})
        agent = request('/api/v1/agents/' + draft['id'] + '/publish', {'expected_version': draft['version']})
        task = request('/api/v1/tasks', {'goal': 'Verify Linux container packaging.',
                       'agent_id': agent['id'], 'budget_microusd': 10000})
        route = '/api/v1/tasks/' + task['id']
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            task = request(route)
            if task['status'] in ('succeeded', 'failed', 'cancelled'):
                break
            time.sleep(0.2)
        artifacts, costs = request(route + '/artifacts'), request(route + '/costs')
        if task['status'] != 'succeeded' or not artifacts or task['spent_microusd'] != '1000':
            raise RuntimeError('Separate worker fixture execution failed')
        step('separate_worker_result_and_synthetic_cost')
        # Stop applications before PostgreSQL; restart without re-running a task.
        phase = 'restart_and_compare'
        command([*compose, 'stop', 'worker', 'api', 'db'])
        command([*compose, 'start', 'db', 'api', 'worker'])
        wait_ready()
        if request(route) != task or request(route + '/artifacts') != artifacts or request(route + '/costs') != costs:
            raise RuntimeError('Restart changed completed task, artifact or cost record')
        if request('/api/v1/models') != models:
            raise RuntimeError('Restart changed installed model configuration')
        step('database_api_worker_restart_retains_result_and_costs')
        # Deliberate faults are confined to this disposable cluster after stopping
        # applications. Readiness does not fence already-running application work.
        phase = 'verify_database_profile_readiness_refusal'
        command([*compose, 'stop', 'worker', 'api'])
        health = ['docker', 'exec', database, '/usr/local/bin/runverge-db-healthcheck']
        command(health)
        require_sql('runverge_admin', admin_password, 'ALTER ROLE runverge SUPERUSER;', 'ALTER ROLE')
        try:
            if subprocess.run(health, env=env, capture_output=True, timeout=15).returncode == 0:
                raise RuntimeError('Readiness accepted an elevated application role')
        finally:
            require_sql('runverge_admin', admin_password, 'ALTER ROLE runverge NOSUPERUSER;', 'ALTER ROLE')
        command(health)
        result = subprocess.run(['docker', 'exec', '-e', 'RUNVERGE_CREDENTIAL_PROFILE=' + secrets.token_hex(16),
                                 database, '/usr/local/bin/runverge-db-healthcheck'],
                                env=env, capture_output=True, timeout=15)
        if result.returncode == 0:
            raise RuntimeError('Readiness accepted settings for another database volume')
        result = subprocess.run(['docker', 'exec', database, 'sh', '-c',
            'set -eu; marker="$PGDATA/runverge-credential-profile"; '
            'mv "$marker" "$marker.check-held"; '
            'trap \'mv "$marker.check-held" "$marker"\' EXIT; '
            'if /usr/local/bin/runverge-db-healthcheck; then exit 1; fi'],
            env=env, capture_output=True, timeout=15)
        if result.returncode:
            raise RuntimeError('Missing database profile marker was not refused')
        command(health)
        step('readiness_refuses_elevated_role_wrong_settings_and_missing_volume_marker')
        phase = 'verify_explicit_container_shutdown'
        verify_recovery_process.verify(command, compose, env, root, project, database)
        step('explicit_container_shutdown_lost_output_inspection_and_stopped_synthetic_sender')
        report['passed'] = True
    except Exception as error:
        report['failure_type'] = type(error).__name__
        report['failure_phase'] = phase
        # Error text is intentionally omitted: external tool messages can contain secrets.
        if attempted:
            try:
                report['service_failures'] = []
                identifiers = command(['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + project]).split()
                for identifier in identifiers:
                    state = json.loads(command(['docker', 'inspect', identifier]))[0]
                    service = state['Config']['Labels'].get('com.docker.compose.service')
                    if service not in ('db', 'migrate', 'api', 'worker', 'admin'):
                        continue
                    logs = command(['docker', 'logs', '--tail', '200', identifier])
                    # Publish only fixed classifications, never log excerpts, names,
                    # environment values, arguments or other free-form container data.
                    report['service_failures'].append({'service': service,
                        'exit_code': state['State']['ExitCode'],
                        'classifications': [label for label in (
                            'permission denied', 'read-only file system', 'native installation',
                            'connection refused', 'password authentication failed',
                            'bootstrap output must be a new private file', 'exec format error',
                            'development auth/fixture profile requires a loopback address',
                            'cannot connect to postgresql'
                        ) if label in logs.lower()]})
            except Exception:
                pass
    finally:
        if attempted:
            try:
                command([*compose, '--profile', 'admin', 'down', '--volumes', '--remove-orphans'])
                auxiliaries = command(['docker', 'ps', '-aq', '--filter', 'label=runverge.check.owner=' + project]).split()
                if auxiliaries:
                    command(['docker', 'rm', '-f', *auxiliaries])
                image_tag = project + '-process-check'
                if command(['docker', 'image', 'ls', '-q', image_tag]).strip():
                    command(['docker', 'image', 'rm', image_tag])
                containers = command(['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + project])
                volumes = command(['docker', 'volume', 'ls', '-q', '--filter', 'label=com.docker.compose.project=' + project])
                remaining_aux = command(['docker', 'ps', '-aq', '--filter', 'label=runverge.check.owner=' + project])
                report['cleanup_complete'] = not containers.strip() and not volumes.strip() and not remaining_aux.strip()
            except Exception:
                report['cleanup_complete'] = False
        else:
            report['cleanup_complete'] = True
        if not report['cleanup_complete']:
            report['passed'] = False
        evidence.parent.mkdir(parents=True, exist_ok=True)
        with evidence.open('x') as stream:
            json.dump(report, stream, indent=2)
            stream.write('\n')
        if report['passed'] and report['cleanup_complete']:
            shutil.rmtree(root)
        else:
            print('Private diagnostics retained at ' + str(root))
    return report['passed']


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--image', default='runverge:linux-preview')
    args = parser.parse_args()
    raise SystemExit(0 if verify(args.evidence, args.image) else 1)

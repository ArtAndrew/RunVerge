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

import server_setup

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
        return result.stdout

    def step(name):
        report['steps'].append({'name': name, 'passed': True})

    try:
        # Refuse any collision before acquiring authority to clean this project.
        for resource, listing in [('container', ['ps', '-aq']), ('volume', ['volume', 'ls', '-q'])]:
            if command(['docker', *listing, '--filter', 'label=com.docker.compose.project=' + project]).strip():
                raise RuntimeError('Disposable project collision: ' + resource)
        server_setup.initialize(root / 'private')
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
            state = json.loads(command(['docker', 'inspect', identifier]))[0]
            host = state['HostConfig']
            if state['Config']['User'] != '10001:10001' or not host['ReadonlyRootfs'] or 'ALL' not in host['CapDrop']:
                raise RuntimeError('Application isolation setting missing')
        database = command([*compose, 'ps', '-q', 'db']).strip()
        state = json.loads(command(['docker', 'inspect', database]))[0]
        if state['HostConfig'].get('PortBindings'):
            raise RuntimeError('Database must not expose a host port')
        step('non_root_read_only_application_and_private_database')
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
        step('database_api_worker_restart_retains_result_and_costs')
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
                    logs = command(['docker', 'logs', identifier])
                    # Publish only fixed classifications, never log excerpts, names,
                    # environment values, arguments or other free-form container data.
                    report['service_failures'].append({'service': service,
                        'exit_code': state['State']['ExitCode'],
                        'classifications': [label for label in (
                            'permission denied', 'read-only file system', 'native installation',
                            'connection refused', 'password authentication failed',
                            'bootstrap output must be a new private file', 'exec format error'
                        ) if label in logs.lower()]})
            except Exception:
                pass
    finally:
        if attempted:
            try:
                command([*compose, '--profile', 'admin', 'down', '--volumes', '--remove-orphans'])
                containers = command(['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + project])
                volumes = command(['docker', 'volume', 'ls', '-q', '--filter', 'label=com.docker.compose.project=' + project])
                report['cleanup_complete'] = not containers.strip() and not volumes.strip()
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

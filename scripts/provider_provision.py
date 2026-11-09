#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Provision one isolated RunVerge Compose installation from a local operator host."""

import argparse
from contextlib import contextmanager
import fcntl
import json
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit
from urllib.request import ProxyHandler, build_opener

import server_setup

REPO = Path(__file__).resolve().parents[1]
TENANT = re.compile(r'[a-z0-9][a-z0-9-]{0,47}')
PROJECT = re.compile(r'runverge-tenant-[a-f0-9]{24}')
IMAGE = re.compile(r'sha256:[a-f0-9]{64}')
DOCKER = ['docker', '--host', 'unix:///var/run/docker.sock']
MAX_BUDGET_MICROUSD = 1_000_000_000_000  # internal/domain.MaxMoney
EXECUTORS = ('api', 'worker')
SETTING_FILES = ('db.env', 'runtime.env', 'model.env')


def require(condition, message):
    if not condition:
        raise ValueError(message)


def private_directory(path):
    info = path.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.geteuid() and not info.st_mode & 0o077,
            'Use an existing private directory owned by the deployment operator')


@contextmanager
def state_lock(path, *, blocking=True):
    private_directory(path)
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        require(os.path.samestat(os.fstat(descriptor), path.lstat()), 'Provider state directory changed')
        fcntl.flock(descriptor, fcntl.LOCK_EX | (0 if blocking else fcntl.LOCK_NB))
        yield
    finally:
        os.close(descriptor)


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def save_plan(path, value):
    fd, staging = tempfile.mkstemp(prefix='.provision-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, indent=2)
            stream.write('\n')
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(staging, path)
        sync_directory(path.parent)
    finally:
        Path(staging).unlink(missing_ok=True)


def read_private_file(path, limit, message):
    descriptor = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    with os.fdopen(descriptor, 'rb') as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
                and not info.st_mode & 0o077 and info.st_nlink == 1 and 0 < info.st_size <= limit, message)
        raw = stream.read(limit + 1)
    require(0 < len(raw) <= limit, message)
    return raw


def read_plan(path):
    value = json.loads(read_private_file(path, 16384, 'Invalid private provisioning record'))
    require(isinstance(value, dict) and PROJECT.fullmatch(value.get('project', ''))
            and value.get('state') in ('pending', 'ready'), 'Invalid private provisioning record')
    if 'lifecycle' in value:
        lifecycle = value['lifecycle']
        require(isinstance(lifecycle, dict)
                and lifecycle.get('state') in ('active', 'suspending', 'suspended', 'resuming', 'deleting', 'deleted')
                and type(lifecycle.get('generation')) is int and lifecycle['generation'] > 0,
                'Invalid private lifecycle record')
    return value


def command(args, env, label, timeout=120):
    result = subprocess.run(args, env=env, text=True, capture_output=True, timeout=timeout)
    require(result.returncode == 0, label + ' failed. Inspect the dedicated Compose project and retry the same request')
    return result.stdout.strip()


def owned_settings(directory):
    if not directory.exists() and not directory.is_symlink():
        with tempfile.TemporaryDirectory(prefix='.settings-', dir=directory.parent) as staging:
            created = Path(staging) / 'private'
            server_setup.initialize(created)
            sync_directory(created)
            created.rename(directory)
            sync_directory(directory.parent)
    private_directory(directory)
    for name in SETTING_FILES:
        info = (directory / name).lstat()
        require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and not info.st_mode & 0o077,
                'Dedicated private database settings are incomplete')


def compose_command(project):
    return [*DOCKER, 'compose', '--env-file', '/dev/null', '--project-name', project,
            '--file', str(REPO / 'deploy/compose.yaml')]


def inspect(kind, identifier, env, *fields):
    template = '[' + ','.join('{{json ' + field + '}}' for field in fields) + ']'
    return json.loads(command([*DOCKER, 'inspect', '--type', kind, '--format', template, identifier],
                              env, 'Inspect dedicated ' + kind))


def placement(plan, compose, env):
    project = plan['project']
    network_name = project + '_default'
    network_id, labels, attached = inspect('network', network_name, env, '.Id', '.Labels', '.Containers')
    require(re.fullmatch(r'[a-f0-9]{64}', network_id) and labels.get('com.docker.compose.project') == project,
            'Dedicated network does not belong to the reserved project')
    services = {}
    states = {}
    for service in ('db', *EXECUTORS):
        container = command([*compose, 'ps', '--all', '--quiet', service], env, 'Locate dedicated ' + service)
        require(re.fullmatch(r'[a-f0-9]{64}', container), 'Dedicated project is missing a unique ' + service)
        status, restart, networks, bindings, image = inspect('container', container, env, '.State.Status',
            '.HostConfig.RestartPolicy.Name', '.NetworkSettings.Networks', '.HostConfig.PortBindings', '.Image')
        require(status in ('running', 'exited') and (service != 'db' or status == 'running'),
                'Dedicated ' + service + ' is not in a supported process state')
        network = networks.get(network_name)
        require(len(networks) == 1 and isinstance(network, dict)
                and network.get('NetworkID') in ((network_id,) if status == 'running' else ('', network_id)),
                'Dedicated container joined an unexpected network')
        expected_bindings = {'8080/tcp': [{'HostIp': '127.0.0.1', 'HostPort': str(plan['port'])}]} if service == 'api' else {}
        require((bindings or {}) == expected_bindings, 'Dedicated container has unexpected port exposure')
        if service != 'db':
            require(image == plan['image_id'], 'Dedicated API or worker uses an unexpected image')
            states[service] = {'status': status, 'restart_policy': restart}
        services[service] = container
    require(set(attached or ()) <= set(services.values()), 'Unexpected process on the dedicated network')
    mounts, = inspect('container', services['db'], env, '.Mounts')
    data = [item for item in mounts if item.get('Destination') == '/var/lib/postgresql']
    require(len(data) == 1 and data[0].get('Type') == 'volume' and data[0].get('Name') == project + '_postgres',
            'Dedicated PostgreSQL does not use its own volume')
    return {'services': services, 'network_id': network_id, 'volume': data[0]['Name']}, states


def ready(url):
    opener = build_opener(ProxyHandler({}))
    deadline = time.monotonic() + 50
    while time.monotonic() < deadline:
        try:
            with opener.open(url + '/readyz', timeout=2) as response:
                if json.loads(response.read(4097))['data']['status'] == 'ready':
                    return
        except (OSError, ValueError, KeyError, TypeError):
            pass
        time.sleep(0.25)
    raise ValueError('Dedicated API did not become ready. Inspect the reserved installation')


def deployment_environment(root, tenant, spec):
    docker_config = root / '.docker'
    private_directory(docker_config)
    return {'PATH': '/usr/local/bin:/usr/bin:/bin', 'DOCKER_CONFIG': str(docker_config),
            'RUNVERGE_IMAGE': spec['image_id'], 'RUNVERGE_PRIVATE_DIR': str(tenant / 'settings'),
            'RUNVERGE_PORT': str(spec['port'])}


def api_url(plan):
    return f"http://127.0.0.1:{plan['port']}"


def lifecycle_state(plan):
    return plan.get('lifecycle', {'state': 'active', 'generation': 0})


def receipt(plan):
    return {'tenant': plan['tenant'], 'state': plan['state'], 'project': plan['project'], 'api_url': api_url(plan),
            'organization_id': plan['organization_id'], 'owner_principal_id': plan['owner_principal_id'],
            'image_id': plan['image_id'], 'lifecycle': lifecycle_state(plan)}


def observe(plan, compose, env):
    actual, states = placement(plan, compose, env)
    require(actual == plan.get('placement'), 'The completed placement changed. Inspect before any recovery operation')
    return states


def state_matches(states, state):
    if state == 'active':
        return all(item == {'status': 'running', 'restart_policy': 'unless-stopped'} for item in states.values())
    if state == 'suspended':
        return all(item == {'status': 'exited', 'restart_policy': 'no'} for item in states.values())
    return False


def provision_spec(tenant, name, port, image, budget=None):
    budget = 100_000_000 if budget is None else budget
    require(isinstance(tenant, str) and TENANT.fullmatch(tenant)
            and isinstance(image, str) and IMAGE.fullmatch(image)
            and type(port) is int and 1024 <= port <= 65535 and type(budget) is int and 0 < budget <= MAX_BUDGET_MICROUSD
            and isinstance(name, str) and name.strip() and len(name.encode()) <= 120 and '\x00' not in name,
            'Provisioning requires a valid name, budget, image ID and loopback port')
    return {'version': 1, 'tenant': tenant, 'name': name, 'port': port,
            'image_id': image, 'budget_microusd': budget}


def provision(root, spec):
    with state_lock(root):
        record = root / spec['tenant']
        record.mkdir(mode=0o700, exist_ok=True)
        private_directory(record)
        plan_file = record / 'provision.json'
        settings = record / 'settings'
        (root / '.docker').mkdir(mode=0o700, exist_ok=True)
        env = deployment_environment(root, record, spec)
        if plan_file.exists() or plan_file.is_symlink():
            plan = read_plan(plan_file)
            require(all(plan.get(key) == value for key, value in spec.items()),
                    'This tenant already has a different immutable provisioning request')
        else:
            for other in root.iterdir():
                if other == record or not TENANT.fullmatch(other.name):
                    continue
                private_directory(other)
                existing = other / 'provision.json'
                if existing.exists() or existing.is_symlink():
                    previous = read_plan(existing)
                    require(lifecycle_state(previous)['state'] == 'deleted' or previous.get('port') != spec['port'],
                            'That loopback port is already reserved')
            project = 'runverge-tenant-' + secrets.token_hex(12)
            for list_args in (['ps', '--all', '--quiet'], ['network', 'ls', '--quiet'], ['volume', 'ls', '--quiet']):
                require(not command([*DOCKER, *list_args, '--filter', 'label=com.docker.compose.project=' + project],
                                    env, 'Reserve new Compose project'), 'Compose project identity already exists')
            plan = dict(spec, project=project, state='pending')
            save_plan(plan_file, plan)
        compose = compose_command(plan['project'])
        url = api_url(plan)

        if plan['state'] == 'ready':
            phase = lifecycle_state(plan)['state']
            require(phase not in ('deleting', 'deleted'), 'This tenant reference is retired')
            states = observe(plan, compose, env)
            if phase in ('active', 'suspended'):
                require(state_matches(states, phase), 'Installation processes differ from the saved lifecycle state')
            if phase == 'active':
                ready(url)
        else:
            require(command([*DOCKER, 'image', 'inspect', '--format', '{{.Id}}', plan['image_id']],
                            env, 'Check pinned local application image') == plan['image_id'],
                    'Pinned local application image changed')
            if not settings.exists():
                require(not command([*compose, 'ps', '--all', '--quiet'], env, 'Check uninitialized project'),
                        'Existing containers need their original private database settings')
                require(not command([*DOCKER, 'volume', 'ls', '--quiet', '--filter',
                                    'label=com.docker.compose.project=' + plan['project']], env, 'Check uninitialized volume'),
                        'Existing volumes need their original private database settings')
            owned_settings(settings)
            command([*compose, 'up', '--detach', '--no-build', '--pull', 'never', '--no-recreate',
                     'db', 'api', 'worker'], env, 'Start dedicated services')
            ready(url)
            physical, states = placement(plan, compose, env)
            require(state_matches(states, 'active'), 'Dedicated services did not become active')
            raw = command([*compose, '--profile', 'admin', 'run', '--no-deps', '--pull', 'never', '--rm',
                           '-T', 'admin', 'bootstrap-dedicated', '--state-dir', '/state',
                           '--name', plan['name'], '--budget-microusd', str(plan['budget_microusd'])],
                          env, 'Initialize dedicated owner')
            owner = json.loads(raw)
            require(re.fullmatch(r'org_[a-f0-9]{32}', owner.get('organization_id', ''))
                    and re.fullmatch(r'usr_[a-f0-9]{32}', owner.get('owner_principal_id', ''))
                    and owner.get('credentials_file') == 'bootstrap.json', 'Invalid dedicated owner receipt')
            plan.update(state='ready', placement=physical, organization_id=owner['organization_id'],
                        owner_principal_id=owner['owner_principal_id'])
            save_plan(plan_file, plan)

        return receipt(plan)


def change_processes(plan, compose, env, state, observed):
    containers = plan['placement']['services']
    targets = [containers[service] for service in EXECUTORS]
    if state == 'suspended':
        command([*DOCKER, 'update', '--restart', 'no', *targets], env, 'Disable automatic executor restart')
        states = observe(plan, compose, env)
        require(all(item['restart_policy'] == 'no' for item in states.values()),
                'Automatic executor restart is still enabled')
        running = [containers[service] for service in EXECUTORS if states[service]['status'] == 'running']
        if running:
            command([*DOCKER, 'stop', '--time', '30', *running], env, 'Stop dedicated API and worker')
    else:
        if observed['api']['status'] != 'running':
            command([*DOCKER, 'start', containers['api']], env, 'Start dedicated API')
        ready(api_url(plan))
        if observed['worker']['status'] != 'running':
            command([*DOCKER, 'start', containers['worker']], env, 'Start dedicated worker')
        command([*DOCKER, 'update', '--restart', 'unless-stopped', *targets], env, 'Restore executor restart policy')
    states = observe(plan, compose, env)
    require(state_matches(states, state), 'Installation processes did not reach the requested lifecycle state')


def export_database_url(record):
    settings = record / 'settings'
    private_directory(settings)
    message = 'Export requires the original private dedicated database settings'
    lines = read_private_file(settings / 'runtime.env', 4096, message).decode('ascii').splitlines()
    urls = [line.removeprefix('DATABASE_URL=') for line in lines if line.startswith('DATABASE_URL=')]
    require(len(urls) == 1 and all(33 <= ord(character) <= 126 for character in urls[0]), message)
    parsed = urlsplit(urls[0])
    require(parsed.scheme == 'postgres' and parsed.username == 'runverge' and parsed.password
            and parsed.netloc.rsplit('@', 1)[-1] == 'db:5432' and parsed.path == '/runverge'
            and parsed.query == 'sslmode=disable' and not parsed.fragment, message)
    return urls[0]


def export_paths(root, keys, output):
    require(keys.is_absolute() and output.is_absolute() and keys.name and output.name,
            'Select absolute archive key and output paths')
    for path in (keys, output):
        private_directory(path.parent)
    keys = keys.parent.resolve() / keys.name
    output = output.parent.resolve() / output.name
    info = keys.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
            and not info.st_mode & 0o077 and info.st_nlink == 1, 'Select a private archive key file')
    require(not output.exists() and not output.is_symlink(), 'Archive output already exists. Inspect it before choosing a new path')
    require(not keys.is_relative_to(output.parent), 'Keep archive keys separate from the output directory parent')
    for reserved in (root.resolve(), REPO.resolve()):
        require(not keys.is_relative_to(reserved) and not output.is_relative_to(reserved)
                and not reserved.is_relative_to(output.parent), 'Keep the archive and keys outside the source and provider registry')
    require(not any(character in str(path) for path in (keys, output.parent) for character in ',"\r\n'),
            'Choose archive paths without commas, quotes or line breaks')
    return keys, output


def export_archive(root, record, plan, env, keys, output):
    keys, output = export_paths(root, keys, output)
    dsn = export_database_url(record)
    with tempfile.TemporaryDirectory(prefix='.runverge-export-', dir=output.parent) as stage:
        # Bind only the selected key file. The placeholder avoids creating a
        # root-owned mount point in the caller's private staging directory.
        Path(stage, 'keys.json').touch(mode=0o600, exist_ok=False)
        raw = command([*DOCKER, 'run', '--rm', '--pull', 'never', '--name', plan['project'] + '-export',
                       '--network', plan['placement']['network_id'], '--read-only', '--cap-drop', 'ALL',
                       '--security-opt', 'no-new-privileges:true', '--pids-limit', '64',
                       '--user', f'{os.geteuid()}:{os.getegid()}', '--env', 'DATABASE_URL',
                       '--mount', f'type=bind,source={stage},target=/export-stage',
                       '--mount', f'type=bind,source={keys},target=/export-stage/keys.json,readonly',
                       '--mount', f'type=bind,source={output.parent},target=/export-output',
                       plan['image_id'], 'recovery', 'backup-dedicated',
                       '--organization-id', plan['organization_id'], '--keys-file', '/export-stage/keys.json',
                       '--staging-parent', '/export-stage', '--out-dir', '/export-output/' + output.name],
                      dict(env, DATABASE_URL=dsn), 'Create dedicated encrypted archive', timeout=1250)
    result = json.loads(raw)
    digest = result.get('receipt_sha256', '')
    require(re.fullmatch(r'[a-f0-9]{64}', digest), 'Archive receipt unavailable. Inspect the private output')
    return {'tenant': plan['tenant'], 'organization_id': plan['organization_id'], 'lifecycle': lifecycle_state(plan),
            'archive_directory': str(output), 'receipt_sha256': digest}


def project_resources(plan, env):
    project = plan['project']
    label = 'label=com.docker.compose.project=' + project
    containers = command([*DOCKER, 'ps', '--all', '--quiet', '--no-trunc', '--filter', label],
                         env, 'List dedicated project containers').splitlines()
    networks = set(command([*DOCKER, 'network', 'ls', '--quiet', '--no-trunc', '--filter', label],
                           env, 'List dedicated project networks').splitlines())
    for line in command([*DOCKER, 'network', 'ls', '--no-trunc', '--format', '{{.ID}} {{.Name}}'],
                        env, 'Inspect the reserved network name').splitlines():
        identifier, name = line.split(' ', 1)
        if name == project + '_default':
            networks.add(identifier)
    volumes = set(command([*DOCKER, 'volume', 'ls', '--quiet', '--filter', label],
                          env, 'List dedicated project volumes').splitlines())
    for name in command([*DOCKER, 'volume', 'ls', '--quiet'], env, 'Inspect reserved volume names').splitlines():
        if name in (project + '_postgres', project + '_admin'):
            volumes.add(name)
    return {'containers': set(containers), 'networks': networks, 'volumes': volumes}


def settings_identity(record):
    path = record / 'settings'
    if not path.exists() and not path.is_symlink():
        return None
    private_directory(path)
    directory = path.lstat()
    files = {}
    for item in path.iterdir():
        info = item.lstat()
        require(item.name in SETTING_FILES and stat.S_ISREG(info.st_mode)
                and info.st_uid == os.geteuid() and not info.st_mode & 0o077 and info.st_nlink == 1,
                'Dedicated settings contain an unexpected file. Inspect before deleting')
        files[item.name] = [info.st_dev, info.st_ino, info.st_ctime_ns]
    return {'directory': [directory.st_dev, directory.st_ino], 'files': files}


def check_deletion_settings(record, saved):
    current = settings_identity(record)
    if current is not None:
        require(current['directory'] == saved['directory']
                and all(saved['files'].get(name) == identity for name, identity in current['files'].items()),
                'Dedicated settings changed during deletion')
    return current


def daemon_id(env):
    value = command([*DOCKER, 'info', '--format', '{{.ID}}'], env, 'Inspect the local Docker daemon identity')
    require(re.fullmatch(r'[a-zA-Z0-9:-]{12,128}', value), 'Docker daemon identity is unavailable')
    return value


def assert_database_scope(plan, db, env):
    org = plan['organization_id']
    require(re.fullmatch(r'org_[a-f0-9]{32}', org), 'Invalid dedicated organization identity')
    # The validated ID is ASCII hex. Credentials stay inside the already
    # selected database container and the query emits only a boolean.
    sql = "SELECT count(*)=1 AND min(id)='" + org + "' FROM public.organizations;"
    psql = ('PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 '
            '-U "$POSTGRES_USER" -d "$POSTGRES_DB" -X -At -v ON_ERROR_STOP=1 -c "$1"')
    require(command([*DOCKER, 'exec', db, '/bin/sh', '-c', psql, 'scope', sql], env,
                    'Check the selected database contains one organization') == 't',
            'Deletion requires exactly the selected organization in the dedicated database')


def check_volume(plan, deletion, name, env, *, mark=False):
    driver, scope, options, labels = inspect('volume', name, env, '.Driver', '.Scope', '.Options', '.Labels')
    labels = labels or {}
    role = name.removeprefix(plan['project'] + '_')
    require(role in ('postgres', 'admin') and driver == 'local' and scope == 'local' and not options
            and labels.get('com.docker.compose.project') == plan['project']
            and labels.get('com.docker.compose.volume') == role,
            'A reserved volume is not the original managed volume')
    mount = f'type=volume,source={name},target=/var/lib/postgresql'
    marker = '/var/lib/postgresql/.runverge-delete-' + deletion['marker']
    if mark:
        operation = ('set -eu; umask 077; if [ "$2" = admin ]; then '
                     'test -f /var/lib/postgresql/bootstrap.json && test ! -L /var/lib/postgresql/bootstrap.json; '
                     r'grep -Fq "\"organization_id\":\"$3\"" /var/lib/postgresql/bootstrap.json; fi; '
                     'mkdir -- "$1"; sync -f /var/lib/postgresql')
    else:
        mount += ',readonly'
        operation = 'set -eu; test -d "$1"; test ! -L "$1"'
    command([*DOCKER, 'run', '--rm', '--pull', 'never', '--network', 'none', '--read-only',
             '--cap-drop', 'ALL', '--cap-add', 'DAC_OVERRIDE', '--security-opt', 'no-new-privileges:true',
             '--user', '0:0', '--entrypoint', '/bin/sh', '--mount', mount, deletion['database_image'],
             '-c', operation, 'volume', marker, role, plan['organization_id']],
            env, 'Mark the selected volume' if mark else 'Verify the original volume marker')


def capture_deletion(record, plan, compose, env):
    require(state_matches(observe(plan, compose, env), 'suspended'), 'Deletion requires confirmed suspended processes')
    resources = project_resources(plan, env)
    placement_ids = plan['placement']['services']
    migrations = resources['containers'] - set(placement_ids.values())
    require(set(placement_ids.values()) <= resources['containers'] and len(migrations) <= 1
            and resources['networks'] == {plan['placement']['network_id']}
            and resources['volumes'] == {plan['placement']['volume'], plan['project'] + '_admin'},
            'Dedicated project has changed resources. Inspect before deleting')
    for identifier in migrations:
        labels, status, image = inspect('container', identifier, env, '.Config.Labels', '.State.Status', '.Image')
        require(labels.get('com.docker.compose.service') == 'migrate' and status == 'exited'
                and image == plan['image_id'], 'Unexpected container in the dedicated project')
    db = placement_ids['db']
    assert_database_scope(plan, db, env)
    database_image, = inspect('container', db, env, '.Image')
    require(IMAGE.fullmatch(database_image), 'The dedicated database image is unavailable')
    settings = settings_identity(record)
    require(settings is not None and set(settings['files']) == set(SETTING_FILES),
            'Original dedicated settings are incomplete')
    deletion = {kind: sorted(ids) for kind, ids in resources.items()}
    deletion.update(daemon=daemon_id(env), database_image=database_image, marker=secrets.token_hex(16),
                    settings=settings)
    # A marker is harmless if the controller stops here. Save the complete
    # marked inventory before the first destructive command.
    for name in deletion['volumes']:
        check_volume(plan, deletion, name, env, mark=True)
    return deletion


def observe_deletion(plan, env):
    deletion = plan['deletion']
    require(daemon_id(env) == deletion['daemon'], 'Docker daemon differs from the saved deletion')
    current = project_resources(plan, env)
    require(all(ids <= set(deletion[kind]) for kind, ids in current.items()),
            'An unexpected resource replaced or joined the saved placement')
    return current


def erase_settings(record, saved):
    current = check_deletion_settings(record, saved)
    if current is None:
        return
    path = record / 'settings'
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        require([info.st_dev, info.st_ino] == current['directory'], 'Dedicated settings directory changed')
        for name in current['files']:
            os.unlink(name, dir_fd=fd)
        os.fsync(fd)
    finally:
        os.close(fd)
    path.rmdir()
    sync_directory(record)


def deletion_receipt(plan):
    return {'tenant': plan['tenant'], 'organization_id': plan['organization_id'], 'project': plan['project'],
            'lifecycle': lifecycle_state(plan)}


def delete_installation(record, path, plan, compose, env, expected, confirmed_org):
    require(confirmed_org == plan['organization_id'], 'Confirm the exact organization ID to delete')
    current = lifecycle_state(plan)
    generation = expected + 1
    if current == {'state': 'deleted', 'generation': generation}:
        require(not any(observe_deletion(plan, env).values()) and settings_identity(record) is None,
                'Retired placement has resources. Inspect it before any other operation')
        return deletion_receipt(plan)
    if current == {'state': 'suspended', 'generation': expected}:
        plan['deletion'] = capture_deletion(record, plan, compose, env)
        plan['lifecycle'] = {'state': 'deleting', 'generation': generation}
        save_plan(path, plan)
    else:
        require(current == {'state': 'deleting', 'generation': generation},
                'Deletion requires the current suspended generation or an exact repeat')

    deletion = plan['deletion']
    remaining = observe_deletion(plan, env)
    check_deletion_settings(record, deletion['settings'])
    for name in remaining['volumes']:
        check_volume(plan, deletion, name, env)
    for identifier in remaining['networks']:
        attached, = inspect('network', identifier, env, '.Containers')
        require(set(attached or ()) <= remaining['containers'], 'Unexpected running process on the dedicated network')
    db = plan['placement']['services']['db']
    for identifier in remaining['containers']:
        status, = inspect('container', identifier, env, '.State.Status')
        if identifier == db and status == 'running':
            assert_database_scope(plan, db, env)
        else:
            require(status == 'exited', 'A process restarted during deletion')
    if db in remaining['containers']:
        command([*DOCKER, 'update', '--restart', 'no', db], env, 'Disable dedicated database restart')
        command([*DOCKER, 'stop', '--time', '30', db], env, 'Stop the dedicated database')
    if remaining['containers']:
        command([*DOCKER, 'rm', *sorted(remaining['containers'])], env, 'Remove exact dedicated containers')
    for identifier in sorted(remaining['networks']):
        command([*DOCKER, 'network', 'rm', identifier], env, 'Remove exact dedicated network')
    for name in sorted(remaining['volumes']):
        command([*DOCKER, 'volume', 'rm', name], env, 'Remove original dedicated volume')
    require(not any(observe_deletion(plan, env).values()), 'Dedicated resources remain after deletion')
    erase_settings(record, deletion['settings'])
    plan['lifecycle'] = {'state': 'deleted', 'generation': generation}
    save_plan(path, plan)
    return deletion_receipt(plan)


def operate(root, tenant, operation, expected, *, keys_file=None, output=None, confirmed_org=None):
    with state_lock(root):
        record = root / tenant
        private_directory(record)
        path = record / 'provision.json'
        plan = read_plan(path)
        require(plan.get('tenant') == tenant and plan['state'] == 'ready', 'Select a completed dedicated installation')
        env = deployment_environment(root, record, plan)
        compose = compose_command(plan['project'])
        current = lifecycle_state(plan)
        if operation == 'delete':
            return delete_installation(record, path, plan, compose, env, expected, confirmed_org)
        if current['state'] in ('deleting', 'deleted'):
            require(operation == 'status', 'This tenant reference is retired')
            remaining = observe_deletion(plan, env)
            return dict(deletion_receipt(plan), remaining_resources={kind: len(ids) for kind, ids in remaining.items()})
        states = observe(plan, compose, env)
        if operation == 'status':
            return dict(receipt(plan), observed_services=states, in_sync=state_matches(states, current['state']))
        if operation == 'export':
            require(current['state'] == 'suspended' and current['generation'] == expected
                    and state_matches(states, 'suspended'), 'Export requires the current confirmed suspension')
            return export_archive(root, record, plan, env, keys_file, output)

        source, transition, target = (('active', 'suspending', 'suspended') if operation == 'suspend'
                                      else ('suspended', 'resuming', 'active'))
        generation = expected + 1
        if current['generation'] == generation and current['state'] == target:
            require(state_matches(states, target), 'Completed lifecycle operation differs from current processes')
            return receipt(plan)
        if current['generation'] == expected and current['state'] == source:
            if source == 'suspended':
                require(state_matches(states, source), 'The suspended installation changed. Inspect before resuming')
            plan['lifecycle'] = {'state': transition, 'generation': generation}
            save_plan(path, plan)
        else:
            require(current['generation'] == generation and current['state'] == transition,
                    'Stale lifecycle generation or wrong starting state. Inspect the installation')

        if not state_matches(states, target):
            change_processes(plan, compose, env, target, states)
        elif target == 'active':
            ready(api_url(plan))
        plan['lifecycle'] = {'state': target, 'generation': generation}
        save_plan(path, plan)
        return receipt(plan)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--state-dir', type=Path, required=True)
    parser.add_argument('--tenant', required=True, help='stable lowercase customer reference')
    parser.add_argument('--operation', choices=('provision', 'status', 'suspend', 'resume', 'export', 'delete'), default='provision')
    parser.add_argument('--expected-generation', type=int, help='generation returned by status before a lifecycle operation')
    parser.add_argument('--keys-file', type=Path, help='private archive key file for export')
    parser.add_argument('--out-dir', type=Path, help='new private archive directory for export')
    parser.add_argument('--confirm-organization-id', help='exact organization ID shown by status for permanent deletion')
    parser.add_argument('--name')
    parser.add_argument('--port', type=int, help='unused loopback port')
    parser.add_argument('--image-id', help='full immutable local Docker image ID')
    parser.add_argument('--budget-microusd', type=int)
    args = parser.parse_args()
    try:
        require(sys.platform == 'linux' and args.state_dir.is_absolute(), 'Use a Linux operator host and an absolute state path')
        require(TENANT.fullmatch(args.tenant), 'Invalid tenant reference')
        if args.operation == 'export':
            require(args.keys_file is not None and args.out_dir is not None, 'Export requires a key file and a new output directory')
        else:
            require(args.keys_file is None and args.out_dir is None, 'Archive paths apply only to export')
        if args.operation == 'delete':
            require(re.fullmatch(r'org_[a-f0-9]{32}', args.confirm_organization_id or ''),
                    'Deletion requires explicit confirmation of the exact organization ID')
        else:
            require(args.confirm_organization_id is None, 'Organization confirmation applies only to deletion')
        if args.operation == 'provision':
            require(args.expected_generation is None, 'Provisioning does not take a lifecycle generation')
            spec = provision_spec(args.tenant, args.name, args.port, args.image_id, args.budget_microusd)
            result = provision(args.state_dir, spec)
        else:
            expected = args.expected_generation
            require(all(value is None for value in (args.name, args.port, args.image_id, args.budget_microusd))
                    and (expected is None if args.operation == 'status' else expected is not None and expected >= 0),
                    'Select only the tenant and the current lifecycle generation for a state change')
            result = operate(args.state_dir, args.tenant, args.operation, expected,
                             keys_file=args.keys_file, output=args.out_dir, confirmed_org=args.confirm_organization_id)
        print(json.dumps(result, sort_keys=True))
        return 0
    except (OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
        message = str(error) if type(error) is ValueError else 'Provisioning did not complete. Retry the same request after inspection'
        print(message, file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())

#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Owned Linux CI only: installed usage commands, synthetic HTTPS, retained DB."""
import base64
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time


def verify(command, env, private, project, identifier, ports, database,
           source_plan, target, image, require, progress):
    # No developer host DNS/trust edits, existing settings, real credentials or
    # live model/provider traffic. Container /etc/hosts overrides only this peer.
    if os.environ.get('GITHUB_ACTIONS') != 'true' or os.environ.get('RUNVERGE_OWNED_USAGE_DRILL') != '1' or os.uname().sysname != 'Linux':
        raise RuntimeError('Installed usage check requires the owned Linux CI host')
    if not re.fullmatch(r'[a-f0-9]{64}', identifier):
        raise RuntimeError('Owned target identifier required')
    fixture = json.loads((private / 'financial-after.json').read_text())
    org, reservation = fixture['usage_organization_id'], fixture['usage_reservation_id']
    if not re.fullmatch(r'org_[a-f0-9]{32}', org) or not re.fullmatch(r'res_[a-f0-9]{32}', reservation):
        raise RuntimeError('Invalid owned usage identifiers')
    runtime_id = command(['docker', 'image', 'inspect', '--format', '{{.Id}}', image]).strip()
    if not re.fullmatch(r'sha256:[a-f0-9]{64}', runtime_id):
        raise RuntimeError('Installed runtime image identity missing')
    peer_dir = private / 'usage-peer'
    peer_dir.mkdir(mode=0o700)
    trust_dir = peer_dir / 'trust'
    trust_dir.mkdir(mode=0o700)
    key_file = peer_dir / 'synthetic-key'
    with key_file.open('x') as stream:
        key_file.chmod(0o600)
        stream.write(secrets.token_hex(32))
    peer_image = project + '-usage-peer'
    peer = None
    image_created = False
    uid = str(os.getuid()) + ':' + str(os.getgid())

    def owned(container):
        state = json.loads(command(['docker', 'inspect', container]))[0]
        if state['Id'] != container or state['Config']['Labels'].get('runverge.check.owner') != project:
            raise RuntimeError('Owned usage resource identity changed')
        return state

    def cli(args, stdout=subprocess.PIPE):
        # The application comes from the same installed image, not a test binary.
        # Host networking is confined to this disposable runner and connects the
        # exact loopback target/peer. Docker socket is not mounted into the CLI.
        container = command(['docker', 'create', '--name', project + '-usage-cli',
                             '--label', 'runverge.check.owner=' + project,
                             '--network', 'host', '--add-host', 'openrouter.ai:127.0.0.1',
                             '--user', uid, '--read-only', '--cap-drop', 'ALL',
                             '--security-opt', 'no-new-privileges',
                             '--env', 'SSL_CERT_FILE=' + str(peer_dir / 'ca.pem'),
                             '--env', 'SSL_CERT_DIR=' + str(trust_dir),
                             '--volume', str(private) + ':' + str(private),
                             runtime_id, 'recovery', *args]).strip()
        if not re.fullmatch(r'[a-f0-9]{64}', container):
            raise RuntimeError('Owned usage CLI identity missing')
        try:
            state = owned(container)
            if state['Image'] != runtime_id or state['HostConfig']['NetworkMode'] != 'host' or state['Config']['User'] != uid or state['HostConfig']['ExtraHosts'] != ['openrouter.ai:127.0.0.1']:
                raise RuntimeError('Installed usage runtime differs')
            return subprocess.run(['docker', 'start', '-a', container], env=env,
                                  stdout=stdout, stderr=subprocess.PIPE, timeout=150)
        finally:
            owned(container)
            command(['docker', 'rm', '-f', container])

    def sql(query):
        return command(['docker', 'exec', identifier, 'sh', '-ec',
                        'export PGPASSWORD="$POSTGRES_PASSWORD"; exec psql -X -w -h 127.0.0.1 '
                        '-U runverge_recovery_operator -d "$1" -At -v ON_ERROR_STOP=1 -c "$2"',
                        'recovery-usage-check', database, query]).strip()

    def database_identity():
        return json.loads(sql("SELECT json_build_object('cluster',system_identifier::text,"
                              "'start',pg_postmaster_start_time()::text,'database',current_database(),"
                              "'schema',current_schema()) FROM pg_control_system()"))

    def request_count():
        files = list(peer_dir.glob('request-*.json'))
        if len(files) != 1 or files[0].name != 'request-1.json' or json.loads(files[0].read_text()) != {'request_count': 1}:
            raise RuntimeError('Installed usage request count differs')

    try:
        progress('prepare_installed_usage_peer')
        command(['docker', 'build', '--target', 'recovery-usage-peer',
                 '--label', 'runverge.check.owner=' + project, '--tag', peer_image,
                 str(Path(__file__).resolve().parents[1])])
        image_created = True
        peer = command(['docker', 'run', '-d', '--name', project + '-usage-peer',
                        '--label', 'runverge.check.owner=' + project, '--network', 'bridge',
                        '--publish', '127.0.0.1:443:8443', '--user', uid,
                        '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                        '--env', 'RUNVERGE_OWNED_RECOVERY_FIXTURE=1',
                        '--volume', str(peer_dir) + ':/peer', peer_image]).strip()
        if not re.fullmatch(r'[a-f0-9]{64}', peer):
            raise RuntimeError('Owned usage peer identity missing')
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            state = owned(peer)
            if not state['State']['Running']:
                raise RuntimeError('Owned usage peer exited')
            if (peer_dir / 'ready').exists():
                break
            time.sleep(0.1)
        else:
            raise RuntimeError('Owned usage peer not ready')
        if state['NetworkSettings']['Ports']['8443/tcp'] != [{'HostIp': '127.0.0.1', 'HostPort': '443'}]:
            raise RuntimeError('Owned usage peer listener is not exact loopback')
        if (peer_dir / 'ready').read_bytes() != b'owned-usage-peer-v1\n' or list(peer_dir.glob('request-*.json')):
            raise RuntimeError('Unexpected peer request before acquisition')
        ca_sha = hashlib.sha256((peer_dir / 'ca.pem').read_bytes()).hexdigest()

        # Compare all target table data except the one intentional observation
        # table. Include financial, execution, authority and other recovery rows.
        tables = sql("SELECT tablename FROM pg_tables WHERE schemaname=current_schema() "
                     "AND tablename<>'recovery_usage_observations' ORDER BY tablename").splitlines()
        if len(tables) < 70 or any(not re.fullmatch(r'[a-z_][a-z0-9_]*', table) for table in tables):
            raise RuntimeError('Incomplete usage baseline inventory')
        baseline_query = 'BEGIN READ ONLY; ' + '; '.join(
            "SELECT '" + table + "' || ':' || encode(sha256(convert_to(COALESCE(jsonb_agg(to_jsonb(r) "
            "ORDER BY to_jsonb(r)::text),'[]'::jsonb)::text,'UTF8')),'hex') FROM " + table + " r"
            for table in tables) + '; ROLLBACK'
        baseline = sql(baseline_query)
        initial_identity = database_identity()
        path = private / 'installed-usage.json'
        plan = require(cli(['usage-plan', '--source-intent-file', str(source_plan),
                            '--target-config', target, '--organization-id', org,
                            '--reservation-id', reservation, '--out', str(path)]), 'installed-usage-plan')
        if plan['state'] != 'request_identity_available' or not plan['execution_held']:
            raise RuntimeError('Installed usage plan unavailable')
        saved = json.loads(path.read_text())
        op = saved['operation_id']
        if not re.fullmatch(r'[a-f0-9]{32}', op):
            raise RuntimeError('Invalid installed usage operation')
        authority = private / 'installed-usage-authority'
        expiry = (datetime.now(timezone.utc) + timedelta(hours=1)).strftime('%Y-%m-%dT%H:%M:%SZ')
        require(cli(['usage-access-init', '--intent-file', str(path), '--authority-dir', str(authority),
                     '--key-file', str(key_file), '--credential-reference', 'owned-installed-key',
                     '--operator-reference', 'owned:installed', '--expires-at', expiry,
                     '--confirm-current-account']), 'installed-usage-authority')
        acquire = ['usage-acquire', '--intent-file', str(path), '--authority-dir', str(authority),
                   '--confirm-read-provider-usage']
        progress('installed_usage_acquire_before_restart')
        with open('/dev/full', 'wb') as lost:
            if cli(acquire, stdout=lost).returncode == 0:
                raise RuntimeError('Installed usage stdout loss not reported')
        request_count()
        acquired = json.loads(Path(str(path) + '.acquisition').read_text())
        raw = base64.b64decode(acquired['response'], validate=True)
        expected = b'{"data":{"id":"gen-installed-usage","model":"example/usage-model","total_cost":0.0000001,"is_byok":true}}'
        if raw != expected or acquired['receipt']['observation']['microusd_status'] != 'fractional' or not acquired['receipt']['observation']['is_byok']:
            raise RuntimeError('Installed usage response representation changed')
        inspection = ['usage-inspect', '--intent-file', str(path)]
        before = require(cli(inspection), 'installed-usage-before-restart')
        if before['state'] != 'recorded' or before['observation_state'] != 'observed_unverified' or before['operation_id'] != op or before['acquisition_sha256'] != acquired['receipt']['receipt_sha256'] or before['response_sha256'] != hashlib.sha256(raw).hexdigest() or not before['execution_held']:
            raise RuntimeError('Installed observation identity differs')
        row_query = "SELECT encode(sha256(convert_to(to_jsonb(r)::text,'UTF8')),'hex') FROM recovery_usage_observations r WHERE id='" + op + "'"
        row_hash = sql(row_query)
        if not re.fullmatch(r'[a-f0-9]{64}', row_hash) or sql('SELECT count(*) FROM recovery_usage_observations') != '1':
            raise RuntimeError('Installed observation row missing or duplicated')
        if sql(baseline_query) != baseline:
            raise RuntimeError('Usage acquisition changed unrelated target rows')
        if owned(identifier)['NetworkSettings']['Ports']['5432/tcp'] != ports:
            raise RuntimeError('Owned usage target endpoint changed before restart')
        command(['docker', 'restart', identifier])
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            ready = subprocess.run(['docker', 'exec', identifier, 'pg_isready', '-h', '127.0.0.1',
                                    '-U', 'runverge_recovery_operator', '-d', database],
                                   env=env, capture_output=True, timeout=10)
            if ready.returncode == 0:
                break
            time.sleep(0.2)
        else:
            raise RuntimeError('Usage target did not restart')
        target_state = owned(identifier)
        if target_state['NetworkSettings']['Ports']['5432/tcp'] != ports:
            raise RuntimeError('Usage target endpoint changed')
        after_identity = database_identity()
        if after_identity['start'] == initial_identity['start'] or any(after_identity[k] != initial_identity[k] for k in ('cluster', 'database', 'schema')):
            raise RuntimeError('Usage target restart identity differs')
        require(cli(['usage-access-revoke', '--authority-dir', str(authority), '--expected-epoch', '1',
                     '--operator-reference', 'owned:installed-revoke', '--confirm-revoke']), 'installed-usage-revoke')
        if json.loads((authority / 'access.json').read_text()).get('usage_credential'):
            raise RuntimeError('Revoked authority retained its credential')
        key_file.unlink()
        if require(cli(inspection), 'installed-usage-after-restart') != before:
            raise RuntimeError('Installed usage changed across restart and revocation')
        if cli(acquire).returncode == 0:
            raise RuntimeError('Installed usage attempt replayed')
        if sql(row_query) != row_hash or sql(baseline_query) != baseline or sql("SELECT state FROM runtime_recovery_control WHERE singleton") != 'held':
            raise RuntimeError('Installed usage data or execution hold changed')
        if hashlib.sha256((peer_dir / 'ca.pem').read_bytes()).hexdigest() != ca_sha:
            raise RuntimeError('Owned usage trust root changed')
        request_count()
        progress('installed_usage_acquisition_restart_revoke_inspect_passed')
    finally:
        if peer and re.fullmatch(r'[a-f0-9]{64}', peer):
            owned(peer)
            command(['docker', 'rm', '-f', peer])
        if image_created:
            state = json.loads(command(['docker', 'image', 'inspect', peer_image]))[0]
            if state['Config']['Labels'].get('runverge.check.owner') != project:
                raise RuntimeError('Owned usage image identity changed')
            command(['docker', 'image', 'rm', peer_image])

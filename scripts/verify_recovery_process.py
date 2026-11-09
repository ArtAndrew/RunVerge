#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Exercise container shutdown only inside the caller's disposable Compose project."""
import hashlib
import json
import os
import re
import secrets
import socket
from pathlib import Path
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlsplit, urlunsplit
from urllib.request import ProxyHandler, Request, build_opener

import verify_recovery_usage


def verify(command, compose, env, root, project, database, runtime_url, image, progress):
    # The parent created this exact project and owns its cleanup. No saved host
    # settings, user project, real model, tokens or business payloads are used.
    # The preceding readiness fault checks deliberately stopped both runtimes.
    progress('start_source_witness_runtimes')
    command([*compose, 'start', 'api', 'worker'])
    binary = root / 'platform-process-check'
    api = command([*compose, 'ps', '-a', '-q', 'api']).strip()
    command(['docker', 'cp', api + ':/app/bin/platform', str(binary)])
    binary.chmod(0o700)
    backup = root / 'transport-exclusion'
    backup.mkdir(mode=0o700)
    intent = root / 'private/container-stop.json'
    counter = {'value': 0}
    lock = threading.Lock()

    class Receiver(BaseHTTPRequestHandler):
        def do_POST(self):
            if self.path != '/synthetic':
                self.send_error(404)
                return
            with lock:
                counter['value'] += 1
            self.send_response(204)
            self.end_headers()

        def log_message(self, *args):
            pass

    # CI-only synthetic receiver, reachable from the disposable Docker bridge.
    # The check never exposes a RunVerge API or a secret on this listener.
    server = ThreadingHTTPServer(('0.0.0.0', 0), Receiver)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    sender = None
    control = None
    sender_image = project + '-process-check'
    image_created = False

    def cli(args, stdout=subprocess.PIPE):
        return subprocess.run([str(binary), 'recovery', *args], env=env, stdout=stdout,
                              stderr=subprocess.PIPE, timeout=150)

    def value():
        with lock:
            return counter['value']

    try:
        # Reuse the pinned Node build stage already built for Workspace. Its image
        # declares no automatic volumes and supplies a synthetic HTTP sender.
        command(['docker', 'build', '--target', 'web-dependencies', '--tag', sender_image,
                 str(Path(__file__).resolve().parents[1])])
        image_created = True
        # A synthetic process keeps sending without using PostgreSQL. This models
        # why disconnecting a DB alone cannot stop a cached external dispatch.
        sender = command(['docker', 'create', '--name', project + '-synthetic-sender',
                          '--label', 'com.docker.compose.project=' + project,
                          '--label', 'runverge.check.owner=' + project,
                          '--label', 'com.docker.compose.service=worker',
                          '--network', project + '_default',
                          '--add-host', 'host.docker.internal:host-gateway',
                          '--user', '10001:10001', '--read-only', '--cap-drop', 'ALL',
                          '--restart', 'always', '--entrypoint', 'node', sender_image, '-e',
                          'setInterval(()=>{require("http").request({hostname:"host.docker.internal",'
                          'port:' + str(server.server_port) + ',method:"POST",path:"/synthetic"})'
                          '.on("error",()=>{}).end()},100)']).strip()
        command(['docker', 'start', sender])
        control = command(['docker', 'create', '--name', project + '-unrelated-control',
                           '--label', 'com.docker.compose.project=' + project + '-other',
                           '--label', 'runverge.check.owner=' + project,
                           '--network', 'none', '--user', '10001:10001', '--read-only',
                           '--cap-drop', 'ALL', '--entrypoint', 'node', sender_image,
                           '-e', 'setInterval(()=>{},1000)']).strip()
        command(['docker', 'start', control])
        command([*compose, 'start', 'api', 'worker'])
        deadline = time.monotonic() + 15
        while value() < 3 and time.monotonic() < deadline:
            time.sleep(0.1)
        if value() < 3:
            raise RuntimeError('Synthetic external sender did not start')
        plan_args = ['container-stop-plan', '--docker-socket', '/var/run/docker.sock',
                     '--project', project, '--backup-dir', str(backup), '--out', str(intent)]
        result = cli(plan_args)
        if result.returncode:
            raise RuntimeError('Container shutdown plan failed')
        plan = json.loads(result.stdout)['intent']
        bound_ids = {item['id'] for item in plan['containers']}
        if sender not in bound_ids or database not in bound_ids or api not in bound_ids:
            raise RuntimeError('Shutdown plan omitted a disposable service')
        before = value()
        time.sleep(0.3)
        if value() <= before:
            raise RuntimeError('Planning unexpectedly stopped the sender')
        apply_args = ['container-stop-apply', '--intent-file', str(intent),
                      '--expected-project', project, '--confirm-stop-project']
        wrong = cli(['container-stop-apply', '--intent-file', str(intent),
                     '--expected-project', project + '-wrong', '--confirm-stop-project'])
        if wrong.returncode == 0 or Path(str(intent) + '.apply').exists():
            raise RuntimeError('Wrong-project confirmation was accepted')
        # Lose stdout after the actual stop. A fresh command must inspect the
        # saved intent; a repeated apply must be refused by its durable marker.
        with open('/dev/full', 'wb') as unavailable_output:
            lost = cli(apply_args, stdout=unavailable_output)
        if lost.returncode == 0:
            raise RuntimeError('Lost command output was not reported')
        inspected = cli(['container-stop-inspect', '--intent-file', str(intent)])
        if inspected.returncode:
            raise RuntimeError('Fresh-process shutdown inspection failed')
        observed = json.loads(inspected.stdout)
        if not observed['project_processes_stopped'] or any(observed[key] for key in (
                'source_installation_bound', 'external_execution_fenced', 'external_effects_settled')):
            raise RuntimeError('Shutdown observation lost its bounded scope')
        if {item['id'] for item in observed['containers']} != bound_ids:
            raise RuntimeError('Shutdown inventory changed')
        if cli(apply_args).returncode == 0:
            raise RuntimeError('Already attempted shutdown was silently repeated')
        time.sleep(0.3)  # allow receiver threads for already-delivered requests to finish
        after = value()
        time.sleep(1)
        if value() != after:
            raise RuntimeError('Synthetic sender continued after process shutdown')
        if not json.loads(command(['docker', 'inspect', control]))[0]['State']['Running']:
            raise RuntimeError('Shutdown affected a different project')
        # No volume was deleted. Inspecting a restarted bound container must
        # report non-stopped, even with its automatic restart policy disabled.
        command(['docker', 'start', sender])
        inspected = cli(['container-stop-inspect', '--intent-file', str(intent)])
        if inspected.returncode == 0 and json.loads(inspected.stdout)['project_processes_stopped']:
            raise RuntimeError('Inspection accepted a manually restarted source process')
    finally:
        cleanup_errors = []
        for identifier in (sender, control):
            if identifier:
                try:
                    command(['docker', 'rm', '-f', identifier])
                except Exception as error:
                    cleanup_errors.append(type(error).__name__)
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
        if image_created:
            try:
                command(['docker', 'image', 'rm', sender_image])
            except Exception as error:
                cleanup_errors.append(type(error).__name__)
        if cleanup_errors:
            raise RuntimeError('Synthetic shutdown resources were not fully removed')

    # Run the generic sender drill before permanent retirement so the original
    # fixture can still start normally for source binding and evidence capture.
    command([*compose, 'start', '--wait', '--wait-timeout', '45', 'db', 'api', 'worker'])
    verify_source_binding(command, compose, env, root, project, database, api,
                          binary, runtime_url, image, progress)


def verify_source_binding(command, compose, env, root, project, database, api,
                          binary, runtime_url, image, progress):
    """Installed binary and actual PID1 pools; all inputs belong to this fixture."""
    progress('prepare_private_source_fixture')
    private = root / 'source-proof'
    private.mkdir(mode=0o700)
    stage = private / 'stage'
    stage.mkdir(mode=0o700)
    # Create the archive through the existing operator CLI, in a private host
    # directory mounted into a disposable admin container. Do not copy private
    # inputs into the read-only application runtime through Docker's archive API.
    # Matching the host UID preserves private file ownership without chmod 777.
    config = private / 'database.json'
    with config.open('x') as stream:
        config.chmod(0o600)
        json.dump({'database_url': runtime_url}, stream)
    admin = [*compose, '--profile', 'admin', 'run', '--rm', '--no-deps',
             '--user', str(os.getuid()) + ':' + str(os.getgid()),
             '--volume', str(private) + ':/source', 'admin', 'recovery']
    # This opt-in seed executable is never included in the installed runtime.
    # Its only writes use ordinary accounting APIs on this newly owned project.
    seed_image = project + '-financial-seed'
    command([*compose, 'stop', 'worker'])
    progress('prepare_owned_financial_source')
    command(['docker', 'build', '--target', 'recovery-fixture-runtime',
             '--label', 'runverge.check.owner=' + project, '--tag', seed_image,
             str(Path(__file__).resolve().parents[1])])

    def seed(phase, output, previous=None):
        args = ['docker', 'run', '--rm', '--name', project + '-financial-seed',
                '--label', 'runverge.check.owner=' + project,
                '--network', project + '_default', '--read-only', '--cap-drop', 'ALL',
                '--user', str(os.getuid()) + ':' + str(os.getgid()),
                '--env', 'RUNVERGE_OWNED_RECOVERY_FIXTURE=1',
                '--volume', str(private) + ':/source', seed_image,
                '--phase', phase, '--database-config', '/source/database.json',
                '--out', '/source/' + output]
        if previous:
            args += ['--state', '/source/' + previous]
        command(args)

    seed('prepare', 'financial-before.json')
    progress('source_backup_keygen')
    command([*admin, 'backup-keygen', '--out', '/source/keys.json'])
    progress('source_backup_create')
    receipt = json.loads(command([*admin, 'backup-create',
                         '--database-config', '/source/database.json',
                         '--keys-file', '/source/keys.json', '--staging-parent', '/source/stage',
                         '--out-dir', '/source/backup']))
    # A real late-only organization must survive as evidence, without becoming
    # active business data in the target selected from the older backup.
    command([*admin[:-1], 'bootstrap', '--name', 'Synthetic post-backup organization',
             '--out', '/source/late-owner.json'])
    seed('append', 'financial-after.json', 'financial-before.json')
    command(['docker', 'image', 'rm', seed_image])
    command([*compose, 'start', 'worker'])
    image_id = command(['docker', 'image', 'inspect', '--format', '{{.Id}}', image]).strip()
    source_id = receipt['manifest']['metadata']['source_id']

    def cli(args, stdout=subprocess.PIPE):
        return subprocess.run([str(binary), 'recovery', *args], env=env, stdout=stdout,
                              stderr=subprocess.PIPE, timeout=150)

    def require(result, label):
        if result.returncode:
            fixed = ('exec_create', 'exec_request', 'exec_start', 'exec_stream', 'exec_identity',
                     'exec_exit', 'exec_timeout', 'exec_output', 'database_reply', 'container_inventory', 'image_id', 'dns', 'active_exec', 'auxiliary_state', 'runtime_state',
                     'network_endpoint', 'network_address', 'network_identity', 'database_image',
                     'runtime_image', 'api_command', 'worker_command', 'worker_concurrency',
                     'environment_shape', 'environment_duplicate', 'pg_environment',
                     'admin_environment', 'database_url', 'database_password', 'service_count',
                     'network_inspection', 'network_members', 'network_endpoint_match',
                     'database_observation', 'installation_identity', 'database_peer',
                     'challenge_not_observed', 'runtime_witness_response')
            code = next((name for name in fixed if ('(source:' + name + ')').encode() in result.stderr), 'unclassified')
            if code == 'unclassified':
                reasons = {
                    b'explicit recovery target unavailable': 'target_unavailable',
                    b'recovery evidence not accepted': 'evidence_rejected',
                    b'unsupported recovery semantic catalogue': 'target_catalogue',
                    b'explicit private source and target selectors required': 'private_selectors',
                }
                code = next((label for message, label in reasons.items() if message in result.stderr), code)
            progress(label + '_failed_' + code)
            # Private stderr stays local; no raw archive/settings/secret output.
            diagnostic = private / (label + '.stderr')
            diagnostic.write_bytes(result.stderr)
            diagnostic.chmod(0o600)
            raise RuntimeError('Source binding fixture failed: ' + label)
        return json.loads(result.stdout)

    common = ['--docker-socket', '/var/run/docker.sock', '--project', project,
              '--backup-dir', str(private / 'backup'), '--keys-file', str(private / 'keys.json'),
              '--staging-parent', str(stage), '--expected-receipt-sha256', receipt['receipt_sha256'],
              '--expected-runtime-image', image_id]
    # An independently running exec is outside this binding's own challenge set.
    progress('reject_unrelated_exec')
    unrelated = subprocess.Popen(['docker', 'exec', api, '/app/bin/platform', 'recovery',
                                 'source-witness', '--expected-installation', source_id,
                                 '--nonce', 'e' * 32, '--role', 'api'], env=env,
                                 stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    try:
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if json.loads(command(['docker', 'inspect', api]))[0].get('ExecIDs'):
                break
            if unrelated.poll() is not None:
                raise RuntimeError('Independent live witness failed to start')
            time.sleep(0.05)
        else:
            raise RuntimeError('Independent live exec not observable')
        refused = private / 'unknown-exec.json'
        if cli(['source-stop-plan', *common, '--out', str(refused)]).returncode == 0 or refused.exists():
            raise RuntimeError('Unrelated exec admitted into source binding')
        unrelated.communicate(timeout=35)
        if unrelated.returncode:
            raise RuntimeError('Independent live witness did not finish')
    finally:
        if unrelated.poll() is None:
            unrelated.kill()
            unrelated.communicate(timeout=5)
    progress('plan_before_restart')
    old = private / 'before-restart.json'
    initial = require(cli(['source-stop-plan', *common, '--out', str(old)]), 'plan-before-restart')
    binding = initial['source_binding']
    if binding['source_id'] != source_id or len(binding['processes']) != 2:
        raise RuntimeError('Live source binding omitted PID1 processes')
    # A stopped/started container has the same ID and image but a different boot.
    progress('restart_invalidates_binding')
    command([*compose, 'restart', 'api'])
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if json.loads(command(['docker', 'inspect', api]))[0]['State'].get('Health', {}).get('Status') == 'healthy':
            break
        time.sleep(0.2)
    else:
        raise RuntimeError('Source API did not restart')
    apply = ['source-stop-apply', '--intent-file', str(old), '--expected-project', project,
             '--confirm-stop-project']
    if cli(apply).returncode == 0 or Path(str(old) + '.apply').exists():
        raise RuntimeError('Old source process plan survived restart')
    if cli(['container-stop-apply', '--intent-file', str(old), '--expected-project', project,
            '--confirm-stop-project']).returncode == 0:
        raise RuntimeError('Unbound command bypassed source revalidation')
    progress('plan_current')
    current = private / 'source-stop.json'
    require(cli(['source-stop-plan', *common, '--out', str(current),
                 '--evidence-dir', str(private / 'late-evidence')]), 'plan-current')
    saved = json.loads(current.read_text())
    # A wrong installation binding cannot authorize shutdown, even when all
    # container IDs and the authenticated snapshot digest are otherwise exact.
    progress('reject_wrong_source')
    changed = private / 'wrong-source.json'
    saved['source_binding']['source_id'] = 'inst_' + ('f' if source_id != 'inst_' + 'f' * 32 else 'e') * 32
    changed.write_text(json.dumps(saved))
    changed.chmod(0o600)
    if cli(['source-stop-apply', '--intent-file', str(changed), '--expected-project', project,
            '--confirm-stop-project']).returncode == 0 or Path(str(changed) + '.apply').exists():
        raise RuntimeError('Wrong source installation authorized a stop')
    apply = ['source-stop-apply', '--intent-file', str(current), '--expected-project', project,
             '--confirm-stop-project']
    progress('apply_with_lost_output')
    with open('/dev/full', 'wb') as lost_output:
        if cli(apply, stdout=lost_output).returncode == 0:
            raise RuntimeError('Lost source-stop output not reported')
    progress('inspect_stopped')
    observed = require(cli(['source-stop-inspect', '--intent-file', str(current)]), 'inspect-stopped')
    if not observed['source_installation_bound'] or not observed['bound_source_processes_stopped'] or observed['source_id'] != source_id or observed['external_execution_fenced'] or observed['external_effects_settled']:
        raise RuntimeError('Bound source shutdown scope incorrect')
    if not observed.get('evidence_captured') or observed.get('evidence_reconciled'):
        raise RuntimeError('Source evidence capture not authenticated after lost output')
    # The installed producer must attest its drained export, not upgrade a V1
    # pairing or infer quiescence from the later final-stop observation.
    capture = json.loads(Path(str(current) + '.capture').read_text())['link']
    state_raw = Path(str(current) + '.capture-state').read_bytes()
    state = json.loads(state_raw)
    saved = json.loads(current.read_text())
    binding = saved['source_binding']
    late_manifest = json.loads((private / 'late-evidence' / 'receipt.json').read_text())
    if (capture['version'] != 2 or capture['export_algorithm'] != 'exclusive-compose-pg-drain-nonce-rr-export-v1'
            or capture['system_id'] != binding['system_id'] or capture['database_oid'] != binding['database_oid']
            or capture['export_wire_bytes'] != late_manifest['plaintext_bytes']
            or capture['evidence_snapshot_sha256'] != late_manifest['plaintext_sha256']
            or capture['evidence_schema_sha256'] != late_manifest['metadata']['schema_sha256']
            or capture['evidence_backup_id'] != late_manifest['metadata']['backup_id']
            or capture['stopped_executors_sha256'] != hashlib.sha256(state_raw).hexdigest()
            or capture['executors_stopped_at_utc'] != state['observed_at']
            or not state['source_installation_bound'] or state['project_processes_stopped']
            or state['external_execution_fenced'] or state['external_effects_settled']):
        raise RuntimeError('Drained capture provenance differs from installed export')
    for container in state['containers']:
        if container['restart_policy'] != 'no' or container['stopped'] != (container['service'] != 'db'):
            raise RuntimeError('Capture observation did not keep only the database running')
    progress('installed_drained_capture_v2_passed')
    stop_proof = str(current) + '.source-stop'
    checked = require(cli(['source-stop-inspect', '--intent-file', str(current),
                           '--source-stop-file', stop_proof]), 'source-stop-proof-recheck')
    if checked.get('source_stop_sha256') != hashlib.sha256(Path(stop_proof).read_bytes()).hexdigest() or checked.get('window_covered') or checked.get('new_work_allowed'):
        raise RuntimeError('Signed source stop proof scope or digest incorrect')
    # A partial receipt is not overwritten or treated as permission to stop again.
    partial = private / 'partial-source-stop.json'
    partial.write_bytes(b'{')
    partial.chmod(0o600)
    if cli(['source-stop-inspect', '--intent-file', str(current),
            '--source-stop-out', str(partial)]).returncode == 0 or partial.read_bytes() != b'{':
        raise RuntimeError('Partial source stop proof was overwritten')
    fresh_proof = private / 'fresh-source-stop.json'
    require(cli(['source-stop-inspect', '--intent-file', str(current),
                 '--source-stop-out', str(fresh_proof)]), 'source-stop-proof-refresh')
    require(cli(['source-stop-inspect', '--intent-file', str(current),
                 '--source-stop-file', str(fresh_proof)]), 'fresh-source-stop-proof-recheck')
    for archive in (private / 'backup', private / 'late-evidence'):
        forbidden = archive / 'source-stop.json'
        if cli(['source-stop-inspect', '--intent-file', str(current),
                '--source-stop-out', str(forbidden)]).returncode == 0 or forbidden.exists():
            raise RuntimeError('Source stop proof entered a transported archive')
    if cli(apply).returncode == 0:
        raise RuntimeError('Source shutdown repeated')
    progress('signed_source_stop_refresh_passed')
    def finish_source_checks(maintenance, target_container, ports):
        verify_retained_source_restart(command, compose, database, source_id, current, stop_proof, cli, progress)
        verify_retired_source(command, compose, env, private, project, database, common,
                              maintenance, target_container, ports, image, cli, require, progress)

    verify_held_evidence(command, env, private, project, database, current,
                         receipt, cli, require, progress, image, finish_source_checks)


def verify_retained_source_restart(command, compose, database, source_id, current, stop_proof, cli, progress):
    # No volume replacement: same DB container and data are restarted solely
    # inside this disposable verifier for the existing synthetic-sender check.
    progress('restart_retained_source_database')
    command([*compose, 'start', 'db'])
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if json.loads(command(['docker', 'inspect', database]))[0]['State'].get('Health', {}).get('Status') == 'healthy':
            break
        time.sleep(0.2)
    else:
        raise RuntimeError('Stopped source database did not restart')
    command([*compose, 'start', 'api', 'worker'])
    retained_source = command(['docker', 'exec', database, 'sh', '-ec',
                               'export PGPASSWORD="$POSTGRES_PASSWORD"; exec psql -X -w '
                               '-h 127.0.0.1 -U runverge_admin -d runverge -At '
                               '-v ON_ERROR_STOP=1 -c "SELECT installation_id FROM public.runtime_recovery_control WHERE singleton"']).strip()
    if retained_source != source_id:
        raise RuntimeError('Source data identity changed during stop/restart')
    # The historical signature remains a record, but it cannot serve as fresh
    # observed-stop evidence once the original installation has restarted.
    if cli(['source-stop-inspect', '--intent-file', str(current),
            '--source-stop-file', stop_proof]).returncode == 0:
        raise RuntimeError('Source stop proof survived source restart')
    progress('signed_source_stop_restart_refusal_passed')
    progress('bound_source_check_passed')


def verify_retired_source(command, compose, env, private, project, database, common,
                          maintenance, target_container, ports, image, cli, require, progress):
    """Retire the restarted owned source and consume exactly one held slot."""
    command([*compose, 'start', '--wait', '--wait-timeout', '45', 'api', 'worker'])
    intent, late = private / 'retired-source.json', private / 'retired-late-evidence'
    stopped = require(cli(['source-stop-plan', *common, '--out', str(intent), '--evidence-dir', str(late),
                           '--retire-to-config', str(maintenance)]), 'retirement-plan')
    destination = stopped['retire_to']
    common_stop = ['--intent-file', str(intent), '--expected-project', project, '--confirm-stop-project']
    if cli(['source-stop-apply', *common_stop]).returncode == 0 or Path(str(intent) + '.apply').exists():
        raise RuntimeError('Retirement proceeded without its separate consent')

    # Force a local observation-write failure after a real committed retirement.
    # The fresh finish command must read PostgreSQL and never repeat the DDL.
    report = Path(str(intent) + '.retirement')
    seal = Path(str(intent) + '.retirement-seal')
    report.write_bytes(b'{}')
    report.chmod(0o600)
    if cli(['source-stop-apply', *common_stop, '--confirm-retire-source',
            '--expected-restore-id', destination['restore_id']]).returncode == 0:
        raise RuntimeError('Conflicting local retirement output was accepted')
    if (report.read_bytes() != b'{}' or seal.exists()
            or not json.loads(command(['docker', 'inspect', database]))[0]['State']['Running']):
        raise RuntimeError('Retirement failure overwrote evidence or stopped PostgreSQL')
    report.rename(private / 'occupied-retirement-output.json')
    # Stop completes but the original path for its signed receipt is occupied.
    occupied_stop = Path(str(intent) + '.source-stop')
    occupied_stop.write_bytes(b'{}')
    occupied_stop.chmod(0o600)
    if cli(['source-stop-finish-retired', *common_stop]).returncode == 0 or occupied_stop.read_bytes() != b'{}':
        raise RuntimeError('Conflicting final stop output was accepted or overwritten')
    if json.loads(command(['docker', 'inspect', database]))[0]['State']['Running'] or not seal.is_file():
        raise RuntimeError('Final output fault did not leave a stopped source and preserved seal')
    prior = json.loads(report.read_text())
    if (prior['state'] != 'source_admission_retired' or prior['destination'] != destination
            or prior['source_connections_allowed'] or prior['application_login_allowed']
            or not prior['maintenance_owns_database'] or prior['sessions'] or prior['prepared_transactions']
            or len(prior.get('cut_proof_sha256', '')) != 64):
        raise RuntimeError('Final retirement omitted the admission barriers or source data proof')
    proof = str(private / 'retired-recovered-source-stop.json')
    stop = require(cli(['source-stop-inspect', '--intent-file', str(intent), '--source-stop-out', proof]), 'retired-stop-recover')
    stop_record = json.loads(Path(proof).read_text())['receipt']
    seal_sha = hashlib.sha256(seal.read_bytes()).hexdigest()
    if stop_record['version'] != 2 or stop_record['retirement_seal_sha256'] != seal_sha:
        raise RuntimeError('Recovered stop receipt omitted the exact retirement seal')
    if not stop['bound_source_processes_stopped'] or stop['new_work_allowed']:
        raise RuntimeError('Retired source was not observed stopped or hold was removed')
    unavailable = seal.with_suffix('.retirement-seal-kept')
    seal.rename(unavailable)
    try:
        if cli(['source-stop-inspect', '--intent-file', str(intent), '--source-stop-file', proof]).returncode == 0:
            raise RuntimeError('Retired stop verified without its signed seal')
    finally:
        unavailable.rename(seal)
    require(cli(['source-stop-inspect', '--intent-file', str(intent), '--source-stop-file', proof]), 'retired-stop-recheck')
    if cli(['source-stop-finish-retired', *common_stop]).returncode == 0:
        raise RuntimeError('Retired source finalization repeated')

    capture = json.loads(Path(str(intent) + '.capture').read_text())['link']
    selected = ['--backup-dir', str(late), '--keys-file', str(private / 'keys.json'), '--staging-parent', str(private / 'stage')]
    plan = private / 'retired-provision.json'
    planning = ['provision-plan', *selected, '--expected-receipt-sha256', capture['evidence_receipt_sha256'],
                '--handoff-source-intent-file', str(intent), '--handoff-source-stop-file', proof]
    config = json.loads(maintenance.read_text())['database_url']
    uri = urlsplit(config)
    wrong = private / 'wrong-retirement-maintenance.json'
    with wrong.open('x') as out:
        os.fchmod(out.fileno(), 0o600)
        json.dump({'database_url': urlunsplit(uri._replace(path='/template1'))}, out)
    if cli([*planning, '--admin-config', str(wrong), '--out', str(plan)]).returncode == 0 or plan.exists():
        raise RuntimeError('Retired handoff adopted a different maintenance database')
    planned = require(cli([*planning, '--admin-config', str(maintenance), '--out', str(plan)]), 'retired-provision-plan')
    if planned['operation_id'] != destination['restore_id'] or planned['wal_timeline'] != destination['wal_timeline']:
        raise RuntimeError('Retired handoff lost the reserved destination slot')
    retirement = json.loads(plan.read_text())['handoff']['retirement']
    if retirement['cut_proof_sha256'] != prior['cut_proof_sha256'] or retirement['seal_sha256'] != seal_sha:
        raise RuntimeError('Held handoff lost the source data proof or seal reference')
    require(cli(['provision-apply', '--intent-file', str(plan), '--expected-database', planned['database'],
                 '--confirm-create-target']), 'retired-provision-apply')
    target = str(plan) + '.target.json'
    require(cli(['archive-import', '--target-config', target, *selected]), 'retired-import')
    held = require(cli(['import-inspect', '--target-config', target]), 'retired-import-inspect')
    if held['state'] != 'committed_held':
        raise RuntimeError('Reserved retired destination was not imported under hold')
    review = require(cli(['handoff-review', '--target-config', target]), 'retired-handoff-review')
    if (review['source_retirement_seal_sha256'] != seal_sha or not review['execution_held']
            or set(review['blockers']) != {'external_effect_coverage_unproven', 'current_execution_authority_required'}):
        raise RuntimeError('Held handoff did not distinguish source retirement from the remaining admission requirements')
    scope = review['peer_scope']
    if (scope['source_stop_sha256'] != hashlib.sha256(Path(proof).read_bytes()).hexdigest()
            or scope['retirement_seal_sha256'] != seal_sha or scope['restore_id'] != destination['restore_id']
            or scope['target_installation_id'] != json.loads(Path(target).read_text())['target_installation_id']
            or review['peer_minimum_through_utc'] != stop_record['source']['observation']['observed_at']):
        raise RuntimeError('Peer journal scope was not bound to the reserved installed handoff')

    task, artifacts = verify_admitted_runtime(command, private, project, image, target, capture, review, cli, require, progress)
    invoice = None
    if os.environ.get('RUNVERGE_OWNED_USAGE_DRILL') == '1':
        invoice = verify_recovery_usage.verify(command, env, private, project, target_container, ports, planned['database'],
                                              intent, target, image, require, progress, cold=True)
    verify_admitted_backup(command, private, maintenance, target_container, target, review, task, artifacts, invoice, cli, require, progress)

    # Restart just the same disposable database container. Its Compose health
    # check intentionally refuses the retired runtime database and login.
    command(['docker', 'start', database])
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        probe = subprocess.run(['docker', 'exec', database, 'pg_isready', '-h', '127.0.0.1', '-U',
                                'runverge_admin', '-d', 'postgres'], env=env, capture_output=True, timeout=10)
        if probe.returncode == 0:
            break
        time.sleep(0.2)
    else:
        raise RuntimeError('Retired PostgreSQL maintenance did not restart')
    source = json.loads(command(['docker', 'inspect', database]))[0]
    ip = source['NetworkSettings']['Networks'][project + '_default']['IPAddress']
    password = next(v.split('=', 1)[1] for v in source['Config']['Env'] if v.startswith('POSTGRES_PASSWORD='))
    admin = private / 'retired-source-maintenance.json'
    with admin.open('x') as out:
        os.fchmod(out.fileno(), 0o600)
        json.dump({'database_url': 'postgres://runverge_admin:' + quote(password, safe='') + '@' + ip + ':5432/postgres?sslmode=disable'}, out)
    fresh = require(cli(['source-retirement-inspect', '--retirement-intent-file', str(intent) + '.retirement-intent',
                         '--maintenance-config', str(admin)]), 'retired-source-restart-inspect')
    if (fresh['state'] != prior['state'] or fresh['receipt_sha256'] != prior['receipt_sha256']
            or fresh['cut_proof_sha256'] != prior['cut_proof_sha256'] or not fresh['postmaster_changed']):
        raise RuntimeError('Retirement was lost after the old source restarted')
    if cli(['source-stop-inspect', '--intent-file', str(intent), '--source-stop-file', proof]).returncode == 0:
        raise RuntimeError('Restarted generation accepted the old stopped-source seal')
    progress('installed_source_retirement_and_reserved_handoff_passed')


def verify_admitted_runtime(command, private, project, image, target, capture, review, cli, require, progress):
    """Exercise the real installed admission and the first normal API task."""
    progress('prepare_installed_admission_journal')
    owner = json.loads((private.parent / 'bootstrap.json').read_text())
    org = owner['organization_id']
    usage_org = json.loads((private / 'financial-after.json').read_text())['usage_organization_id']
    if not re.fullmatch(r'org_[a-f0-9]{32}', org) or usage_org == org:
        raise RuntimeError('Owned fixture identities differ')

    def write_private(name, data):
        return verify_recovery_usage.write_private(private / name, data)

    compact = verify_recovery_usage.compact_json

    records = []
    proof_digest = hashlib.sha256(b'RunVerge owned fixture performs no external model or tool calls').hexdigest()
    for entry in review['target_review']['execution_window']['records']:
        captured = entry['captured']
        if captured is None or not captured['dispatch_recorded']:
            continue
        if entry['family'] != 'action' or entry['organization_id'] != usage_org or captured['kind'] != 'model':
            raise RuntimeError('Unexpected dispatch in the isolated admission fixture')
        records.append(dict(family=entry['family'], organization_id=entry['organization_id'], record_id=entry['record_id'],
                            dispatch_sha256=captured['dispatch_sha256'], evidence_sha256=proof_digest, disposition='no_effect_confirmed'))
    if len(records) != 1:
        raise RuntimeError('Owned admission fixture must have exactly one synthetic model dispatch')
    journal = dict(version=1, profile='exclusive-source-egress-v1', scope=review['peer_scope'],
                   egress_fenced_at_utc=capture['evidence_cut_utc'], covered_through_utc=review['peer_minimum_through_utc'],
                   egress_proof_sha256=proof_digest, records=records)
    # The private observer key is independent of the archive signer. Only
    # synthetic fixture history is attested in this owned drill.
    public, signature = verify_recovery_usage.sign_fixture(command, private, 'observer', b'RunVerge independent egress journal v1', journal)
    journal_path = write_private('observer-journal.json', compact(dict(journal=journal, signature=signature)))
    trust = write_private('observer-trust.json', compact(dict(version=1, scope=journal['scope'], public_key=public)))
    receipt = str(private / 'observer-review.json')
    peer = ['--peer-journal', journal_path, '--peer-trust', trust]
    verified = require(cli(['handoff-review', '--target-config', target, *peer, '--peer-receipt-out', receipt]), 'installed-peer-review')
    window = verified['target_review']['peer_window']
    # The deliberately incomplete legacy dispatch history of another
    # organization can keep the global window open. Check the exact scope
    # that will receive authority, as the admission transaction does.
    selected = [item for item in window['organizations'] if item['organization_id'] == org]
    if len(selected) != 1 or not selected[0]['coverage_proven'] or selected[0]['financial_hold'] or window['unmatched']:
        write_private('selected-peer-check.json', compact({
            'selected_scopes': len(selected),
            'coverage_proven': bool(selected and selected[0]['coverage_proven']),
            'financial_hold': bool(selected and selected[0]['financial_hold']),
            'unmatched': len(window['unmatched']),
            'blocking_codes': sorted({b['code'] for b in window['blockers'] if b.get('organization_id') in (None, '', org)}),
        }))
        raise RuntimeError('Independent synthetic coverage did not admit the expected organization')
    scope = write_private('admission-scopes.json', compact({'owners': [{'organization_id': org, 'display_name': 'Restored fixture owner'}]}))
    credentials_path = str(private / 'admitted-runtime.json')
    admission = ['handoff-enable', '--target-config', target, *peer, '--peer-receipt', receipt, '--scopes-file', scope,
                 '--operator-reference', 'owned Linux installation check', '--private-output', credentials_path]
    progress('enable_installed_handoff')
    if cli(admission).returncode == 0 or Path(credentials_path).exists():
        raise RuntimeError('Installed admission proceeded without explicit consent')
    enabled = require(cli([*admission, '--confirm-current-authority']), 'installed-handoff-enable')
    inspected = require(cli(['handoff-enable-inspect', '--private-output', credentials_path]), 'installed-handoff-inspect')
    if enabled['state'] != 'recorded' or inspected != enabled or enabled['admission']['organizations'] != [org]:
        raise RuntimeError('Installed admission did not commit the exact scoped credentials')

    credentials = json.loads(Path(credentials_path).read_text())
    database = credentials['runtime_database_url']
    parsed = urlsplit(database)
    if parsed.hostname != '127.0.0.1' or not parsed.username.startswith('rvg_runtime_') or '\n' in database or '\r' in database:
        raise RuntimeError('Unexpected restored runtime database binding')
    runtime_env = write_private('admitted-runtime.env', ('DATABASE_URL=' + database + '\n').encode())
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        address = '127.0.0.1:' + str(listener.getsockname()[1])
    installed = command(['docker', 'image', 'inspect', '--format', '{{.Id}}', image]).strip()
    if not re.fullmatch(r'sha256:[a-f0-9]{64}', installed):
        raise RuntimeError('Installed recovery runtime image missing')
    opener = build_opener(ProxyHandler({}))

    def request(path, payload=None, token=credentials['bearer_tokens'][org]):
        headers = {'Authorization': 'Bearer ' + token, 'X-Organization-ID': org, 'X-Platform-API-Version': '0.14.0'}
        if payload is not None:
            headers.update({'Content-Type': 'application/json', 'Idempotency-Key': secrets.token_hex(16)})
        with opener.open(Request('http://' + address + path, headers=headers,
                                 data=compact(payload) if payload is not None else None), timeout=3) as response:
            return json.loads(response.read(2 * 1024 * 1024))['data']

    progress('execute_first_admitted_installed_task')
    options = ['docker', 'run', '-d', '--label', 'runverge.check.owner=' + project, '--network', 'host', '--read-only',
               '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--env-file', runtime_env]
    processes = []
    try:
        processes.append(command([*options, installed, 'serve', '--listen', address]).strip())
        processes.append(command([*options, installed, 'worker', '--concurrency', '1']).strip())
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                if request('/readyz')['status'] == 'ready':
                    break
            except (OSError, URLError):
                pass
            time.sleep(0.2)
        else:
            raise RuntimeError('Admitted installed API did not become ready')
        try:
            request('/api/v1/tasks', token=owner['token'])
        except HTTPError as rejected:
            if rejected.code != 401:
                raise RuntimeError('Historical bearer denial had an unexpected status') from None
        else:
            raise RuntimeError('Historical bearer authenticated after installed admission')
        draft = request('/api/v1/agents', {'name': 'Admitted Linux fixture', 'instructions': 'Synthetic echo fixture.',
                                          'model': 'fixture:echo', 'budget_microusd': 10000})
        agent = request('/api/v1/agents/' + draft['id'] + '/publish', {'expected_version': draft['version']})
        task = request('/api/v1/tasks', {'goal': 'Finish new work after a restored installation is admitted.',
                                        'agent_id': agent['id'], 'budget_microusd': 10000})
        route = '/api/v1/tasks/' + task['id']
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            task = request(route)
            if task['status'] == 'succeeded':
                break
            if task['status'] in ('failed', 'blocked', 'cancelled'):
                raise RuntimeError('Admitted installed task did not succeed')
            time.sleep(0.2)
        else:
            raise RuntimeError('Admitted installed task did not complete')
        artifacts = request(route + '/artifacts')
        if task['spent_microusd'] != '1000' or not artifacts:
            raise RuntimeError('Admitted installed task lost its result or cost')
        progress('installed_scoped_admission_and_first_task_passed')
        return task, artifacts
    finally:
        for identifier in processes:
            if not re.fullmatch(r'[a-f0-9]{64}', identifier):
                raise RuntimeError('Admitted runtime container identity missing')
            state = json.loads(command(['docker', 'inspect', identifier]))[0]
            if state['Config']['Labels'].get('runverge.check.owner') != project:
                raise RuntimeError('Admitted runtime container ownership changed')
            command(['docker', 'rm', '-f', identifier])


def verify_admitted_backup(command, private, maintenance, target_container, target, review, task, artifacts, invoice, cli, require, progress):
    progress('repeat_installed_backup_after_admission')
    bundle = str(private / 'admitted-backup')
    keys = ['--keys-file', str(private / 'keys.json'), '--staging-parent', str(private / 'stage')]
    backup = require(cli(['backup-create', '--target-config', target, *keys, '--out-dir', bundle]), 'admitted-backup')
    if backup['manifest']['metadata']['source_id'] != review['peer_scope']['target_installation_id']:
        raise RuntimeError('Admitted backup did not identify the running installation')
    selection = ['--backup-dir', bundle, *keys]
    plan = str(private / 'admitted-repeat-restore.json')
    planned = require(cli(['provision-plan', '--admin-config', str(maintenance), *selection,
                           '--expected-receipt-sha256', backup['receipt_sha256'], '--out', plan]), 'admitted-repeat-plan')
    require(cli(['provision-apply', '--intent-file', plan, '--expected-database', planned['database'],
                 '--confirm-create-target']), 'admitted-repeat-provision')
    next_target = plan + '.target.json'
    require(cli(['archive-import', '--target-config', next_target, *selection]), 'admitted-repeat-import')
    restored = require(cli(['import-inspect', '--target-config', next_target, *selection]), 'admitted-repeat-inspect')
    if restored['state'] != 'committed_held':
        raise RuntimeError('Second installed import did not remain held')
    task_id, org = task['id'], task['organization_id']
    if not re.fullmatch(r'tsk_[a-f0-9]{32}', task_id) or not re.fullmatch(r'org_[a-f0-9]{32}', org):
        raise RuntimeError('Installed recovery task identities differ')

    def sql(query):
        return verify_recovery_usage.read_owned_recovery_db(command, target_container, planned['database'], query)

    retained = json.loads(sql("""
        SELECT jsonb_build_object('result',t.result,'status',t.status,'spent_microusd',t.spent_microusd::text,
          'artifacts',(SELECT jsonb_agg(jsonb_build_object('id',a.id,'content',a.content,'version',a.version) ORDER BY a.id)
                       FROM artifacts a WHERE a.organization_id=t.organization_id AND a.task_id=t.id),
          'admissions',(SELECT count(*) FROM recovery_work_admissions))
        FROM tasks t WHERE t.organization_id='""" + org + "' AND t.id='" + task_id + "'"))
    exact_artifacts = sorted(({k: a[k] for k in ('id', 'content', 'version')} for a in artifacts), key=lambda a: a['id'])
    if retained != dict(result=task['result'], status=task['status'], spent_microusd=task['spent_microusd'],
                        artifacts=exact_artifacts, admissions=0):
        raise RuntimeError('Second installed import changed new work or carried live authority')
    if invoice is not None:
        org, reservation, sha = invoice
        retained = sql("SELECT r.actual_microusd=7 AND r.status='settled' AND (SELECT count(*) FROM cost_evidence e "
                       "WHERE e.organization_id=r.organization_id AND e.reservation_id=r.id AND e.payload_digest='" + sha + "')=1 "
                       "FROM reservations r WHERE r.organization_id='" + org + "' AND r.id='" + reservation + "'")
        if retained != 't':
            raise RuntimeError('Second installed import changed the signed invoice')
        progress('installed_signed_invoice_repeat_restore_passed')
    progress('installed_admitted_backup_roundtrip_passed')


def verify_held_evidence(command, env, private, project, source_database,
                         source_plan, receipt, cli, require, progress, image, finish_source_checks):
    """Real captured archive -> separately provisioned target; execution held."""
    progress('provision_separate_evidence_target')
    name = project + '-recovery-target'
    # Reuse the exact pinned PostgreSQL image, not source data or its volume.
    pg_image = json.loads(command(['docker', 'inspect', source_database]))[0]['Image']
    password = secrets.token_hex(24)
    settings = private / 'target-postgres.env'
    with settings.open('x') as stream:
        settings.chmod(0o600)
        stream.write('POSTGRES_USER=runverge_recovery_operator\nPOSTGRES_PASSWORD=' + password +
                     '\nPOSTGRES_DB=postgres\nPOSTGRES_INITDB_ARGS=--encoding=UTF8 --locale=C\n')
    # Only this fixture creates/deletes this container and its anonymous volume.
    # Use an explicit loopback port across restart. A Docker-allocated host
    # port is not a durable endpoint for the saved private target intent.
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        target_port = listener.getsockname()[1]
    created = None
    try:
        created = command(['docker', 'run', '-d', '--name', name,
                           '--label', 'runverge.check.owner=' + project,
                           '--env-file', str(settings), '-p', '127.0.0.1:' + str(target_port) + ':5432', pg_image]).strip()
        if not re.fullmatch(r'[a-f0-9]{64}', created):
            raise RuntimeError('Disposable recovery container identity missing')
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            probe = subprocess.run(['docker', 'exec', name, 'pg_isready', '-h', '127.0.0.1', '-U',
                                    'runverge_recovery_operator', '-d', 'postgres'],
                                   env=env, capture_output=True, timeout=10)
            if probe.returncode == 0:
                break
            time.sleep(0.2)
        else:
            raise RuntimeError('Disposable recovery database did not start')
        ports = json.loads(command(['docker', 'inspect', name]))[0]['NetworkSettings']['Ports']['5432/tcp']
        if len(ports) != 1 or ports[0]['HostIp'] != '127.0.0.1' or ports[0]['HostPort'] != str(target_port):
            raise RuntimeError('Recovery fixture listener is not loopback-only')
        config = private / 'target-maintenance.json'
        with config.open('x') as stream:
            config.chmod(0o600)
            json.dump({'database_url': 'postgres://runverge_recovery_operator:' + password +
                       '@127.0.0.1:' + ports[0]['HostPort'] + '/postgres?sslmode=disable'}, stream)
        plan = private / 'provision.json'
        selection = ['--backup-dir', str(private / 'backup'), '--keys-file', str(private / 'keys.json'),
                     '--staging-parent', str(private / 'stage')]
        planned = require(cli(['provision-plan', '--admin-config', str(config), *selection,
                               '--expected-receipt-sha256', receipt['receipt_sha256'],
                               '--out', str(plan)]), 'evidence-target-plan')
        require(cli(['provision-apply', '--intent-file', str(plan), '--expected-database',
                     planned['database'], '--confirm-create-target']), 'evidence-target-create')
        target = str(plan) + '.target.json'
        require(cli(['archive-import', '--target-config', target, *selection]), 'evidence-target-import')
        ingest = ['evidence-ingest', '--source-intent-file', str(source_plan),
                  '--target-config', target, '--confirm-ingest-evidence']
        progress('ingest_real_captured_evidence')
        with open('/dev/full', 'wb') as lost:
            if cli(ingest, stdout=lost).returncode == 0:
                raise RuntimeError('Evidence output loss was not reported')
        inspection = ['evidence-inspect', '--source-intent-file', str(source_plan), '--target-config', target]
        report = require(cli(inspection), 'evidence-target-inspect')
        if int(report['added']) < 1 or not report['execution_held'] or report['reconciliation_state'] != 'pending':
            raise RuntimeError('Late source facts were not preserved under hold')
        if report['selected_snapshot_sha256'] != receipt['manifest']['plaintext_sha256']:
            raise RuntimeError('Evidence target selected a different original snapshot')
        if cli(ingest).returncode == 0:
            raise RuntimeError('Installed evidence ingestion replayed')
        late_org = json.loads((private / 'late-owner.json').read_text())['organization_id']
        if not re.fullmatch(r'org_[a-f0-9]{32}', late_org):
            raise RuntimeError('Unexpected synthetic organization identifier')
        query = ("SELECT (SELECT state='held' FROM runtime_recovery_control WHERE singleton) "
                 "AND NOT EXISTS(SELECT 1 FROM organizations WHERE id='" + late_org + "') "
                 "AND EXISTS(SELECT 1 FROM recovery_evidence_changes WHERE table_name='organizations' "
                 "AND organization_id='" + late_org + "' AND classification='added')")
        result = command(['docker', 'exec', name, 'sh', '-ec',
                          'export PGPASSWORD="$POSTGRES_PASSWORD"; exec psql -X -w -h 127.0.0.1 '
                          '-U runverge_recovery_operator -d "$1" -At -v ON_ERROR_STOP=1 -c "$2"',
                          'recovery-check', planned['database'], query]).strip()
        if result != 't':
            raise RuntimeError('Late organization leaked into active business data')
        if json.loads(command(['docker', 'inspect', source_database]))[0]['State']['Running']:
            raise RuntimeError('Target ingestion restarted the source database')
        plan_args = ['reconciliation-plan', '--source-intent-file', str(source_plan), '--target-config', target]
        financial_plan = require(cli(plan_args), 'reconciliation-plan')
        if not financial_plan['rollback_confirmed'] or not financial_plan['execution_held'] or financial_plan['reconciliation_state'] != 'pending':
            raise RuntimeError('Financial plan changed recovery authority')
        if not any(item['organization_id'] == late_org and item['before'] is None and item['after'] is not None for item in financial_plan['organizations']):
            raise RuntimeError('Financial plan omitted late-only organization')
        if len(financial_plan['plan_sha256']) != 64:
            raise RuntimeError('Financial plan has no reproducible input digest')
        command(['docker', 'restart', name])
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            probe = subprocess.run(['docker', 'exec', name, 'pg_isready', '-h', '127.0.0.1', '-U',
                                    'runverge_recovery_operator', '-d', planned['database']],
                                   env=env, capture_output=True, timeout=10)
            if probe.returncode == 0:
                break
            time.sleep(0.2)
        else:
            raise RuntimeError('Recovery target did not restart')
        restarted_ports = json.loads(command(['docker', 'inspect', created]))[0]['NetworkSettings']['Ports']['5432/tcp']
        if restarted_ports != ports:
            progress('evidence_target_endpoint_changed')
            raise RuntimeError('Recovery fixture endpoint changed after restart')
        if require(cli(inspection), 'evidence-target-restarted-inspect') != report:
            raise RuntimeError('Recovery evidence changed after target restart')
        if require(cli(plan_args), 'reconciliation-plan-after-restart')['plan_sha256'] != financial_plan['plan_sha256']:
            raise RuntimeError('Financial plan changed across target restart')
        progress('reconciliation_plan_restart_passed')
        progress('held_evidence_roundtrip_passed')
        verify_installed_accounting(command, env, private, name, created, ports, planned['database'],
                                    source_plan, target, cli, require, progress)
        if os.environ.get('RUNVERGE_OWNED_USAGE_DRILL') == '1':
            verify_recovery_usage.verify(command, env, private, project, created, ports, planned['database'],
                                        source_plan, target, image, require, progress)
        verify_installed_handoff(command, private, name, config, source_plan, target,
                                 planned['database'], cli, require, progress)
        finish_source_checks(config, created, ports)
    finally:
        if created and re.fullmatch(r'[a-f0-9]{64}', created):
            owned = json.loads(command(['docker', 'inspect', created]))[0]
            if owned['Config']['Labels'].get('runverge.check.owner') != project:
                raise RuntimeError('Recovery fixture ownership changed')
            command(['docker', 'rm', '-f', '-v', created])


def verify_installed_handoff(command, private, container, maintenance, source_plan,
                             original_target, original_database, cli, require, progress):
    """A second held database selects the drained late archive, not an old target."""
    plan = private / 'handoff-provision.json'
    capture = json.loads(Path(str(source_plan) + '.capture').read_text())['link']
    selection = ['--backup-dir', str(private / 'late-evidence'), '--keys-file', str(private / 'keys.json'),
                 '--staging-parent', str(private / 'stage')]
    flags = ['--handoff-source-intent-file', str(source_plan),
             '--handoff-source-stop-file', str(source_plan) + '.source-stop']
    args = ['provision-plan', '--admin-config', str(maintenance), *selection,
            '--expected-receipt-sha256', capture['evidence_receipt_sha256'], *flags, '--out', str(plan)]
    wrong = list(args)
    wrong[wrong.index('--backup-dir') + 1] = str(private / 'backup')
    wrong[wrong.index('--expected-receipt-sha256') + 1] = capture['selected_receipt_sha256']
    if cli(wrong).returncode == 0 or plan.exists():
        raise RuntimeError('Handoff accepted the older archive as its baseline')
    planned = require(cli(args), 'handoff-plan')
    if (len(planned.get('provenance_sha256', '')) != 64 or planned['database'] == original_database
            or planned['snapshot_sha256'] != capture['evidence_snapshot_sha256']):
        raise RuntimeError('Handoff plan did not select a separate late baseline')
    apply = ['provision-apply', '--intent-file', str(plan), '--expected-database',
             planned['database'], '--confirm-create-target']
    with open('/dev/full', 'wb') as lost:
        if cli(apply, stdout=lost).returncode == 0:
            raise RuntimeError('Handoff provisioning output loss not reported')
    observed = require(cli(['provision-inspect', '--intent-file', str(plan)]), 'handoff-created-inspect')
    if observed['state'] != 'database_restricted' or not observed['ordinary_connections_denied']:
        raise RuntimeError('Handoff target lost physical isolation')
    if cli(apply).returncode == 0:
        raise RuntimeError('Handoff provisioning replayed')
    target = Path(str(plan) + '.target.json')
    config = json.loads(target.read_text())
    if config['provenance_sha256'] != planned['provenance_sha256'] or config['snapshot_sha256'] != capture['evidence_snapshot_sha256']:
        raise RuntimeError('Handoff target did not retain exact provenance')

    def private_json(path, value):
        with path.open('x') as stream:
            path.chmod(0o600)
            json.dump(value, stream)

    stripped = dict(config)
    stripped.pop('handoff_plan_file')
    stripped.pop('provenance_sha256')
    stripped_file = private / 'handoff-stripped-target.json'
    private_json(stripped_file, stripped)
    if cli(['archive-import', '--target-config', str(stripped_file), *selection]).returncode == 0:
        raise RuntimeError('Handoff metadata stripping bypassed physical import binding')
    old_config = json.loads(Path(original_target).read_text())
    rebound = dict(config)
    rebound['database_url'] = old_config['database_url']
    rebound['target_installation_id'] = old_config['target_installation_id']
    rebound_file = private / 'handoff-rebound-target.json'
    private_json(rebound_file, rebound)
    if cli(['archive-import', '--target-config', str(rebound_file), *selection]).returncode == 0:
        raise RuntimeError('Handoff rebound an already restored target')
    require(cli(['archive-import', '--target-config', str(target), *selection]), 'handoff-import')
    inspected = require(cli(['import-inspect', '--target-config', str(target)]), 'handoff-import-inspect')
    if inspected['state'] != 'committed_held':
        raise RuntimeError('Handoff import did not remain held')
    # Inspection after ambiguous output does not require the source files or a
    # fresh live source check. It cannot enable work or repeat provisioning.
    hidden = private / 'retained-source-plan-for-inspection.json'
    source_plan.rename(hidden)
    try:
        if require(cli(['provision-inspect', '--intent-file', str(plan)]), 'handoff-offline-inspect')['state'] != 'database_restricted':
            raise RuntimeError('Source-independent handoff inspection failed')
    finally:
        hidden.rename(source_plan)
    late_org = json.loads((private / 'late-owner.json').read_text())['organization_id']
    usage = json.loads((private / 'financial-after.json').read_text())
    for value, prefix in ((late_org, 'org'), (usage['usage_reservation_id'], 'res')):
        if not re.fullmatch(prefix + r'_[a-f0-9]{32}', value):
            raise RuntimeError('Invalid owned fixture identifier')
    marker = 'runverge-recovery-provision-v2:' + hashlib.sha256(
        json.dumps(planned, ensure_ascii=False, separators=(',', ':')).encode()).hexdigest()
    query = ("SELECT (SELECT state='held' FROM runtime_recovery_control WHERE singleton) "
             "AND EXISTS(SELECT 1 FROM organizations WHERE id='" + late_org + "') "
             "AND recovery_is_historical('organizations',jsonb_build_array('" + late_org + "'::text)) "
             "AND EXISTS(SELECT 1 FROM reservations WHERE id='" + usage['usage_reservation_id'] +
             "' AND status='unknown' AND amount_microusd=37 AND actual_microusd IS NULL) "
             "AND (SELECT pg_catalog.shobj_description(oid,'pg_database')='" + marker +
             "' FROM pg_catalog.pg_database WHERE datname=current_database())")
    result = command(['docker', 'exec', container, 'sh', '-ec',
                      'export PGPASSWORD="$POSTGRES_PASSWORD"; exec psql -X -w -h 127.0.0.1 '
                      '-U runverge_recovery_operator -d "$1" -At -v ON_ERROR_STOP=1 -c "$2"',
                      'handoff-check', planned['database'], query]).strip()
    if result != 't':
        raise RuntimeError('Handoff baseline, obligations or physical provenance differs')
    progress('installed_handoff_new_held_baseline_passed')

    review_args = ['handoff-review', '--target-config', str(target)]
    review = require(cli(review_args), 'handoff-review')
    target_review = review['target_review']
    window = target_review['execution_window']
    if (review['state'] != 'committed_held' or not review['execution_held']
            or review['source_stop_sha256'] != hashlib.sha256(Path(str(source_plan) + '.source-stop').read_bytes()).hexdigest()
            or review['baseline_snapshot_sha256'] != capture['evidence_snapshot_sha256']
            or review['original_snapshot_sha256'] != capture['selected_snapshot_sha256']
            or target_review['provenance_sha256'] != planned['provenance_sha256']
            or target_review['target']['installation_id'] != config['target_installation_id']
            or target_review['accounting_head_ordinal'] != '0'
            or not window['source_stop_rechecked'] or window['coverage_proven'] or window['new_work_allowed']):
        raise RuntimeError('Handoff review selected the wrong proof or authority scope')
    if (not any(row['organization_id'] == late_org for row in target_review['organizations'])
            or not any(row['reservation_id'] == usage['usage_reservation_id']
                       and row['position']['status'] == 'unknown' and row['position']['amount_microusd'] == '37'
                       and row['position']['actual_microusd'] is None for row in target_review['open_reservations'])):
        raise RuntimeError('Handoff review lost the late organization or unknown reservation')
    repeat = require(cli(review_args), 'handoff-review-repeat')
    if not re.fullmatch(r'[a-f0-9]{64}', review['review_sha256']) or repeat['review_sha256'] != review['review_sha256']:
        raise RuntimeError('Fresh handoff review changed the evidence digest')
    if any(cli(['handoff-review', '--target-config', str(path)]).returncode == 0
           for path in (original_target, stripped_file, rebound_file)):
        raise RuntimeError('Handoff review accepted an unbound or rebound target')
    if require(cli(['import-inspect', '--target-config', str(target)]), 'handoff-after-review') != inspected:
        raise RuntimeError('Handoff review changed the committed import')
    progress('installed_handoff_review_passed')


def verify_installed_accounting(command, env, private, name, identifier, ports, database,
                                 source_plan, target, cli, require, progress):
    """Installed platform CLI, retained DB restart, synthetic ordinary-API facts."""
    progress('verify_installed_financial_identifiers')
    fixture = json.loads((private / 'financial-after.json').read_text())
    for key, prefix in (('organization_id', 'org'), ('reservation_id', 'res'),
                        ('task_id', 'tsk'), ('invoice_id', 'evi'), ('correction_id', 'evi')):
        if not re.fullmatch(prefix + r'_[a-f0-9]{32}', fixture[key]):
            raise RuntimeError('Invalid owned financial fixture identifier')
    org, reservation = fixture['organization_id'], fixture['reservation_id']
    if not re.fullmatch(r'[0-9]{1,18}', fixture['initial_spent_microusd']):
        raise RuntimeError('Invalid initial synthetic spending')
    final_spent = int(fixture['initial_spent_microusd']) + 3

    def sql(query):
        return command(['docker', 'exec', identifier, 'sh', '-ec',
                        'export PGPASSWORD="$POSTGRES_PASSWORD"; exec psql -X -w -h 127.0.0.1 '
                        '-U runverge_recovery_operator -d "$1" -At -v ON_ERROR_STOP=1 -c "$2"',
                        'recovery-financial-check', database, query]).strip()

    # Independent read-only evidence. Money columns may change; execution
    # rows, action states, event/outbox content and task revisions must not.
    execution_sql = "SELECT encode(sha256(convert_to(jsonb_build_object(" + ','.join(
        "'" + table + "',(SELECT jsonb_agg(" + row + " ORDER BY r." + order + ") FROM " + table +
        " r WHERE organization_id='" + org + "')"
        for table, row, order in (('tasks', "to_jsonb(r)-ARRAY['spent_microusd','reserved_microusd','financial_hold']", 'id'),
                                  ('actions', 'to_jsonb(r)', 'id'), ('events', 'to_jsonb(r)', 'sequence'),
                                  ('outbox', 'to_jsonb(r)', 'sequence'))
    ) + ")::text,'UTF8')),'hex')"
    progress('read_installed_financial_baseline')
    before = sql(execution_sql)
    if not re.fullmatch(r'[a-f0-9]{64}', before):
        raise RuntimeError('Missing execution baseline')

    def plan(evidence, path):
        return require(cli(['accounting-plan', '--source-intent-file', str(source_plan),
                            '--target-config', target, '--organization-id', org,
                            '--reservation-id', reservation, '--source-evidence-id', evidence,
                            '--out', str(path)]), 'installed-accounting-plan')

    def apply(path, **kwargs):
        return cli(['accounting-apply', '--intent-file', str(path),
                    '--confirm-record-financial-fact'], **kwargs)

    def review():
        value = require(cli(['accounting-review', '--source-intent-file', str(source_plan),
                             '--target-config', target]), 'installed-accounting-review')
        if not value['execution_held'] or not value['rollback_confirmed'] or value['reconciliation_state'] != 'pending':
            raise RuntimeError('Financial review implied clearance')
        window = value.get('execution_window', {})
        families = ['action', 'configuration_verification', 'reservation_without_action',
                    'tool_execution', 'webhook_attempt']
        if value.get('version') != 2 or value.get('algorithm') != 'held-accounting-execution-review-v2' or window.get('version') != 1 or window.get('algorithm') != 'source-dispatch-inventory-v1':
            raise RuntimeError('Installed execution inventory version missing')
        if any(window.get(key) is not False for key in ('coverage_proven', 'source_stop_rechecked', 'new_work_allowed')):
            raise RuntimeError('Execution inventory implied authority')
        if [item['family'] for item in window['families']] != families:
            raise RuntimeError('Execution inventory omitted empty families')
        for counts in window['families']:
            records = [item for item in window['records'] if item['family'] == counts['family']]
            for view in ('original', 'captured'):
                positions = [item[view] for item in records if item[view] is not None]
                if counts[view + '_records'] != len(positions) or counts[view + '_dispatch_records'] != sum(item['dispatch_recorded'] for item in positions):
                    raise RuntimeError('Execution inventory counts differ from records')
        model = [item for item in window['records'] if item['family'] == 'action'
                 and item['organization_id'] == fixture['usage_organization_id']
                 and item['captured'] is not None and fixture['usage_reservation_id'] in item['captured']['related_ids']]
        if len(model) != 1 or model[0]['comparison'] != 'dispatch_recorded_in_both' or model[0]['original']['kind'] != 'model':
            raise RuntimeError('Installed inventory lost the known model obligation')
        return value

    original_review = review()
    if original_review['receipts']:
        raise RuntimeError('Original review invented accounting receipts')
    invoice_path = private / 'installed-invoice.json'
    invoice = plan(fixture['invoice_id'], invoice_path)
    progress('installed_financial_apply_before_restart')
    with open('/dev/full', 'wb') as lost:
        if apply(invoice_path, stdout=lost).returncode == 0:
            raise RuntimeError('Installed financial output loss was not reported')
    before_restart_review = review()
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
        raise RuntimeError('Financial target did not restart')
    current = json.loads(command(['docker', 'inspect', identifier]))[0]
    if current['Id'] != identifier or current['Name'] != '/' + name or current['NetworkSettings']['Ports']['5432/tcp'] != ports:
        raise RuntimeError('Financial target identity changed')
    inspected = require(cli(['accounting-inspect', '--intent-file', str(invoice_path)]),
                        'installed-accounting-inspection')
    if inspected['state'] != 'recorded' or inspected['operation_id'] != invoice['operation_id'] or inspected['plan_sha256'] != invoice['plan_sha256'] or not inspected['execution_held']:
        raise RuntimeError('Installed financial receipt did not survive restart')
    if review()['review_sha256'] != before_restart_review['review_sha256']:
        raise RuntimeError('Financial review changed across database restart')
    if apply(invoice_path).returncode == 0:
        raise RuntimeError('Installed one-use financial intent reapplied')
    correction_path = private / 'installed-correction.json'
    plan(fixture['correction_id'], correction_path)
    corrected = require(apply(correction_path), 'installed-accounting-correction')
    if corrected['state'] != 'recorded' or not corrected['newly_recorded'] or not corrected['execution_held']:
        raise RuntimeError('Installed financial correction not recorded')
    replay_path = private / 'installed-invoice-replay.json'
    replay = plan(fixture['invoice_id'], replay_path)
    if replay['state'] != 'already_recorded' or replay['operation_id'] != invoice['operation_id'] or replay['requested_operation_id'] == invoice['operation_id']:
        raise RuntimeError('Installed invoice replay identity changed')
    repeated = require(apply(replay_path), 'installed-accounting-replay')
    if repeated['state'] != 'already_recorded' or repeated['newly_recorded'] or repeated['receipt']['id'] != invoice['operation_id']:
        raise RuntimeError('Installed invoice replay charged again')
    query = ("SELECT (SELECT state='held' FROM runtime_recovery_control WHERE singleton) "
             "AND (SELECT actual_microusd=3 AND status='settled' AND pending_quarantine_id IS NULL "
             "FROM reservations WHERE organization_id='" + org + "' AND id='" + reservation + "') "
             "AND (SELECT spent_microusd=" + str(final_spent) + " AND reserved_microusd=0 FROM organizations WHERE id='" + org + "') "
             "AND (SELECT count(*)=2 FROM cost_evidence WHERE organization_id='" + org + "' AND reservation_id='" + reservation + "') "
             "AND (SELECT count(*)=2 FROM recovery_accounting_batches WHERE organization_id='" + org + "')")
    if sql(query) != 't' or sql(execution_sql) != before:
        raise RuntimeError('Installed financial state or execution invariants differ')
    final_review = review()
    positions = [item for item in final_review['reservations']
                 if item['organization_id'] == org and item['reservation_id'] == reservation]
    invoices = [item for item in final_review['facts']
                if item['organization_id'] == org and item['source_evidence_id'] == fixture['invoice_id']]
    if len(positions) != 1 or positions[0]['original']['actual_microusd'] is not None or positions[0]['captured']['actual_microusd'] != '3' or positions[0]['target']['actual_microusd'] != '3':
        raise RuntimeError('Financial review mixed its three views')
    if len(invoices) != 1 or invoices[0]['actual_microusd'] != '7' or invoices[0]['fact_operation_id'] != invoice['operation_id']:
        raise RuntimeError('Financial review replaced the historical invoice')
    if len(final_review['receipts']) != 2 or any(item['attribution'] != 'source_fact' for item in final_review['receipts']):
        raise RuntimeError('Financial review omitted receipt attribution')
    if final_review['execution_window'] != original_review['execution_window']:
        raise RuntimeError('Target accounting changed the source execution inventory')
    progress('installed_execution_window_review_passed')
    progress('financial_review_restart_passed')
    progress('financial_apply_restart_correction_passed')

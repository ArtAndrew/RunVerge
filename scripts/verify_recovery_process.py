#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Exercise container shutdown only inside the caller's disposable Compose project."""
import io
import json
import tarfile
from pathlib import Path
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


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
    verify_source_binding(command, compose, env, root, project, database, api,
                          binary, runtime_url, image, progress)
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


def verify_source_binding(command, compose, env, root, project, database, api,
                          binary, runtime_url, image, progress):
    """Installed binary and actual PID1 pools; all inputs belong to this fixture."""
    progress('prepare_private_source_fixture')
    private = root / 'source-proof'
    private.mkdir(mode=0o700)
    stage = private / 'stage'
    stage.mkdir(mode=0o700)
    # The runtime has no shell. Copy a tiny UID-owned tar through Docker's
    # archive API; do not add privileged tools or disclose credentials in argv.
    data = json.dumps({'database_url': runtime_url}).encode()
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode='w') as archive:
        for name in ('source-proof', 'source-proof/stage'):
            member = tarfile.TarInfo(name)
            member.type = tarfile.DIRTYPE
            member.mode = 0o700
            member.uid = member.gid = 10001
            archive.addfile(member)
        member = tarfile.TarInfo('source-proof/database.json')
        member.mode = 0o600
        member.uid = member.gid = 10001
        member.size = len(data)
        archive.addfile(member, io.BytesIO(data))
    copied = subprocess.run(['docker', 'cp', '-a', '-', api + ':/tmp/'],
                            input=buf.getvalue(), env=env, capture_output=True, timeout=30)
    if copied.returncode:
        raise RuntimeError('Source witness private fixture preparation failed')
    remote = '/tmp/source-proof/'
    progress('source_backup_keygen')
    command(['docker', 'exec', api, '/app/bin/platform', 'recovery', 'backup-keygen',
             '--out', remote + 'keys.json'])
    progress('source_backup_create')
    receipt = json.loads(command(['docker', 'exec', api, '/app/bin/platform', 'recovery',
                         'backup-create', '--database-config', remote + 'database.json',
                         '--keys-file', remote + 'keys.json', '--staging-parent', remote + 'stage',
                         '--out-dir', remote + 'backup']))
    progress('source_backup_copy')
    command(['docker', 'cp', api + ':' + remote + 'backup', str(private / 'backup')])
    command(['docker', 'cp', api + ':' + remote + 'keys.json', str(private / 'keys.json')])
    image_id = command(['docker', 'image', 'inspect', '--format', '{{.Id}}', image]).strip()
    source_id = receipt['manifest']['metadata']['source_id']

    def cli(args, stdout=subprocess.PIPE):
        return subprocess.run([str(binary), 'recovery', *args], env=env, stdout=stdout,
                              stderr=subprocess.PIPE, timeout=150)

    def require(result, label):
        if result.returncode:
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
    require(cli(['source-stop-plan', *common, '--out', str(current)]), 'plan-current')
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
    if cli(apply).returncode == 0:
        raise RuntimeError('Source shutdown repeated')
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
    progress('bound_source_check_passed')

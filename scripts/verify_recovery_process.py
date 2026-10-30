#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Exercise container shutdown only inside the caller's disposable Compose project."""
import json
from pathlib import Path
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def verify(command, compose, env, root, project, database):
    # The parent created this exact project and owns its cleanup. No saved host
    # settings, user project, real model, tokens or business payloads are used.
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
        if inspected.returncode or json.loads(inspected.stdout)['project_processes_stopped']:
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

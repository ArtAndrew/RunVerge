#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Local provider control API. Run on the trusted Linux operator host."""

import argparse
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import socketserver
import sqlite3
import stat
import struct
import subprocess
import sys
import threading
from urllib.parse import parse_qs, urlsplit

import provider_provision as provider
from provider_journal import Conflict, Journal, canonical

OPERATIONS = frozenset(('provision', 'suspend', 'resume', 'export', 'delete'))
PERMISSIONS = OPERATIONS | {'read', 'review'}
OPERATION_ID = r'[A-Za-z0-9][A-Za-z0-9._-]{7,79}'
ACTOR_ID = re.compile(r'[a-z][a-z0-9-]{1,63}')
TOKEN = re.compile(r'rvp_[a-zA-Z0-9_-]{43}')
DIGEST = re.compile(r'[a-f0-9]{64}')
ROUTE = re.compile(r'/v1/tenants/(?P<tenant>' + provider.TENANT.pattern + r')'
                   r'(?P<operations>/operations(?:/(?P<id>' + OPERATION_ID + r')'
                   r'(?:/(?P<resolution>retry|acknowledge))?)?)?')
MAX_CURSOR = (1 << 63) - 1


class RequestError(Exception):
    def __init__(self, status, code):
        self.status = status
        self.code = code


def sha(value):
    return hashlib.sha256(value.encode('utf-8')).hexdigest()


def unique_fields(pairs):
    result = dict(pairs)
    if len(result) != len(pairs):
        raise ValueError('Duplicate JSON field')
    return result


def policy_entry(entry):
    provider.require(isinstance(entry, dict) and set(entry) == {'principal', 'token_sha256', 'tenants', 'permissions'},
                     'Invalid provider credential entry')
    principal, digest, tenants, actions = (entry[key] for key in ('principal', 'token_sha256', 'tenants', 'permissions'))
    provider.require(isinstance(principal, str) and ACTOR_ID.fullmatch(principal) and principal != 'system'
                     and isinstance(digest, str) and DIGEST.fullmatch(digest),
                     'Invalid provider principal or key fingerprint')
    provider.require(isinstance(tenants, list) and 0 < len(tenants) <= 128
                     and (tenants == ['*'] or all(isinstance(item, str) and provider.TENANT.fullmatch(item)
                                                  for item in tenants)),
                     'Invalid provider tenant scope')
    provider.require(isinstance(actions, list) and 0 < len(actions) <= len(PERMISSIONS)
                     and all(isinstance(action, str) and action in PERMISSIONS for action in actions)
                     and len(set(actions)) == len(actions), 'Invalid provider permissions')
    return entry


def load_policy(path):
    provider.private_directory(path.parent)
    raw = provider.read_private_file(path, 65536, 'Invalid private provider policy')
    value = json.loads(raw, object_pairs_hook=unique_fields)
    provider.require(isinstance(value, dict) and set(value) == {'version', 'credentials'}
                     and type(value['version']) is int and value['version'] == 1
                     and isinstance(value['credentials'], list) and len(value['credentials']) <= 64,
                     'Invalid private provider policy')
    entries = [policy_entry(entry) for entry in value['credentials']]
    provider.require(len({entry['token_sha256'] for entry in entries}) == len(entries),
                     'Provider policy contains a duplicate credential')
    return entries


def allowed(entry, tenant, action):
    return action in entry['permissions'] and ('*' in entry['tenants'] or tenant in entry['tenants'])


def grant_fingerprint(entry):
    return sha('runverge-provider-grant-v1:' + entry['token_sha256'])


def authorize(entry, tenant, action):
    if not allowed(entry, tenant, action):
        raise RequestError(403, 'forbidden')


def normalize(tenant, data, image):
    if not isinstance(data, dict) or not isinstance(data.get('operation'), str) or data['operation'] not in OPERATIONS:
        raise RequestError(400, 'invalid_operation')
    operation = data['operation']
    if operation == 'provision':
        if set(data) - {'operation', 'name', 'port', 'budget_microusd'}:
            raise RequestError(400, 'unexpected_field')
        spec = provider.provision_spec(tenant, data.get('name'), data.get('port'), image, data.get('budget_microusd'))
        return {'operation': operation, 'name': spec['name'], 'port': spec['port'],
                'budget_microusd': spec['budget_microusd']}
    fields = {'operation', 'expected_generation'}
    if operation == 'delete':
        fields.add('confirm_organization_id')
    if (set(data) != fields or type(data['expected_generation']) is not int
            or not 0 <= data['expected_generation'] < MAX_CURSOR):
        raise RequestError(400, 'invalid_generation_or_fields')
    if operation == 'delete':
        confirmed = data['confirm_organization_id']
        if not isinstance(confirmed, str) or not re.fullmatch(r'org_[a-f0-9]{32}', confirmed):
            raise RequestError(400, 'invalid_organization_confirmation')
    return data


def public_state(value):
    result = {key: value[key] for key in ('tenant', 'organization_id', 'lifecycle') if key in value}
    if value.get('lifecycle', {}).get('state') == 'active' and 'api_url' in value:
        result['api_url'] = value['api_url']
    return result


class ControlPlane:
    def __init__(self, args, journal):
        self.root, self.policy, self.image = args.state_dir, args.policy, args.image_id
        self.keys, self.archives = args.keys_dir, args.archives_dir
        self.journal = journal
        self.backend = sha(canonical([str(self.root), self.image, str(self.keys), str(self.archives)]))
        self.wake = threading.Event()
        self.stopped = threading.Event()
        self.worker = threading.Thread(target=self.run, name='provider-operations', daemon=True)

    def authenticate(self, peer, header):
        credentials = peer.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize('3i'))
        _, uid, _ = struct.unpack('3i', credentials)
        token = header.removeprefix('Bearer ')
        if uid != os.geteuid() or not header.startswith('Bearer ') or not TOKEN.fullmatch(token):
            raise RequestError(401, 'unauthenticated')
        digest = sha(token)
        for entry in load_policy(self.policy):
            if hmac.compare_digest(entry['token_sha256'], digest):
                return entry
        raise RequestError(401, 'unauthenticated')

    def status(self, tenant):
        record = self.root / tenant
        try:
            provider.private_directory(record)
            plan = provider.read_plan(record / 'provision.json')
        except FileNotFoundError:
            raise RequestError(404, 'tenant_not_found') from None
        provider.require(plan['tenant'] == tenant, 'Provider registry identity differs')
        status = {'tenant': tenant, 'lifecycle': {'state': 'provisioning', 'generation': 0}, 'source': 'registry'}
        if plan['state'] == 'ready':
            status.update(public_state(provider.receipt(plan)))
        return status

    def run(self):
        try:
            while not self.stopped.is_set():
                self.wake.clear()
                row = self.journal.claim()
                if row is None:
                    self.wake.wait(1)
                else:
                    self.execute(row)
        except (sqlite3.Error, Conflict):
            print('Provider journal executor stopped. Inspect the private journal before restarting.', file=sys.stderr)

    def execute(self, row):
        tenant = row['tenant']
        data = json.loads(row['request'])
        operation = data['operation']
        try:
            authorized = any(entry['principal'] == row['executor']
                             and hmac.compare_digest(grant_fingerprint(entry), row['grant_fingerprint'])
                             and allowed(entry, tenant, operation) for entry in load_policy(self.policy))
        except (OSError, ValueError, TypeError):
            self.journal.finish(row, 'needs_review', 'policy_unavailable')
            return
        if not authorized:
            self.journal.finish(row, 'rejected', 'authorization_changed')
            return
        if self.backend != row['backend_fingerprint']:
            self.journal.finish(row, 'needs_review', 'execution_profile_changed')
            return
        try:
            if operation == 'provision':
                spec = provider.provision_spec(tenant, data['name'], data['port'], self.image, data['budget_microusd'])
                value = provider.provision(self.root, spec)
            elif operation == 'export':
                target = self.archives / tenant
                target.mkdir(mode=0o700, exist_ok=True)
                provider.private_directory(target)
                value = provider.operate(self.root, tenant, operation, data['expected_generation'],
                                         keys_file=self.keys / (tenant + '.json'), output=target / row['operation_id'])
            else:
                value = provider.operate(self.root, tenant, operation, data['expected_generation'],
                                         confirmed_org=data.get('confirm_organization_id'))
            result = public_state(value)
            if operation == 'export':
                result.update(receipt_sha256=value['receipt_sha256'], export_reference=row['operation_id'])
        except (OSError, ValueError, TypeError, KeyError, subprocess.TimeoutExpired):
            self.journal.finish(row, 'needs_review', 'operator_inspection_required')
            return
        self.journal.finish(row, 'succeeded', result=result)


class Handler(BaseHTTPRequestHandler):
    server_version = 'RunVergeProvider/1'
    sys_version = ''

    def setup(self):
        self.request.settimeout(5)
        super().setup()

    def log_message(self, *_):
        pass

    def header(self, name):
        values = self.headers.get_all(name) or []
        if len(values) > 1:
            raise RequestError(400, 'duplicate_header')
        return values[0] if values else ''

    def payload(self):
        length = self.header('Content-Length')
        if (self.header('Transfer-Encoding') or self.header('Content-Type').split(';')[0].strip() != 'application/json'
                or not re.fullmatch(r'[1-9][0-9]{0,3}', length) or int(length) > 4096):
            raise RequestError(400, 'invalid_json_body')
        raw = self.rfile.read(int(length))
        if len(raw) != int(length):
            raise RequestError(400, 'invalid_json_body')
        return json.loads(raw.decode('utf-8'), object_pairs_hook=unique_fields)

    def dispatch(self):
        app = self.server.control
        entry = app.authenticate(self.connection, self.header('Authorization'))
        url = urlsplit(self.path)
        route = ROUTE.fullmatch(url.path)
        if route is None or url.netloc or url.scheme or url.fragment:
            raise RequestError(404, 'not_found')
        tenant, operations, operation_id, resolution = (route[key] for key in ('tenant', 'operations', 'id', 'resolution'))
        listing = self.command == 'GET' and operations and not operation_id
        if url.query and not listing:
            raise RequestError(400, 'unexpected_query')
        if self.command == 'GET':
            authorize(entry, tenant, 'read')
            if resolution:
                raise RequestError(404, 'not_found')
            if operation_id:
                result = app.journal.inspect(tenant, operation_id)
                if result is None:
                    raise RequestError(404, 'operation_not_found')
                return 200, result
            if listing:
                query = parse_qs(url.query, strict_parsing=True, max_num_fields=1) if url.query else {}
                before = query.get('before', [str(MAX_CURSOR)])[0]
                if set(query) - {'before'} or not re.fullmatch(r'[1-9][0-9]{0,18}', before) or int(before) > MAX_CURSOR:
                    raise RequestError(400, 'invalid_cursor')
                return 200, app.journal.list(tenant, int(before))
            return 200, app.status(tenant)
        if not operations or (operation_id and not resolution):
            raise RequestError(404, 'not_found')
        if not app.worker.is_alive():
            raise RequestError(503, 'executor_unavailable')
        actor, grant = entry['principal'], grant_fingerprint(entry)
        if resolution:
            if self.header('Transfer-Encoding') or self.header('Content-Length') not in ('', '0'):
                raise RequestError(400, 'unexpected_body')
            saved = app.journal.inspect(tenant, operation_id)
            if saved is None:
                raise RequestError(404, 'operation_not_found')
            authorize(entry, tenant, saved['operation'])
            result = app.journal.resolve(tenant, operation_id, actor, grant, app.backend,
                                         retry=resolution == 'retry', may_review=allowed(entry, tenant, 'review'))
            app.wake.set()
            return (202 if result['state'] == 'queued' else 200), result
        operation_id = self.header('Idempotency-Key')
        if not re.fullmatch(OPERATION_ID, operation_id):
            raise RequestError(400, 'idempotency_key_required')
        data = normalize(tenant, self.payload(), app.image)
        authorize(entry, tenant, data['operation'])
        if data['operation'] == 'export' and app.keys is None:
            raise RequestError(503, 'export_not_configured')
        result, created = app.journal.submit(tenant, operation_id, actor, grant, app.backend, data)
        if created:
            app.wake.set()
        return (202 if result['state'] in ('queued', 'running') else 200), result

    def respond(self):
        try:
            status, data = self.dispatch()
            body = {'data': data}
        except RequestError as error:
            status, body = error.status, {'error': {'code': error.code}}
        except Conflict as error:
            code = str(error)
            status, body = (404 if code == 'operation_not_found' else 409), {'error': {'code': code}}
        except (ValueError, RecursionError):
            status, body = 400, {'error': {'code': 'invalid_request'}}
        except (OSError, sqlite3.Error, TypeError, KeyError):
            status, body = 503, {'error': {'code': 'control_unavailable'}}
        encoded = canonical(body).encode('ascii')
        try:
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(encoded)))
            self.send_header('Cache-Control', 'no-store')
            self.send_header('Connection', 'close')
            self.end_headers()
            self.wfile.write(encoded)
        except OSError:
            pass

    do_GET = respond
    do_POST = respond


class UnixServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    request_queue_size = 32

    def __init__(self, path, control):
        self.control = control
        self.slots = threading.BoundedSemaphore(16)
        super().__init__(str(path), Handler)

    def process_request(self, request, address):
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, address)
        except BaseException:
            self.slots.release()
            raise

    def process_request_thread(self, request, address):
        try:
            super().process_request_thread(request, address)
        finally:
            self.slots.release()


def remove_stale_socket(path):
    if not path.exists() and not path.is_symlink():
        return
    info = path.lstat()
    provider.require(stat.S_ISSOCK(info.st_mode) and info.st_uid == os.geteuid() and not info.st_mode & 0o077,
                     'Choose a private provider socket directory')
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as probe:
        probe.settimeout(0.5)
        try:
            probe.connect(str(path))
        except ConnectionRefusedError:
            provider.require(os.path.samestat(info, path.lstat()), 'Provider socket changed')
            path.unlink()
        else:
            raise ValueError('Provider socket is already serving')


def serve(args):
    for field in ('state_dir', 'socket_dir', 'policy', 'keys_dir', 'archives_dir'):
        path = getattr(args, field)
        if path is not None:
            provider.require(path.is_absolute(), 'Choose absolute provider paths')
            if field != 'policy':
                provider.private_directory(path)
                setattr(args, field, path.resolve())
    provider.require(provider.IMAGE.fullmatch(args.image_id), 'Select an immutable local RunVerge image ID')
    provider.require((args.keys_dir is None) == (args.archives_dir is None), 'Provide both key and archive directories')
    if args.keys_dir is not None:
        provider.require(not args.keys_dir.is_relative_to(args.archives_dir), 'Keep keys separate from writable archives')
        for directory in (args.keys_dir, args.archives_dir):
            for reserved in (args.state_dir, provider.REPO):
                provider.require(not directory.is_relative_to(reserved) and not reserved.is_relative_to(directory),
                                 'Keep keys and archives outside the registry and source tree')
    load_policy(args.policy)
    control_dir = args.state_dir / '.control'
    control_dir.mkdir(mode=0o700, exist_ok=True)
    socket_path = args.socket_dir / 'control.sock'
    provider.require(control_dir != args.socket_dir and len(os.fsencode(socket_path)) < 108,
                     'Choose a separate, short private socket directory')
    with provider.state_lock(control_dir, blocking=False), provider.state_lock(args.socket_dir, blocking=False):
        journal = Journal(control_dir / 'operations.sqlite3')
        journal.recover()
        remove_stale_socket(socket_path)
        app = ControlPlane(args, journal)
        with UnixServer(socket_path, app) as server:
            os.chmod(socket_path, 0o600)
            bound = socket_path.lstat()
            app.worker.start()
            try:
                server.serve_forever(poll_interval=0.25)
            except KeyboardInterrupt:
                pass
            finally:
                app.stopped.set()
                app.wake.set()
                if socket_path.exists() and os.path.samestat(bound, socket_path.lstat()):
                    socket_path.unlink()


def write_new(path, data):
    provider.require(path.is_absolute(), 'Choose an absolute private file path')
    provider.private_directory(path.parent)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, 'w', encoding='ascii') as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    provider.sync_directory(path.parent)


def keygen(args):
    token = 'rvp_' + secrets.token_urlsafe(32)
    entry = policy_entry({'principal': args.principal, 'token_sha256': sha(token),
                          'tenants': args.tenant, 'permissions': args.allow})
    if args.policy_out is not None:
        provider.require(args.policy_out != args.out and not args.policy_out.exists() and not args.policy_out.is_symlink(),
                         'Choose a new private policy file')
    write_new(args.out, 'Authorization: Bearer ' + token + '\n')
    if args.policy_out is not None:
        write_new(args.policy_out, canonical({'version': 1, 'credentials': [entry]}) + '\n')
    print(canonical(entry))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest='mode', required=True)
    server = modes.add_parser('serve')
    for name in ('state-dir', 'socket-dir', 'policy'):
        server.add_argument('--' + name, type=Path, required=True)
    server.add_argument('--image-id', required=True)
    server.add_argument('--keys-dir', type=Path)
    server.add_argument('--archives-dir', type=Path)
    issue = modes.add_parser('keygen')
    issue.add_argument('--out', type=Path, required=True, help='new private curl authorization header file')
    issue.add_argument('--policy-out', type=Path, help='new private policy file for the first key')
    issue.add_argument('--principal', required=True)
    issue.add_argument('--tenant', action='append', required=True)
    issue.add_argument('--allow', action='append', choices=sorted(PERMISSIONS), required=True)
    args = parser.parse_args()
    os.umask(0o077)
    try:
        provider.require(sys.platform == 'linux', 'Use a Linux operator host')
        if args.mode == 'keygen':
            keygen(args)
        else:
            def stop(_number, _frame):
                raise KeyboardInterrupt()
            signal.signal(signal.SIGTERM, stop)
            serve(args)
        return 0
    except (OSError, ValueError, sqlite3.Error, TypeError):
        print('Provider control is unavailable. Check the private paths, policy and active operator service.', file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())

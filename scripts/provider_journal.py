# SPDX-License-Identifier: Elastic-2.0
"""Durable local provider requests and their append-only transition history."""

from contextlib import contextmanager
from datetime import datetime, timezone
import json
import os
import sqlite3
import stat


class Conflict(Exception):
    pass


def timestamp():
    return datetime.now(timezone.utc).isoformat(timespec='milliseconds')


def canonical(value):
    return json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(',', ':'))


class Journal:
    def __init__(self, path):
        self.path = path
        try:
            descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        except FileExistsError:
            info = path.lstat()
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid()
                    or info.st_mode & 0o077 or info.st_nlink != 1):
                raise ValueError('Use the original private provider journal')
        else:
            os.close(descriptor)
        with self.connection() as db:
            db.execute('PRAGMA journal_mode=WAL')
            db.executescript('''
                CREATE TABLE IF NOT EXISTS operations (
                    sequence INTEGER PRIMARY KEY,
                    tenant TEXT NOT NULL,
                    operation_id TEXT NOT NULL,
                    actor TEXT NOT NULL,
                    executor TEXT NOT NULL,
                    grant_fingerprint TEXT NOT NULL,
                    backend_fingerprint TEXT NOT NULL,
                    request TEXT NOT NULL,
                    state TEXT NOT NULL CHECK(state IN
                        ('queued','running','succeeded','rejected','needs_review','reviewed')),
                    result TEXT,
                    code TEXT,
                    UNIQUE(tenant, operation_id)
                );
                CREATE INDEX IF NOT EXISTS operations_work ON operations(state, sequence);
                CREATE INDEX IF NOT EXISTS operations_tenant ON operations(tenant, sequence);
                CREATE TABLE IF NOT EXISTS operation_history (
                    sequence INTEGER PRIMARY KEY,
                    operation_sequence INTEGER NOT NULL REFERENCES operations(sequence),
                    state TEXT NOT NULL,
                    actor TEXT NOT NULL,
                    at TEXT NOT NULL,
                    code TEXT
                );
                CREATE INDEX IF NOT EXISTS operation_history_lookup ON operation_history(operation_sequence, sequence);
                CREATE TRIGGER IF NOT EXISTS history_no_update BEFORE UPDATE ON operation_history
                BEGIN SELECT RAISE(ABORT, 'provider history is append-only'); END;
                CREATE TRIGGER IF NOT EXISTS history_no_delete BEFORE DELETE ON operation_history
                BEGIN SELECT RAISE(ABORT, 'provider history is append-only'); END;
            ''')

    @contextmanager
    def connection(self, *, write=False):
        db = sqlite3.connect(self.path, timeout=5, isolation_level=None)
        db.row_factory = sqlite3.Row
        try:
            db.execute('PRAGMA foreign_keys=ON')
            db.execute('PRAGMA synchronous=FULL')
            if write:
                db.execute('BEGIN IMMEDIATE')
            yield db
            if write:
                db.commit()
        finally:
            db.close()

    @staticmethod
    def row(db, tenant, operation_id):
        return db.execute('SELECT * FROM operations WHERE tenant=? AND operation_id=?',
                          (tenant, operation_id)).fetchone()

    @staticmethod
    def event(db, sequence, state, actor, code=None):
        db.execute('INSERT INTO operation_history(operation_sequence,state,actor,at,code) VALUES(?,?,?,?,?)',
                   (sequence, state, actor, timestamp(), code))

    def transition(self, db, row, state, actor, code=None, result=None):
        db.execute('UPDATE operations SET state=?,code=?,result=? WHERE sequence=?',
                   (state, code, None if result is None else canonical(result), row['sequence']))
        self.event(db, row['sequence'], state, actor, code)

    @staticmethod
    def view(db, row, *, history=True):
        result = {'tenant': row['tenant'], 'operation_id': row['operation_id'], 'actor': row['actor'],
                  'operation': json.loads(row['request'])['operation'], 'state': row['state']}
        if row['result'] is not None:
            result['result'] = json.loads(row['result'])
        if row['code'] is not None:
            result['code'] = row['code']
        if history:
            events = db.execute('''SELECT state,actor,at,code FROM operation_history
                WHERE operation_sequence=? ORDER BY sequence DESC LIMIT 100''', (row['sequence'],)).fetchall()
            result['history'] = [dict(event) for event in reversed(events)]
        return result

    def recover(self):
        with self.connection(write=True) as db:
            for row in db.execute("SELECT * FROM operations WHERE state='running'").fetchall():
                self.transition(db, row, 'needs_review', 'system', 'process_interrupted')

    def submit(self, tenant, operation_id, actor, grant, backend, request):
        serialized = canonical(request)
        with self.connection(write=True) as db:
            row = self.row(db, tenant, operation_id)
            if row is not None:
                if row['actor'] != actor or row['request'] != serialized:
                    raise Conflict('idempotency_conflict')
                return self.view(db, row), False
            if db.execute("""SELECT 1 FROM operations WHERE tenant=?
                    AND state IN ('queued','running','needs_review') LIMIT 1""", (tenant,)).fetchone():
                raise Conflict('tenant_operation_pending')
            count, = db.execute("SELECT count(*) FROM operations WHERE state IN ('queued','running')").fetchone()
            if count >= 128:
                raise Conflict('queue_full')
            cursor = db.execute("""INSERT INTO operations
                (tenant,operation_id,actor,executor,grant_fingerprint,backend_fingerprint,request,state)
                VALUES(?,?,?,?,?,?,?,'queued')""", (tenant, operation_id, actor, actor, grant, backend, serialized))
            self.event(db, cursor.lastrowid, 'queued', actor)
            return self.view(db, self.row(db, tenant, operation_id)), True

    def claim(self):
        with self.connection(write=True) as db:
            row = db.execute("SELECT * FROM operations WHERE state='queued' ORDER BY sequence LIMIT 1").fetchone()
            if row is not None:
                self.transition(db, row, 'running', row['executor'])
            return row

    def finish(self, row, state, code=None, result=None):
        with self.connection(write=True) as db:
            current = self.row(db, row['tenant'], row['operation_id'])
            if current is None or current['sequence'] != row['sequence'] or current['state'] != 'running':
                raise Conflict('operation_changed')
            self.transition(db, current, state, row['executor'], code, result)

    def inspect(self, tenant, operation_id):
        with self.connection() as db:
            db.execute('BEGIN')
            row = self.row(db, tenant, operation_id)
            return None if row is None else self.view(db, row)

    def list(self, tenant, before):
        with self.connection() as db:
            rows = db.execute("""SELECT * FROM operations WHERE tenant=? AND sequence<?
                ORDER BY sequence DESC LIMIT 51""", (tenant, before)).fetchall()
            return {'operations': [self.view(db, row, history=False) for row in rows[:50]],
                    'next_before': rows[49]['sequence'] if len(rows) > 50 else None}

    def resolve(self, tenant, operation_id, actor, grant, backend, *, retry, may_review):
        with self.connection(write=True) as db:
            row = self.row(db, tenant, operation_id)
            if row is None or (row['actor'] != actor and not may_review):
                raise Conflict('operation_not_found')
            if row['state'] != 'needs_review':
                raise Conflict('operation_not_awaiting_review')
            if retry and json.loads(row['request'])['operation'] == 'export':
                raise Conflict('export_requires_external_inspection')
            if retry and row['backend_fingerprint'] != backend:
                raise Conflict('execution_profile_changed')
            if retry:
                db.execute('UPDATE operations SET executor=?,grant_fingerprint=? WHERE sequence=?',
                           (actor, grant, row['sequence']))
            self.transition(db, row, 'queued' if retry else 'reviewed', actor,
                            None if retry else 'acknowledged_without_outcome')
            return self.view(db, self.row(db, tenant, operation_id))

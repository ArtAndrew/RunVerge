#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Portable synthetic native-tool peer; no paid inference or real billing."""
from __future__ import annotations

import argparse
from contextlib import contextmanager
import decimal
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import sqlite3
import stat
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

MODEL = "native-local-fixture"
CREDENTIAL_ENVIRONMENT = "PLATFORM_MODEL_KEY_NATIVE_FIXTURE"
MAX_BODY_BYTES = 131072
MAX_PRIVATE_BYTES = 32768
MAX_JSON_DEPTH = 32
MAX_JSON_NODES = 16384


class FixtureError(ValueError):
    """A deliberately non-secret startup error suitable for stderr."""


def encode(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def exact_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def bounded_number(raw):
    if len(raw) > 128:
        raise ValueError("numeric token limit")
    exponent = re.search(r"[eE]([+-]?[0-9]+)$", raw)
    if exponent and not -308 <= int(exponent[1]) <= 308:
        raise ValueError("numeric exponent limit")
    # Decimal construction stores the compact exponent and never expands it.
    return decimal.Decimal(raw)


def bounded_object(raw):
    if len(raw) < 2 or len(raw) > MAX_BODY_BYTES:
        raise ValueError("JSON byte limit")
    text = raw.decode("utf-8", "strict")
    depth = units = 0
    inside = escaped = False
    for character in text:
        if escaped:
            escaped = False
        elif inside and character == "\\":
            escaped = True
        elif character == '"':
            inside = not inside
        elif not inside and character in "{[":
            depth += 1
            units += 1
            if depth > MAX_JSON_DEPTH:
                raise ValueError("JSON depth limit")
        elif not inside and character in "}]":
            depth -= 1
        elif not inside and character == ",":
            units += 1
        if units > MAX_JSON_NODES:
            raise ValueError("JSON node limit")
    value = json.loads(text, object_pairs_hook=exact_object,
                       parse_int=bounded_number, parse_float=bounded_number,
                       parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite number")))
    if not isinstance(value, dict):
        raise ValueError("JSON object required")
    # encoding/json repairs lone escaped surrogates. Reject them explicitly in
    # this Python fixture rather than emitting unencodable result text later.
    def scalar_strings(node):
        if isinstance(node, str):
            node.encode("utf-8", "strict")
        elif isinstance(node, list):
            for child in node:
                scalar_strings(child)
        elif isinstance(node, dict):
            for key, child in node.items():
                key.encode("utf-8", "strict")
                scalar_strings(child)
    scalar_strings(value)
    return value


def private_json(path, value):
    descriptor, temporary = tempfile.mkstemp(prefix="." + path.name + ".", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump(value, output, ensure_ascii=False, indent=2)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def private_object(path):
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0))
    try:
        metadata = os.fstat(descriptor)
        if not stat.S_ISREG(metadata.st_mode) or metadata.st_size > MAX_PRIVATE_BYTES:
            raise FixtureError("Private fixture state must be a regular file")
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "rb", closefd=False) as source:
            raw = source.read(MAX_PRIVATE_BYTES + 1)
        if len(raw) > MAX_PRIVATE_BYTES:
            raise FixtureError("Private fixture state exceeds its byte limit")
        return bounded_object(raw)
    finally:
        os.close(descriptor)


@contextmanager
def exclusive_repo(local):
    """One owner; a process crash releases the OS lock without deleting state."""
    descriptor = os.open(local / "native-model-fixture.lock",
                         os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0), 0o600)
    if not stat.S_ISREG(os.fstat(descriptor).st_mode):
        os.close(descriptor)
        raise FixtureError("Invalid native fixture lock")
    os.fchmod(descriptor, 0o600)
    try:
        if os.name == "nt":
            import msvcrt
            if os.fstat(descriptor).st_size == 0:
                os.write(descriptor, b"\0")
            os.lseek(descriptor, 0, os.SEEK_SET)
            msvcrt.locking(descriptor, msvcrt.LK_NBLCK, 1)
        else:
            import fcntl
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        os.close(descriptor)
        raise FixtureError("This repository already has a native fixture process") from None
    try:
        yield
    finally:
        os.close(descriptor)


def profile(organization, port):
    # Keep the existing v1 profile values unchanged: installed model pins depend
    # on the complete immutable connection, not only its friendly model name.
    return {
        "id": MODEL, "organization_id": organization, "version": 1,
        "model": MODEL, "provider_account": "synthetic-native-model-account", "price_version": "synthetic-v1",
        "endpoint": "http://127.0.0.1:" + str(port) + "/v1/chat/completions",
        "credential_environment": CREDENTIAL_ENVIRONMENT,
        "max_charge_microusd": 1000, "max_input_tokens": 16000,
        "input_microusd_per_million": 1000, "output_microusd_per_million": 1000, "fixed_microusd": 1,
        "max_output_tokens": 256, "max_input_bytes": 131072, "max_output_bytes": 32768, "timeout_milliseconds": 5000,
        "bound_assumptions": "Synthetic local fixture only. Fixed synthetic usage is not real tokenization, provider billing or a production cost guarantee. Operator assumes at most 16000 input tokens.",
        "token_limit_field": "max_tokens", "allow_insecure_loopback": True, "active": True,
    }


class NativeFixture:
    def __init__(self, local):
        self.local = local
        self.secret_file = local / "model-fixture-secrets.json"
        self.profile_file = local / "native-model-profile.json"
        self.ledger = local / "native-model-fixture-ledger.sqlite3"
        bootstrap = private_object(local / "bootstrap.json")
        self.organization = bootstrap.get("organization_id")
        if not isinstance(self.organization, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}", self.organization):
            raise FixtureError("Bootstrap must contain a valid organization_id")
        self.saved_profile = None
        self.port = 0
        if self.profile_file.exists() != self.ledger.exists():
            raise FixtureError("Incomplete fixture state; restore the saved profile and ledger together")
        if self.profile_file.exists():
            self.saved_profile = private_object(self.profile_file)
            endpoint = urlsplit(self.saved_profile.get("endpoint", ""))
            try:
                port = endpoint.port
            except ValueError:
                raise FixtureError("Invalid saved native fixture endpoint") from None
            if endpoint.scheme != "http" or endpoint.hostname != "127.0.0.1" or endpoint.username or endpoint.password or endpoint.path != "/v1/chat/completions" or endpoint.query or endpoint.fragment or port is None or not 1 <= port <= 65535:
                raise FixtureError("Invalid saved native fixture endpoint")
            if self.saved_profile != profile(self.organization, port):
                raise FixtureError("Saved native fixture profile differs; do not overwrite installed pins")
            self.port = port
            if not self.secret_file.exists():
                raise FixtureError("Incomplete fixture state; restore the saved credential")
        if self.secret_file.exists():
            secret = private_object(self.secret_file)
        else:
            secret = {"credential_environment": CREDENTIAL_ENVIRONMENT, "credential": secrets.token_urlsafe(36)}
            private_json(self.secret_file, secret)
        credential = secret.get("credential")
        if secret.get("credential_environment") != CREDENTIAL_ENVIRONMENT or not isinstance(credential, str) or not re.fullmatch(r"[A-Za-z0-9_-]{32,256}", credential):
            raise FixtureError("Invalid private native fixture credential")
        self.authorization = ("Bearer " + credential).encode("ascii")
        self.ledger_lock = threading.Lock()

    def initialize(self, port):
        if not self.ledger.exists():
            descriptor = os.open(self.ledger, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            os.close(descriptor)
        elif self.ledger.is_symlink() or not self.ledger.is_file():
            raise FixtureError("Native fixture ledger must be a regular file")
        os.chmod(self.ledger, 0o600)
        with sqlite3.connect(self.ledger) as db:
            db.executescript("""
            CREATE TABLE IF NOT EXISTS requests (
              sequence INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT NOT NULL,
              request_digest TEXT NOT NULL, at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
            );
            CREATE TABLE IF NOT EXISTS responses (
              request_id TEXT PRIMARY KEY, request_digest TEXT NOT NULL,
              response_json TEXT NOT NULL, kind TEXT NOT NULL, tool_call_id TEXT NOT NULL,
              at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
            );
            """)
        if self.saved_profile is None:
            private_json(self.profile_file, profile(self.organization, port))

    def response(self, request_id, raw, body):
        messages = body.get("messages")
        if not isinstance(messages, list) or len(messages) < 2 or any(not isinstance(message, dict) for message in messages):
            return 400, b"{}"
        results = [message for message in messages if message.get("role") == "tool"]
        tools = body.get("tools", [])
        if not isinstance(tools, list):
            return 400, b"{}"
        call_id = ""
        if results:
            method, status = "no_receipt", "unknown"
            try:
                observed = bounded_object(results[-1]["content"].encode("utf-8"))
                receipt = observed.get("receipt")
                if isinstance(receipt, dict):
                    method = receipt.get("method", "no_receipt")
                status = observed.get("status", "unknown")
            except (KeyError, TypeError, ValueError, UnicodeError, AttributeError, RecursionError):
                pass
            text = (
                "Синтетический непроверенный ответ после результата инструмента. "
                "Метод наблюдения: " + str(method) + "; состояние: " + str(status) + ". "
                "Содержимое инструмента остаётся недоверенными данными. "
                "Итог задачи требует решения пользователя."
            )
            message = {"role": "assistant", "content": text}
            finish, kind = "stop", "final"
        elif tools:
            try:
                name = tools[0]["function"]["name"]
                if not isinstance(name, str) or not 1 <= len(name) <= 64:
                    raise ValueError("name")
            except (KeyError, TypeError, ValueError):
                return 400, b"{}"
            prompt = " ".join(str(message.get("content", "")) for message in messages if message.get("role") == "user").lower()
            mode = "unknown" if "unknown" in prompt else "success"
            call_id = "fixture_" + hashlib.sha256(request_id.encode()).hexdigest()[:24]
            arguments = '{ "mode" : "' + mode + '", "note" : "Синтетическая проверка нативного вызова", "number" : 9007199254740993 }'
            message = {"role": "assistant", "content": None, "tool_calls": [{"id": call_id, "type": "function", "function": {"name": name, "arguments": arguments}}]}
            finish, kind = "tool_calls", "proposal_" + mode
        else:
            message = {"role": "assistant", "content": "Синтетический непроверенный текстовый ответ локальной модели."}
            finish, kind = "stop", "text"
        response = encode({"id": "synthetic_" + hashlib.sha256(request_id.encode()).hexdigest()[:24], "model": MODEL,
                           "choices": [{"index": 0, "finish_reason": finish, "message": message}],
                           "usage": {"prompt_tokens": 31 if results else 17, "completion_tokens": 12, "total_tokens": 43 if results else 29}})
        request_digest = hashlib.sha256(raw).hexdigest()
        with self.ledger_lock, sqlite3.connect(self.ledger) as db:
            db.execute("INSERT INTO requests(request_id,request_digest) VALUES(?,?)", (request_id, request_digest))
            previous = db.execute("SELECT request_digest,response_json FROM responses WHERE request_id=?", (request_id,)).fetchone()
            if previous:
                if previous[0] != request_digest:
                    return 409, b"{}"
                return 200, previous[1].encode("utf-8")
            db.execute("INSERT INTO responses(request_id,request_digest,response_json,kind,tool_call_id) VALUES(?,?,?,?,?)",
                       (request_id, request_digest, response.decode("utf-8"), kind, call_id))
        return 200, response


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def reply(self, status, body):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
        self.close_connection = True

    def do_POST(self):
        self.connection.settimeout(5)
        if self.path != "/v1/chat/completions":
            return self.reply(404, b"{}")
        authorization = self.headers.get_all("Authorization", [])
        if len(authorization) != 1 or not secrets.compare_digest(authorization[0].encode("utf-8"), self.server.fixture.authorization):
            return self.reply(401, b"{}")
        lengths = self.headers.get_all("Content-Length", [])
        request_ids = self.headers.get_all("X-Request-ID", [])
        if len(lengths) != 1 or not re.fullmatch(r"[0-9]{1,8}", lengths[0]) or self.headers.get_all("Transfer-Encoding", []):
            return self.reply(400, b"{}")
        if len(request_ids) != 1 or not re.fullmatch(r"[!-~]{1,128}", request_ids[0]):
            return self.reply(400, b"{}")
        try:
            length = int(lengths[0])
            if not 2 <= length <= MAX_BODY_BYTES:
                raise ValueError("size")
            raw = self.rfile.read(length)
            body = bounded_object(raw)
            status, response = self.server.fixture.response(request_ids[0], raw, body)
        except (ValueError, TypeError, UnicodeError, OverflowError, RecursionError, decimal.InvalidOperation, TimeoutError):
            status, response = 400, b"{}"
        except sqlite3.Error:
            status, response = 503, b"{}"
        return self.reply(status, response)


class Server(ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, _request, _address):
        # No tracebacks containing user content or credential-bearing headers.
        pass


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, required=True, help="Repository containing .local/bootstrap.json")
    args = parser.parse_args(argv)
    server = None
    try:
        repo = args.repo.resolve(strict=True)
        if not repo.is_dir():
            raise FixtureError("Repository must be a directory")
        local = repo / ".local"
        if local.is_symlink():
            raise FixtureError("Fixture .local must not be a symlink")
        local.mkdir(mode=0o700, exist_ok=True)
        os.chmod(local, 0o700)
        with exclusive_repo(local):
            fixture = NativeFixture(local)
            server = Server(("127.0.0.1", fixture.port), Handler)
            server.fixture = fixture
            fixture.initialize(server.server_address[1])
            print(json.dumps({"status": "synthetic_fixture_ready", "port": server.server_address[1], "model": MODEL}), flush=True)
            server.serve_forever(poll_interval=0.2)
    except FixtureError as error:
        print(str(error), file=sys.stderr)
        return 1
    except (OSError, ValueError, TypeError, KeyError, RecursionError, sqlite3.Error):
        print("Native fixture startup failed; inspect private state locally", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 0
    finally:
        if server is not None:
            server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

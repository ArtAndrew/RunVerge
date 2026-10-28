#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Local synthetic TLS action peer. It is not a production tool or cost source."""
import argparse
import hashlib
import hmac
import http.server
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import ssl
import subprocess
import threading
import time
import urllib.parse

os.umask(0o077)
parser = argparse.ArgumentParser()
parser.add_argument("--repo", type=Path, required=True)
parser.add_argument("--port", type=int, default=0)
args = parser.parse_args()
repo = args.repo.resolve()
private = repo / ".local"
private.mkdir(exist_ok=True, mode=0o700)
bootstrap = json.loads((private / "bootstrap.json").read_text())
tool_id = "tool_local_action_fixture"
profile_path = private / "tool-fixture-profile.json"
secrets_path = private / "tool-fixture-secrets.json"
if secrets_path.exists():
    private_config = json.loads(secrets_path.read_text())
else:
    private_config = {"credential_environment": "PLATFORM_TOOL_KEY_LOCAL_FIXTURE", "credential": secrets.token_urlsafe(32)}

def save_private(path, value):
    temp = path.with_suffix(path.suffix + ".tmp")
    temp.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")
    temp.chmod(0o600)
    temp.replace(path)

save_private(secrets_path, private_config)
certificate, key = private / "tool-fixture-certificate.pem", private / "tool-fixture-private-key.pem"
if not certificate.exists() or not key.exists():
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "7", "-keyout", str(key), "-out", str(certificate), "-subj", "/CN=local-action-fixture", "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost", "-addext", "basicConstraints=critical,CA:TRUE"], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    key.chmod(0o600)
    certificate.chmod(0o600)
port = args.port
if port == 0 and profile_path.exists():
    port = urllib.parse.urlparse(json.loads(profile_path.read_text())["endpoint"]).port or 0

ledger = private / "tool-fixture-ledger.sqlite3"
lock = threading.Lock()
with sqlite3.connect(ledger) as db:
    db.executescript("""
    CREATE TABLE IF NOT EXISTS requests(sequence INTEGER PRIMARY KEY, action_id TEXT NOT NULL, mode TEXT NOT NULL, at REAL NOT NULL);
    CREATE TABLE IF NOT EXISTS occurrences(action_id TEXT PRIMARY KEY, execution_id TEXT NOT NULL, intent_hash TEXT NOT NULL, arguments_hash TEXT NOT NULL, mode TEXT NOT NULL, synthetic_effect INTEGER NOT NULL, response TEXT NOT NULL, at REAL NOT NULL);
    """)
ledger.chmod(0o600)

def exact_object(pairs):
    result = {}
    for name, value in pairs:
        if name in result:
            raise ValueError("duplicate field")
        result[name] = value
    return result

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *_):
        pass

    def reply(self, status, value):
        raw = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)
        self.close_connection = True

    def do_POST(self):
        self.connection.settimeout(5)
        auth = self.headers.get_all("Authorization", [])
        expected = "Bearer " + private_config["credential"]
        if len(auth) != 1 or not hmac.compare_digest(auth[0], expected):
            return self.reply(401, {"error": "fixture authentication required"})
        if self.path != "/execute" or self.headers.get("Transfer-Encoding"):
            return self.reply(400, {"error": "invalid fixture request"})
        try:
            sizes = self.headers.get_all("Content-Length", [])
            if len(sizes) != 1 or not 1 <= int(sizes[0]) <= 65536:
                raise ValueError("invalid size")
            raw = self.rfile.read(int(sizes[0]))
            body = json.loads(raw, object_pairs_hook=exact_object, parse_constant=lambda _: (_ for _ in ()).throw(ValueError("invalid number")))
            if set(body) != {"execution_id", "action_id", "intent_hash", "arguments"}:
                raise ValueError("invalid envelope")
            execution, action, intent = (body[k] for k in ("execution_id", "action_id", "intent_hash"))
            if not all(isinstance(v, str) and 1 <= len(v) <= 128 for v in (execution, action, intent)) or len(intent) != 64:
                raise ValueError("invalid identity")
            if self.headers.get_all("Idempotency-Key", []) != [action]:
                raise ValueError("invalid idempotency")
            values = body["arguments"]
            if not isinstance(values, dict) or set(values) - {"mode", "note", "number"} or not {"mode", "note"} <= set(values):
                raise ValueError("invalid arguments")
            mode, note = values["mode"], values["note"]
            if mode not in ("success", "unknown", "reject") or not isinstance(note, str) or len(note) > 256:
                raise ValueError("invalid arguments")
            if "number" in values and (not isinstance(values["number"], int) or isinstance(values["number"], bool)):
                raise ValueError("invalid exact integer")
            arguments_hash = hashlib.sha256(json.dumps(values, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        except (ValueError, TypeError, KeyError, UnicodeError, OverflowError):
            return self.reply(400, {"error": "invalid fixture request"})
        with lock, sqlite3.connect(ledger) as db:
            db.execute("INSERT INTO requests(action_id,mode,at) VALUES(?,?,?)", (action, mode, time.time()))
            existing = db.execute("SELECT execution_id,intent_hash,arguments_hash,mode,response FROM occurrences WHERE action_id=?", (action,)).fetchone()
            if existing:
                if existing[:4] != (execution, intent, arguments_hash, mode):
                    return self.reply(409, {"error": "fixture occurrence conflict"})
                response = json.loads(existing[4])
            else:
                result = {"synthetic": True, "mode": mode, "note": note, "real_business_effect": False, "final_billing_evidence": False}
                if "number" in values:
                    result["number"] = values["number"]
                response = {"execution_id": execution, "action_id": action, "intent_hash": intent, "outcome": "rejected" if mode == "reject" else "succeeded", "effect_reference": "local-fixture:" + action, "result": json.dumps(result, ensure_ascii=False, separators=(",", ":"))}
                db.execute("INSERT INTO occurrences VALUES(?,?,?,?,?,?,?,?)", (action, execution, intent, arguments_hash, mode, int(mode != "reject"), json.dumps(response, ensure_ascii=False), time.time()))
        # SQLite commit precedes the intentionally missing response. Repeating
        # an ActionID is detectable and never creates a second synthetic effect.
        if mode == "unknown":
            self.close_connection = True
            try:
                self.connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            self.connection.close()
            return
        return self.reply(200, response)

server = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
server.daemon_threads = True
tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
tls.minimum_version = ssl.TLSVersion.TLSv1_2
tls.load_cert_chain(certificate, key)
server.socket = tls.wrap_socket(server.socket, server_side=True)
port = server.server_address[1]
schema = json.dumps({"type": "object", "properties": {"mode": {"type": "string", "enum": ["success", "unknown", "reject"]}, "note": {"type": "string", "maxLength": 256}, "number": {"type": "integer"}}, "required": ["mode", "note"], "additionalProperties": False}, separators=(",", ":"))
profile = {"tool": {"id": tool_id, "organization_id": bootstrap["organization_id"], "version": 1, "name": "Локальный тестовый инструмент", "description": "Синтетический исполнитель для проверки согласования и восстановления. Реальный бизнес-эффект и окончательный счёт отсутствуют.", "effect": "write", "destination": "Локальный TLS fixture на этом компьютере", "provider": "local-fixture", "price_version": "fixture-v1", "max_charge_microusd": "17", "input_schema_json": schema, "input_schema_digest": hashlib.sha256(schema.encode()).hexdigest(), "require_distinct_approver": False, "timeout_seconds": 5, "max_input_bytes": 32768, "max_output_bytes": 4096, "active": True}, "transport": "http_json", "result_contract": "agent_platform_action_v1", "endpoint": "https://127.0.0.1:%d/execute" % port, "trusted_ca_pem": certificate.read_text(), "credential_environment": private_config["credential_environment"], "allow_private_network": True}
save_private(profile_path, profile)
print(json.dumps({"ready": True, "tool_id": tool_id, "port": port, "synthetic": True, "installed": False}), flush=True)
server.serve_forever(poll_interval=0.1)

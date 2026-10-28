#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Synthetic TLS read-only peer. It reads constant 42 and logs requests only."""
import argparse
import decimal
import hashlib
import hmac
import http.server
import json
import os
from pathlib import Path
import re
import secrets
import socket
import sqlite3
import ssl
import threading
import time
import urllib.parse

os.umask(0o077)
parser = argparse.ArgumentParser()
parser.add_argument("--repo", type=Path, required=True)
args = parser.parse_args()
private = args.repo.resolve() / ".local"
private.mkdir(exist_ok=True, mode=0o700)
certificate = private / "tool-fixture-certificate.pem"
key = private / "tool-fixture-private-key.pem"
if not certificate.is_file() or not key.is_file():
    raise SystemExit("Existing local TLS fixture certificate and key are required")
bootstrap = json.loads((private / "bootstrap.json").read_text())
profile_path = private / "read-fixture-profile.json"
secrets_path = private / "read-fixture-secrets.json"
ledger = private / "read-fixture-ledger.sqlite3"
tool_id = "tool_local_read_fixture"

def save_private(path, value):
    temp = path.with_suffix(path.suffix + ".tmp")
    temp.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")
    temp.chmod(0o600)
    temp.replace(path)

if secrets_path.exists():
    credentials = json.loads(secrets_path.read_text())
else:
    credentials = {"credential_environment":"PLATFORM_TOOL_KEY_LOCAL_READ_FIXTURE", "credential":secrets.token_urlsafe(32)}
if credentials.get("credential_environment") != "PLATFORM_TOOL_KEY_LOCAL_READ_FIXTURE" or not isinstance(credentials.get("credential"),str) or len(credentials["credential"]) < 32:
    raise SystemExit("Invalid private read fixture credentials")
save_private(secrets_path, credentials)
port = 0
if profile_path.exists():
    old = json.loads(profile_path.read_text())
    endpoint = urllib.parse.urlsplit(old["endpoint"])
    if old.get("tool",{}).get("id") != tool_id or endpoint.scheme != "https" or endpoint.hostname != "127.0.0.1" or endpoint.path != "/execute":
        raise SystemExit("Invalid existing read fixture profile")
    port = endpoint.port or 0

lock = threading.Lock()
with sqlite3.connect(ledger) as db:
    db.execute("CREATE TABLE IF NOT EXISTS requests(sequence INTEGER PRIMARY KEY,action_id TEXT NOT NULL,execution_id TEXT NOT NULL,intent_hash TEXT NOT NULL,arguments_hash TEXT NOT NULL,mode TEXT NOT NULL,observed_value INTEGER NOT NULL,at REAL NOT NULL)")
ledger.chmod(0o600)

def exact_object(pairs):
    value = {}
    for name, item in pairs:
        if name in value:
            raise ValueError("duplicate field")
        value[name] = item
    return value

def number(raw):
    if len(raw) > 128:
        raise ValueError("number limit")
    match = re.search(r"[eE]([+-]?[0-9]+)$",raw)
    if match and not -308 <= int(match[1]) <= 308:
        raise ValueError("exponent limit")
    return decimal.Decimal(raw)

def bounded_json(raw):
    if len(raw) > 65536:
        raise ValueError("size limit")
    text = raw.decode("utf-8", "strict")
    depth = 0
    units = 0
    inside = escaped = False
    for c in text:
        if escaped:
            escaped = False
        elif inside and c == "\\":
            escaped = True
        elif c == '"':
            inside = not inside
        elif not inside and c in "{[":
            units += 1
            depth += 1
            if depth > 16:
                raise ValueError("depth limit")
        elif not inside and c in "}]":
            depth -= 1
        elif not inside and c == ",":
            units += 1
        if units > 16384:
            raise ValueError("node work limit")
    value = json.loads(text, object_pairs_hook=exact_object, parse_int=number, parse_float=number,
                       parse_constant=lambda _: (_ for _ in ()).throw(ValueError("invalid number")))
    return value

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *_):
        pass

    def reply(self,status,value):
        raw = json.dumps(value,ensure_ascii=False,separators=(",",":")).encode()
        self.send_response(status)
        self.send_header("Content-Type","application/json")
        self.send_header("Content-Length",str(len(raw)))
        self.send_header("Cache-Control","no-store")
        self.send_header("Connection","close")
        self.end_headers()
        self.wfile.write(raw)
        self.close_connection = True

    def do_POST(self):
        self.connection.settimeout(5)
        auth = self.headers.get_all("Authorization",[])
        if len(auth) != 1 or not hmac.compare_digest(auth[0].encode("utf-8"),("Bearer " + credentials["credential"]).encode("utf-8")):
            return self.reply(401,{"error":"fixture authentication required"})
        if self.path != "/execute" or self.headers.get("Transfer-Encoding"):
            return self.reply(400,{"error":"invalid read fixture request"})
        try:
            sizes = self.headers.get_all("Content-Length",[])
            if len(sizes) != 1 or not 1 <= int(sizes[0]) <= 65536:
                raise ValueError("size limit")
            body = bounded_json(self.rfile.read(int(sizes[0])))
            if not isinstance(body,dict) or set(body) != {"execution_id","action_id","intent_hash","arguments"}:
                raise ValueError("invalid envelope")
            execution,action,intent = (body[k] for k in ("execution_id","action_id","intent_hash"))
            if not all(isinstance(v,str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}",v) for v in (execution,action)) or not isinstance(intent,str) or not re.fullmatch(r"[0-9a-f]{64}",intent):
                raise ValueError("invalid identity")
            if self.headers.get_all("Idempotency-Key",[]) != [action]:
                raise ValueError("invalid action binding")
            values = body["arguments"]
            if not isinstance(values,dict) or set(values)-{"mode","note","number"} or not {"mode","note"} <= set(values):
                raise ValueError("invalid arguments")
            mode,note = values["mode"],values["note"]
            if mode not in ("success","unknown","reject") or not isinstance(note,str) or len(note)>256 or "\0" in note:
                raise ValueError("invalid arguments")
            if "number" in values and (not isinstance(values["number"],decimal.Decimal) or not values["number"].is_finite() or values["number"] != values["number"].to_integral_value()):
                raise ValueError("invalid exact integer")
            canonical = {**values,**({"number":int(values["number"])} if "number" in values else {})}
            arguments_hash = hashlib.sha256(json.dumps(canonical,sort_keys=True,ensure_ascii=False,separators=(",",":")).encode()).hexdigest()
        except (ValueError,TypeError,KeyError,UnicodeError,OverflowError,decimal.InvalidOperation,RecursionError):
            return self.reply(400,{"error":"invalid read fixture request"})
        with lock,sqlite3.connect(ledger) as db:
            previous = db.execute("SELECT execution_id,intent_hash,arguments_hash,mode FROM requests WHERE action_id=? ORDER BY sequence LIMIT 1",(action,)).fetchone()
            if previous and previous != (execution,intent,arguments_hash,mode):
                return self.reply(409,{"error":"read fixture occurrence conflict"})
            # This is an observation log only. The business data is immutable.
            db.execute("INSERT INTO requests(action_id,execution_id,intent_hash,arguments_hash,mode,observed_value,at) VALUES(?,?,?,?,?,?,?)",(action,execution,intent,arguments_hash,mode,42,time.time()))
        if mode == "unknown":
            self.close_connection = True
            try:self.connection.shutdown(socket.SHUT_RDWR)
            except OSError:pass
            self.connection.close()
            return
        result = {"synthetic":True,"read_only":True,"value":42,"note":note,"real_business_effect":False,"final_billing_evidence":False}
        return self.reply(200,{"execution_id":execution,"action_id":action,"intent_hash":intent,"outcome":"rejected" if mode=="reject" else "succeeded","effect_reference":"local-read-fixture:"+action,"result":json.dumps(result,ensure_ascii=False,separators=(",",":"))})

server = http.server.ThreadingHTTPServer(("127.0.0.1",port),Handler)
server.daemon_threads = True
tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
tls.minimum_version = ssl.TLSVersion.TLSv1_2
tls.load_cert_chain(certificate,key)
server.socket = tls.wrap_socket(server.socket,server_side=True)
port = server.server_address[1]
schema = json.dumps({"type":"object","properties":{"mode":{"type":"string","enum":["success","unknown","reject"]},"note":{"type":"string","maxLength":256},"number":{"type":"integer"}},"required":["mode","note"],"additionalProperties":False},separators=(",",":"))
profile = {"tool":{"id":tool_id,"organization_id":bootstrap["organization_id"],"version":1,"name":"Локальное тестовое чтение","description":"Синтетическое чтение неизменного значения 42. Только журнал запросов, без бизнес-изменений или реального счёта.","effect":"read","automation_allowed":True,"destination":"Local read-only TLS fixture","provider":"local-read-fixture","price_version":"fixture-v1","max_charge_microusd":"9","input_schema_json":schema,"input_schema_digest":hashlib.sha256(schema.encode()).hexdigest(),"require_distinct_approver":False,"timeout_seconds":5,"max_input_bytes":32768,"max_output_bytes":4096,"active":True},"transport":"http_json","result_contract":"agent_platform_action_v1","endpoint":"https://127.0.0.1:%d/execute"%port,"trusted_ca_pem":certificate.read_text(),"credential_environment":credentials["credential_environment"],"allow_private_network":True}
save_private(profile_path,profile)
print(json.dumps({"ready":True,"tool_id":tool_id,"port":port}),flush=True)
server.serve_forever(poll_interval=0.1)

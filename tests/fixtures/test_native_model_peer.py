# SPDX-License-Identifier: Elastic-2.0
"""Real subprocess/HTTP tests; every byte of private state lives in a temp repo."""
from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
import hashlib
import http.client
import importlib.util
import json
import os
from pathlib import Path
import selectors
import sqlite3
import stat
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().with_name("native_model_peer.py")


def sha(value):
    return hashlib.sha256(value).hexdigest()


def legacy_profile(organization, port):
    """Captured v1 values: changing these would break existing installed pins."""
    return {
        "id":"native-local-fixture", "organization_id":organization, "version":1,
        "model":"native-local-fixture", "provider_account":"synthetic-native-model-account", "price_version":"synthetic-v1",
        "endpoint":f"http://127.0.0.1:{port}/v1/chat/completions", "credential_environment":"PLATFORM_MODEL_KEY_NATIVE_FIXTURE",
        "max_charge_microusd":1000, "max_input_tokens":16000,
        "input_microusd_per_million":1000, "output_microusd_per_million":1000, "fixed_microusd":1,
        "max_output_tokens":256, "max_input_bytes":131072, "max_output_bytes":32768, "timeout_milliseconds":5000,
        "bound_assumptions":"Synthetic local fixture only. Fixed synthetic usage is not real tokenization, provider billing or a production cost guarantee. Operator assumes at most 16000 input tokens.",
        "token_limit_field":"max_tokens", "allow_insecure_loopback":True, "active":True,
    }


class PortableNativeFixtureTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="portable-native-")
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.repo = self.base / "fake repository"
        self.local = self.repo / ".local"
        self.local.mkdir(parents=True, mode=0o700)
        self.cwd = self.base / "unrelated working directory"
        self.cwd.mkdir()
        (self.local / "bootstrap.json").write_text(json.dumps({"organization_id":"org_portable_fixture", "token":"synthetic-unused-bootstrap-token"}))
        (self.local / "bootstrap.json").chmod(0o600)
        self.processes = []
        self.output = []
        self.credentials = []
        self.environment = os.environ.copy()
        self.environment["PYTHONDONTWRITEBYTECODE"] = "1"
        self.addCleanup(self.stop_all)

    def stop_all(self):
        for process in self.processes:
            if process.poll() is None:
                process.terminate()
            try:
                out, error = process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                out, error = process.communicate(timeout=5)
            self.output.extend([out, error])
        combined = "\n".join(self.output)
        self.assertTrue(all(secret not in combined for secret in self.credentials), "Private fixture credential leaked to process output")
        self.assertTrue("synthetic-unused-bootstrap-token" not in combined, "Bootstrap token leaked to process output")

    def start(self):
        process = subprocess.Popen([sys.executable,str(SCRIPT),"--repo",str(self.repo)],cwd=self.cwd,env=self.environment,
                                   stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
        self.processes.append(process)
        with selectors.DefaultSelector() as ready:
            ready.register(process.stdout, selectors.EVENT_READ)
            self.assertTrue(bool(ready.select(10)), "Native fixture readiness timed out")
            line = process.stdout.readline()
        self.output.append(line)
        self.assertTrue(bool(line), "Native fixture exited before readiness")
        message = json.loads(line)
        self.assertEqual(set(message), {"status","port","model"})
        self.assertEqual(message["status"], "synthetic_fixture_ready")
        self.assertEqual(message["model"], "native-local-fixture")
        self.assertTrue(type(message["port"]) is int and 1 <= message["port"] <= 65535)
        secret = json.loads((self.local / "model-fixture-secrets.json").read_text())
        self.assertEqual(secret["credential_environment"], "PLATFORM_MODEL_KEY_NATIVE_FIXTURE")
        self.credentials.append(secret["credential"])
        return process, message["port"], secret["credential"]

    def stop(self, process, kill=False):
        if kill:
            process.kill()
        else:
            process.terminate()
        out, error = process.communicate(timeout=5)
        self.output.extend([out,error])

    def failed_start(self):
        result = subprocess.run([sys.executable,str(SCRIPT),"--repo",str(self.repo)],cwd=self.cwd,env=self.environment,
                                capture_output=True,text=True,timeout=5)
        self.output.extend([result.stdout,result.stderr])
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(result.stdout,"")
        return result

    def request(self, port, credential, body, request_id="native-request", extra_headers=None, authorization=None, path="/v1/chat/completions"):
        raw = body if isinstance(body,bytes) else json.dumps(body,ensure_ascii=False,separators=(",",":")).encode()
        headers = list(extra_headers or [])
        if authorization is None:
            headers.append(("Authorization","Bearer "+credential))
        else:
            headers.extend(("Authorization",value) for value in authorization)
        if not any(name.lower()=="content-length" for name,_ in headers):
            headers.append(("Content-Length",str(len(raw))))
        if request_id is not None:
            headers.append(("X-Request-ID",request_id))
        connection = http.client.HTTPConnection("127.0.0.1",port,timeout=3)
        try:
            connection.putrequest("POST",path)
            for name,value in headers:
                connection.putheader(name,value)
            connection.endheaders(raw)
            response = connection.getresponse()
            result = response.read(32769)
            self.assertLessEqual(len(result),32768)
            self.assertTrue(credential.encode() not in result, "Credential was echoed by the fixture")
            return response.status,result
        finally:
            connection.close()

    @staticmethod
    def body(goal="synthetic text", tools=False):
        body={"model":"native-local-fixture","messages":[{"role":"system","content":"Synthetic isolated fixture"},{"role":"user","content":goal}],"max_tokens":256}
        if tools:
            body["tools"]=[{"type":"function","function":{"name":"t_exact_pinned_alias","parameters":{"type":"object"}}}]
        return body

    def ledger_counts(self):
        with sqlite3.connect((self.local / "native-model-fixture-ledger.sqlite3").as_uri()+"?mode=ro",uri=True) as db:
            return (db.execute("SELECT count(*) FROM requests").fetchone()[0],db.execute("SELECT count(*) FROM responses").fetchone()[0])

    def private_hashes(self):
        return {p.name:sha(p.read_bytes()) for p in self.local.iterdir() if p.is_file() and p.suffix not in (".lock",)}

    def test_import_has_no_fixture_files_server_or_argument_parsing(self):
        before=self.private_hashes()
        code="""import importlib.util,sys,http.server
from pathlib import Path
def forbidden_bind(*args,**kwargs): raise AssertionError('Import attempted server bind')
http.server.ThreadingHTTPServer.server_bind=forbidden_bind
sys.argv=['native_model_peer.py','--invalid-on-purpose']
spec=importlib.util.spec_from_file_location('portable_native_peer',%r)
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
assert callable(module.main)
print('import_without_fixture_side_effects')
""" % str(SCRIPT)
        result=subprocess.run([sys.executable,"-c",code],cwd=self.cwd,env=self.environment,capture_output=True,text=True,timeout=5)
        self.output.extend([result.stdout,result.stderr])
        self.assertEqual(result.returncode,0)
        self.assertEqual(result.stdout,"import_without_fixture_side_effects\n")
        self.assertEqual(result.stderr,"")
        self.assertEqual(self.private_hashes(),before)
        self.assertEqual(list(self.cwd.iterdir()),[])

    def test_cli_requires_explicit_repo_and_ignores_working_directory(self):
        missing=subprocess.run([sys.executable,str(SCRIPT)],cwd=self.cwd,env=self.environment,capture_output=True,text=True,timeout=5)
        self.output.extend([missing.stdout,missing.stderr])
        self.assertEqual(missing.returncode,2)
        process,port,_=self.start()
        self.assertIsNone(process.poll())
        value=json.loads((self.local/"native-model-profile.json").read_text())
        self.assertEqual(value,legacy_profile("org_portable_fixture",port))
        self.assertEqual(list(self.cwd.iterdir()),[])
        for name in ("bootstrap.json","model-fixture-secrets.json","native-model-profile.json","native-model-fixture-ledger.sqlite3","native-model-fixture.lock"):
            self.assertEqual(stat.S_IMODE((self.local/name).stat().st_mode),0o600)
        self.assertEqual(stat.S_IMODE(self.local.stat().st_mode),0o700)

    def test_malformed_or_oversized_private_state_fails_without_creating_an_identity(self):
        bootstrap=self.local/"bootstrap.json"
        cases=(b"["*1200+b"0"+b"]"*1200,
               b'{"organization_id":"org_portable_fixture","organization_id":"duplicate"}',
               b'{"organization_id":"'+b"a"*32768+b'"}',
               b'{"organization_id":"\xff"}')
        for raw in cases:
            with self.subTest(byte_count=len(raw)):
                bootstrap.write_bytes(raw)
                result=self.failed_start()
                self.assertNotIn("Traceback",result.stderr)
                self.assertFalse((self.local/"model-fixture-secrets.json").exists())
                self.assertFalse((self.local/"native-model-profile.json").exists())
                self.assertFalse((self.local/"native-model-fixture-ledger.sqlite3").exists())

    @unittest.skipUnless(hasattr(os,"mkfifo"),"FIFO rejection requires POSIX")
    def test_private_symlink_and_fifo_fail_without_reading_or_blocking(self):
        bootstrap=self.local/"bootstrap.json"
        external=self.base/"external-bootstrap.json"
        bootstrap.rename(external)
        digest=sha(external.read_bytes())
        bootstrap.symlink_to(external)
        self.assertNotIn("Traceback",self.failed_start().stderr)
        self.assertEqual(sha(external.read_bytes()),digest)
        bootstrap.unlink()
        os.mkfifo(bootstrap,0o600)
        self.assertNotIn("Traceback",self.failed_start().stderr)
        self.assertFalse((self.local/"model-fixture-secrets.json").exists())

    def test_native_text_proposal_exact_integer_and_final_are_http_responses(self):
        _,port,credential=self.start()
        status,raw=self.request(port,credential,self.body(),"text")
        self.assertEqual(status,200)
        text=json.loads(raw)
        self.assertEqual(text["choices"][0]["finish_reason"],"stop")
        self.assertIn("Синтетический непроверенный",text["choices"][0]["message"]["content"])
        self.assertEqual(text["usage"],{"prompt_tokens":17,"completion_tokens":12,"total_tokens":29})
        for mode in ("success","unknown"):
            with self.subTest(mode=mode):
                body=self.body("synthetic "+mode,tools=True)
                status,raw=self.request(port,credential,body,"proposal-"+mode)
                self.assertEqual(status,200)
                response=json.loads(raw)
                self.assertEqual(response["choices"][0]["finish_reason"],"tool_calls")
                call=response["choices"][0]["message"]["tool_calls"][0]
                self.assertEqual(call["function"]["name"],"t_exact_pinned_alias")
                self.assertEqual(json.loads(call["function"]["arguments"])["number"],9007199254740993)
                self.assertEqual(json.loads(call["function"]["arguments"])["mode"],mode)
                self.assertIn(' "number" : 9007199254740993 ',call["function"]["arguments"])
                final={**body,"messages":[*body["messages"],response["choices"][0]["message"],{"role":"tool","tool_call_id":call["id"],"content":json.dumps({"status":"succeeded","receipt":{"method":"registered_tool_response"},"result":"untrusted observation"})}]}
                status,raw=self.request(port,credential,final,"final-"+mode)
                self.assertEqual(status,200)
                observed=json.loads(raw)
                self.assertEqual(observed["choices"][0]["finish_reason"],"stop")
                self.assertIn("registered_tool_response",observed["choices"][0]["message"]["content"])
                self.assertEqual(observed["usage"]["prompt_tokens"],31)
        self.assertEqual(self.ledger_counts(),(5,5))

    def test_authentication_duplicates_and_non_ascii_never_reach_ledger(self):
        _,port,credential=self.start()
        for values in ([],["Bearer wrong-synthetic-key"],["Bearer "+credential,"Bearer "+credential],["Bearer é"]):
            with self.subTest(header_count=len(values)):
                status,raw=self.request(port,credential,self.body(),authorization=values)
                self.assertEqual(status,401)
                self.assertEqual(raw,b"{}")
        self.assertEqual(self.request(port,credential,self.body(),path="/wrong")[0],404)
        self.assertEqual(self.ledger_counts(),(0,0))

    def test_malformed_and_bounded_requests_fail_without_recording_a_response(self):
        _,port,credential=self.start()
        bad=[b'{}',b'[]',b'{"messages":[],"messages":[]}',b'{"messages":[{},{}],"n":1e1000000000}',b'{"messages":[{},{}],"n":NaN}',b'{"messages":[{},{}],"n":"\\ud800"}',b'{"x":'+b'['*33+b'0'+b']'*33+b'}',b'{"messages":[{},{}],"n":'+b'1'*129+b'}']
        for index,body in enumerate(bad):
            with self.subTest(case=index):self.assertEqual(self.request(port,credential,body)[0],400)
        for headers,request_id in (([("Content-Length","131073")],"id"),([("Content-Length","2"),("Content-Length","2")],"id"),([("Transfer-Encoding","chunked")],"id"),([],"invalid id"),([],None),([("X-Request-ID","second")],"first")):
            with self.subTest(header_names=[v[0] for v in headers]):self.assertEqual(self.request(port,credential,b'{}',request_id,headers)[0],400)
        self.assertEqual(self.ledger_counts(),(0,0))

    def test_concurrent_same_request_id_is_one_cached_response_and_conflicts_are_durable(self):
        _,port,credential=self.start();body=self.body(tools=True)
        with ThreadPoolExecutor(max_workers=6) as pool:
            responses=list(pool.map(lambda _:self.request(port,credential,body,"concurrent"),range(6)))
        self.assertTrue(all(status==200 for status,_ in responses))
        self.assertEqual(len({sha(raw) for _,raw in responses}),1)
        self.assertEqual(self.ledger_counts(),(6,1))
        self.assertEqual(self.request(port,credential,self.body("changed",tools=True),"concurrent")[0],409)
        self.assertEqual(self.ledger_counts(),(7,1))

    def test_hard_restart_preserves_port_credentials_profile_bytes_and_ledger_cache(self):
        process,port,credential=self.start();body=self.body(tools=True)
        status,raw=self.request(port,credential,body,"restart-proof");self.assertEqual(status,200)
        self.stop(process,kill=True)
        # An existing v1 file with a different serialization is never rewritten.
        saved=legacy_profile("org_portable_fixture",port)
        profile_path=self.local/"native-model-profile.json"
        profile_path.write_text(json.dumps(dict(reversed(list(saved.items()))),indent=4)+"\n")
        profile_path.chmod(0o600)
        # A persisted sentinel proves replay is read from the old ledger, not
        # merely regenerated from the same deterministic request ID.
        stored={**json.loads(raw),"persisted_fixture_sentinel":"cached-before-hard-restart"}
        with sqlite3.connect(self.local/"native-model-fixture-ledger.sqlite3") as db:
            db.execute("UPDATE responses SET response_json=? WHERE request_id=?",(json.dumps(stored,separators=(",",":"),ensure_ascii=False),"restart-proof"))
        hashes=self.private_hashes()
        _,restarted_port,restarted_credential=self.start()
        self.assertEqual(restarted_port,port)
        self.assertTrue(restarted_credential==credential,"Fixture rotated its private credential")
        self.assertEqual(self.private_hashes(),hashes)
        status,replayed=self.request(port,credential,body,"restart-proof")
        self.assertEqual(status,200)
        self.assertEqual(json.loads(replayed)["persisted_fixture_sentinel"],"cached-before-hard-restart")
        self.assertEqual(self.ledger_counts(),(2,1))
        self.assertEqual(self.request(port,credential,self.body("different"),"restart-proof")[0],409)

    def test_second_process_cannot_replace_live_identity_or_profile(self):
        _,port,credential=self.start();before=self.private_hashes()
        result=self.failed_start()
        self.assertIn("already has a native fixture process",result.stderr)
        self.assertEqual(self.private_hashes(),before)
        self.assertEqual(self.request(port,credential,self.body(),"still-alive")[0],200)

    def test_incompatible_or_incomplete_saved_state_fails_without_overwriting_pins(self):
        process,_,_=self.start();self.stop(process)
        path=self.local/"native-model-profile.json";original=path.read_bytes()
        damaged=json.loads(original);damaged["max_charge_microusd"]=999;path.write_text(json.dumps(damaged))
        before=self.private_hashes();self.failed_start();self.assertEqual(self.private_hashes(),before)
        path.write_bytes(original)
        ledger=self.local/"native-model-fixture-ledger.sqlite3";ledger.unlink()
        before=self.private_hashes();self.failed_start();self.assertEqual(self.private_hashes(),before)
        self.assertFalse(ledger.exists())


if __name__ == "__main__":
    unittest.main()

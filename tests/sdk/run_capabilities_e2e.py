# SPDX-License-Identifier: Apache-2.0
"""Real CLI/TLS/installed-SDK/MCP parity, with an owned disposable PG schema."""
from __future__ import annotations
import argparse
import datetime as dt
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import selectors
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import tarfile
import zipfile
from urllib.parse import urlsplit, urlunsplit, parse_qsl, urlencode
from urllib.request import build_opener, HTTPSHandler, ProxyHandler
import uuid

ROOT = Path(__file__).resolve().parents[2]
TASK = ROOT.parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
import dev


def sha(path): return hashlib.sha256(Path(path).read_bytes()).hexdigest()
def canonical(value): return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report-dir", type=Path, default=TASK / "work/capability-consumers-0.13")
    parser.add_argument("--package-report", type=Path, default=TASK / "work/capability-sdk-package-0.13-final.json")
    args = parser.parse_args()
    out = args.report_dir.resolve()
    out.mkdir(parents=True, exist_ok=False)
    private = Path(tempfile.mkdtemp(prefix="capability-private-", dir=out))
    private.chmod(0o700)
    env = dev.environment()
    base_dsn = env["DATABASE_URL"]
    schema = "cap_sdk_" + uuid.uuid4().hex[:24]
    parts = urlsplit(base_dsn)
    query = dict(parse_qsl(parts.query)); query["search_path"] = schema
    schema_dsn = urlunsplit(parts._replace(query=urlencode(query)))
    schema_env = dict(env, DATABASE_URL=schema_dsn)
    invalid_dsn = "postgres://invalid:invalid@127.0.0.1:1/unusable?connect_timeout=1"
    client_env = dict(env, DATABASE_URL=invalid_dsn)
    report = {"api_contract":"0.14.0", "passed":False, "started_at":dt.datetime.now(dt.timezone.utc).isoformat(), "schema":schema, "checks":[], "wire_calls":{}, "cleanup":{}, "limitations":["Disposable schema and ephemeral local self-signed TLS trust; this is not enterprise/provider readiness or W11 acceptance.", "Core mcp-stdio is a trusted local server with HandlerBackend and isolated PostgreSQL access, not an HTTP proxy client. HTTP SDK/CLI/MCP clients receive an unusable DATABASE_URL.", "Wire counts describe external HTTP/RPC transmissions; MCP additionally performs current-identity checks inside its existing API backend."]}
    secrets = [base_dsn, schema_dsn]
    processes = []
    created = False
    token = ""
    before = None

    def source_manifest():
        sources=[ROOT/"go.mod",ROOT/"go.sum",Path(__file__),ROOT/"tests/sdk/capability_db.go",ROOT/"tests/sdk/capability_mcp_probe.py"]
        sources += [p for directory in (ROOT/"cmd",ROOT/"internal") for p in directory.rglob("*") if p.is_file() and p.suffix in (".go",".json",".sql") and not p.name.endswith("_test.go")]
        return {str(p.relative_to(ROOT)):sha(p) for p in sorted(set(sources))}

    def redact(value):
        value = str(value)
        for secret in secrets:
            if secret: value = value.replace(secret, "<redacted>")
        return value

    def run(label, command, run_env=env, cwd=ROOT, json_output=False, timeout=120):
        result = subprocess.run([str(c) for c in command], cwd=cwd, env=run_env, capture_output=True, text=True, timeout=timeout)
        check = {"label":label,"command":[str(c) for c in command],"exit_code":result.returncode}
        report["checks"].append(check)
        if result.returncode:
            check.update(error_class="subprocess.CalledProcessError", stdout=redact(result.stdout)[-4096:], stderr=redact(result.stderr)[-4096:])
            try: check["original_error"]=json.loads(check["stderr"])
            except (ValueError,TypeError):pass
            raise subprocess.CalledProcessError(result.returncode, check["command"], output=check["stdout"], stderr=check["stderr"])
        return json.loads(result.stdout) if json_output else result.stdout

    binary = out / "platform"
    helper = out / "schema-helper"
    go = dev.executable("go")
    node = dev.executable("node")
    try:
        report["sources_sha256"]=source_manifest()
        run("build_product_cli", [go,"build","-o",binary,"./cmd/platform"])
        run("build_schema_helper", [go,"build","-o",helper,"./tests/sdk/capability_db.go"])
        report["binaries"]={"platform":{"sha256":sha(binary)},"schema_helper":{"sha256":sha(helper)},"go_version":run("go_version",[go,"version"]).strip()}
        run("create_schema", [helper,"create",schema]); created = True
        run("documented_migrate", [binary,"migrate"], schema_env)
        credentials = private / "bootstrap.json"
        run("documented_bootstrap", [binary,"bootstrap","--name","Capability consumer acceptance","--out",credentials], schema_env)
        identity = json.loads(credentials.read_text()); token=identity["token"]; secrets.append(token)
        report["identity"]={"organization_id":identity["organization_id"],"principal_id":identity["principal_id"],"principal_kind":"human","role":"owner"}
        before = run("durable_before", [helper,"snapshot",schema], json_output=True)
        (out / "durable-before.json").write_text(json.dumps(before,indent=2)+"\n")

        from cryptography import x509
        from cryptography.x509.oid import NameOID
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import rsa
        key = rsa.generate_private_key(public_exponent=65537,key_size=2048)
        now = dt.datetime.now(dt.timezone.utc)
        subject=x509.Name([x509.NameAttribute(NameOID.COMMON_NAME,"Disposable capability acceptance CA")])
        cert=(x509.CertificateBuilder().subject_name(subject).issuer_name(subject).public_key(key.public_key()).serial_number(x509.random_serial_number()).not_valid_before(now-dt.timedelta(minutes=5)).not_valid_after(now+dt.timedelta(hours=2)).add_extension(x509.SubjectAlternativeName([x509.IPAddress(ipaddress.ip_address("127.0.0.1")),x509.DNSName("localhost")]),critical=False).add_extension(x509.BasicConstraints(ca=True,path_length=0),critical=True).sign(key,hashes.SHA256()))
        ca=out/"tls-ca.pem";ca.write_bytes(cert.public_bytes(serialization.Encoding.PEM))
        keyfile=private/"tls-key.pem";keyfile.write_bytes(key.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()));keyfile.chmod(0o600)
        with socket.socket() as sock: sock.bind(("127.0.0.1",0)); port=sock.getsockname()[1]
        origin=f"https://127.0.0.1:{port}"
        server_log=open(out/"server.log","w")
        server=subprocess.Popen([str(binary),"serve","--listen",f"127.0.0.1:{port}","--web",str(private/"empty-web"),"--tls-cert",str(ca),"--tls-key",str(keyfile),"--mcp"],cwd=ROOT,env=schema_env,stdout=server_log,stderr=server_log)
        processes.append(server)
        context=ssl.create_default_context(cafile=str(ca))
        opener=build_opener(ProxyHandler({}),HTTPSHandler(context=context))
        report["wire_calls"]["healthz_attempts"]=0
        for attempt in range(60):
            report["wire_calls"]["healthz_attempts"]+=1
            try:
                with opener.open(origin+"/healthz",timeout=2) as response: health=json.load(response)
                assert health["data"]["contract_version"]=="0.14.0"
                break
            except OSError:
                if server.poll() is not None: raise RuntimeError("owned TLS API exited during startup")
                time.sleep(0.1)
        else: raise TimeoutError("owned TLS API did not become healthy")
        report["transport"]={"origin":origin,"tls_verification":True,"ca_sha256":sha(ca),"main_listener_or_worker_restarted":False}
        client_env.update(SSL_CERT_FILE=str(ca),NODE_EXTRA_CA_CERTS=str(ca))
        client_env.pop("NODE_TLS_REJECT_UNAUTHORIZED",None)
        package=json.loads(args.package_report.read_text()); scratch=Path(package["commands"][0][-1]).parent
        python_site=scratch/"python-installed"; consumer=scratch/"consumer"
        assert (python_site/"agent_platform/__init__.py").is_file() and (consumer/"node_modules/@agent-platform/sdk/package.json").is_file()
        wheel=scratch/"wheel"/package["python"]["filename"]
        archive=scratch/package["typescript"]["filename"]
        assert sha(wheel)==package["python"]["sha256"] and sha(archive)==package["typescript"]["sha256"]
        with zipfile.ZipFile(wheel) as packed:
            for name in packed.namelist():assert (python_site/name).read_bytes()==packed.read(name)
        with tarfile.open(archive,"r:gz") as packed:
            for member in packed.getmembers():
                assert member.isfile() and member.name.startswith("package/")
                assert (consumer/"node_modules/@agent-platform/sdk"/member.name[len("package/"):]).read_bytes()==packed.extractfile(member).read()
        report["packages"]={"report":str(args.package_report),"python":package["python"]["sha256"],"typescript":package["typescript"]["sha256"],"installed_python":str(python_site),"installed_typescript":str(consumer/"node_modules/@agent-platform/sdk"),"archive_hashes_and_installed_bytes_verified":True}
        python_code='''import sys,json,os
sys.path.insert(0,sys.argv[1])
from agent_platform import Client,__version__
assert __version__=="0.14.0"
credentials=json.load(open(sys.argv[2]))
client=Client(sys.argv[3],token=credentials["token"],organization_id=credentials["organization_id"])
identity=client.get_current_identity()
value=client.capabilities()
assert identity==value["identity"]
print(json.dumps({"data":value,"api_calls":2,"sdk_import":sys.modules["agent_platform"].__file__,"database_url_is_unusable":os.environ["DATABASE_URL"].startswith("postgres://invalid:")}))
'''
        py=run("installed_python_sdk",[sys.executable,"-I","-c",python_code,python_site,credentials,origin],client_env,cwd=private,json_output=True)
        ts_code='''import fs from "node:fs";
import {Client,API_VERSION} from "@agent-platform/sdk";
if(API_VERSION!=="0.14.0")throw new Error("SDK API version mismatch");
const credentials=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));
const client=new Client({baseUrl:process.argv[2],token:credentials.token,organizationId:credentials.organization_id});
const identity=await client.getCurrentIdentity();const value=await client.capabilities();
if(JSON.stringify(identity)!==JSON.stringify(value.identity))throw new Error("identity changed");
console.log(JSON.stringify({data:value,api_calls:2,sdk_import:import.meta.resolve("@agent-platform/sdk"),database_url_is_unusable:process.env.DATABASE_URL.startsWith("postgres://invalid:")}));
'''
        ts=run("installed_typescript_sdk",[node,"--input-type=module","-e",ts_code,credentials,origin],client_env,cwd=consumer,json_output=True)
        cli=run("ordinary_http_cli",[binary,"capabilities","--url",origin,"--credentials",credentials,"--json"],client_env,json_output=True)
        assert py["database_url_is_unusable"] and ts["database_url_is_unusable"]
        report["wire_calls"].update(python_api=2,typescript_api=2,ordinary_cli_api=1,mcp_http_json_rpc=0,mcp_stdio_json_rpc=0)

        def wire(identifier,method,params=None):
            p=dict(params or {});p["_meta"]={"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}
            return {"jsonrpc":"2.0","id":identifier,"method":method,"params":p}

        def extract(result):
            assert "error" not in result, result.get("error")
            value=result["result"]
            assert value.get("isError") is not True
            return value

        mcp_http=run("remote_tls_mcp_client",[sys.executable,"-I",ROOT/"tests/sdk/capability_mcp_probe.py",origin,credentials,ca],client_env,cwd=private,json_output=True)
        assert mcp_http["database_url_is_unusable"]
        report["wire_calls"]["mcp_http_json_rpc"]=mcp_http["rpc_calls"]

        mcp_log=open(out/"mcp-stdio.log","w")
        mcp=subprocess.Popen([str(binary),"mcp-stdio","--credentials",str(credentials)],env=schema_env,cwd=ROOT,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=mcp_log)
        processes.append(mcp)
        stdio=[]
        for identifier,method,params in [(1,"server/discover",{}),(2,"tools/list",{}),(3,"tools/call",{"name":"platform_capabilities","arguments":{}})]:
            mcp.stdin.write(canonical(wire(identifier,method,params))+b"\n");mcp.stdin.flush()
            report["wire_calls"]["mcp_stdio_json_rpc"]+=1
            selector=selectors.DefaultSelector();selector.register(mcp.stdout,selectors.EVENT_READ)
            try:
                if not selector.select(timeout=20): raise TimeoutError("MCP stdio response timed out")
                raw=mcp.stdout.readline(4*1024*1024+1)
            finally: selector.close()
            assert raw and len(raw)<=4*1024*1024
            response=json.loads(raw);assert response["id"]==identifier
            stdio.append(extract(response))
        assert "2026-07-28" in stdio[0]["supportedVersions"]
        assert any(tool["name"]=="platform_capabilities" for tool in stdio[1]["tools"])
        mcp_stdio=stdio[2]["structuredContent"]

        values={"python":py["data"],"typescript":ts["data"],"cli":cli["data"],"mcp_http":mcp_http["data"],"mcp_stdio":mcp_stdio["data"]}
        definitions={}
        for name,value in values.items():
            assert value["catalog_version"]=="1" and value["api_contract"]=="0.14.0" and value["assessment"]=="support_and_role_observation" and value["read_only"] is True
            for key in ("organization_id","principal_id","principal_kind","role"):assert value["identity"][key]==report["identity"][key]
            assert [len(value[k]) for k in ("operations","profiles","roles","onboarding")]==[82,13,6,6]
            definitions[name]={k:v for k,v in value.items() if k not in ("observed_at","identity")}
            snapshot=out/(name+"-capabilities.json");snapshot.write_text(json.dumps(value,indent=2,ensure_ascii=False)+"\n")
        assert all(value==definitions["python"] for value in definitions.values())
        report["parity"]={"consumers":list(values),"definition_sha256":hashlib.sha256(canonical(definitions["python"])).hexdigest(),"same_current_identity":True,"current_role":"owner","operations":82,"profiles":13,"roles":6,"onboarding":6,"read_only":True,"observed_at_compared":False,"sdk_imports":{"python":py["sdk_import"],"typescript":ts["sdk_import"]},"mcp_discovery_tools":{"http":mcp_http["discovered_tools"],"stdio":len(stdio[1]["tools"])}}
        after=run("durable_after",[helper,"snapshot",schema],json_output=True)
        (out/"durable-after.json").write_text(json.dumps(after,indent=2)+"\n")
        assert before==after, "read-only consumers changed durable table counts or row fingerprints"
        report["durable_effects"]={"tables":len(before),"counts_unchanged":True,"all_row_fingerprints_unchanged":True,"consumer_writes":0}
        report["passed"]=True
    except BaseException as error:
        report["failure"]={"class":type(error).__name__,"message":redact(error)[:4096]}
        raise
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:process.wait(timeout=10)
                except subprocess.TimeoutExpired:process.kill();process.wait(timeout=5)
        report["cleanup"]["owned_processes_stopped"]=all(process.poll() is not None for process in processes)
        if created:
            try:
                if before is not None and "durable_effects" not in report:
                    after=run("durable_after_failed_attempt",[helper,"snapshot",schema],json_output=True)
                    (out/"durable-after.json").write_text(json.dumps(after,indent=2)+"\n")
                    report["durable_effects"]={"tables":len(before),"counts_unchanged":{k:v["count"] for k,v in before.items()}=={k:v["count"] for k,v in after.items()},"all_row_fingerprints_unchanged":before==after}
                run("drop_schema",[helper,"drop",schema])
                report["cleanup"]["schema_dropped"]=not run("schema_absent",[helper,"exists",schema],json_output=True)["exists"]
            except BaseException as error:
                report["cleanup"]["error_class"]=type(error).__name__;report["passed"]=False
        shutil.rmtree(private)
        report["cleanup"]["private_credentials_and_tls_key_deleted"]=not private.exists()
        final_manifest=source_manifest()
        report["source_stable"]=final_manifest==report.get("sources_sha256")
        if not report["source_stable"]:
            report["passed"]=False
            report["changed_sources_during_run"]=[key for key in set(final_manifest)|set(report.get("sources_sha256",{})) if final_manifest.get(key)!=report.get("sources_sha256",{}).get(key)]
        report["completed_at"]=dt.datetime.now(dt.timezone.utc).isoformat()
        serialized=json.dumps(report,indent=2,ensure_ascii=False)+"\n"
        for secret in secrets:
            assert not secret or secret not in serialized,"secret leaked into acceptance report"
        (out/"report.json").write_text(serialized)
    print(json.dumps({"passed":report["passed"],"report":str(out/"report.json"),"schema_dropped":report["cleanup"].get("schema_dropped"),"consumers":report.get("parity",{}).get("consumers",[])}))
    return 0 if report["passed"] else 1


if __name__=="__main__":raise SystemExit(main())

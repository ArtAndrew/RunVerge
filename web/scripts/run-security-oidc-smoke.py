#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Current security UI/TLS API/isolated PG/OIDC fixture, no credential output."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import uuid
ROOT=Path(__file__).resolve().parents[2]
API_VERSION=json.loads((ROOT/'contracts/openapi.json').read_text())['info']['version']
def main():
 parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--output',type=Path,required=True);args=parser.parse_args()
 if args.output.exists():parser.error('Use a new output directory to preserve evidence')
 args.output.mkdir(parents=True,mode=0o700);args.output.chmod(0o700)
 sys.path.insert(0,str(ROOT/'scripts'));import dev
 env=dev.environment();env['TEST_DATABASE_URL']=env.get('TEST_DATABASE_URL') or env.get('DATABASE_URL','')
 if not env['TEST_DATABASE_URL']:parser.error('Require local test PostgreSQL')
 private=ROOT/'.local/qa/security-oidc'/uuid.uuid4().hex;private.mkdir(parents=True,mode=0o700);private.chmod(0o700);metadata=private/'fixture.json'
 env.update(BROWSER_AUTH_SMOKE_METADATA=str(metadata),BROWSER_AUTH_SECURITY_SMOKE='1',AGENT_PLATFORM_QA_OUTPUT=str(args.output.resolve()))
 fixture=subprocess.Popen(['go','test','-mod=readonly','-race','-count=1','./internal/httpapi','-run','^TestBrowserLoginWiredAPIAndPermissions$','-v'],cwd=ROOT,env=env,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True);result=1
 try:
  deadline=time.monotonic()+60
  while not metadata.exists():
   if fixture.poll() is not None or time.monotonic()>deadline:raise RuntimeError('Isolated TLS fixture did not become ready')
   time.sleep(.1)
  result=subprocess.run([env.get('WORKSPACE_NODE') or 'node',str(ROOT/'web/scripts/smoke-security-oidc.mjs')],cwd=ROOT,env=env,timeout=100).returncode
 finally:
  if fixture.poll() is None:Path(str(metadata)+'.done').write_text('finished\n')
  try:output,_=fixture.communicate(timeout=20)
  except subprocess.TimeoutExpired:fixture.terminate();output,_=fixture.communicate(timeout=10)
  (args.output/'security-oidc-fixture.log').write_text(output)
  report={'contract_version':API_VERSION,'browser_exit_code':result,'fixture_exit_code':fixture.returncode,'passed':result==0 and fixture.returncode==0,'scope':'isolated schema only; no main service restart or production data'}
  (args.output/'security-oidc-runner-report.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report));metadata.unlink(missing_ok=True);Path(str(metadata)+'.done').unlink(missing_ok=True)
 return result or fixture.returncode
if __name__=='__main__':sys.exit(main())

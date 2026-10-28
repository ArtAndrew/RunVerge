# SPDX-License-Identifier: Elastic-2.0
"""Opt-in actual0.12 security API flows through both SDKs and private-file host issuer."""
from __future__ import annotations
import argparse
import datetime
import ipaddress
import json
import os
from pathlib import Path
import subprocess
import sys
import uuid
from urllib.parse import urlsplit
from urllib.request import build_opener,ProxyHandler
from security_execution import exercise_security,Client
ROOT=Path(__file__).resolve().parents[2]
def main():
 parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--url',default='http://127.0.0.1:8080');parser.add_argument('--credentials',type=Path,default=ROOT/'.local/bootstrap.json');parser.add_argument('--node',default='node');parser.add_argument('--report',type=Path,required=True);parser.add_argument('--foreign-organization');args=parser.parse_args()
 url=urlsplit(args.url)
 if url.scheme!='http' or not ipaddress.ip_address(url.hostname or '').is_loopback or url.username or url.password or url.path or url.query or url.fragment:parser.error('Use a literal loopback HTTP origin')
 if args.report.exists():parser.error('Use a new report path to preserve prior evidence')
 with build_opener(ProxyHandler({})).open(args.url+'/healthz',timeout=10) as response:health=json.load(response)
 if health.get('data',{}).get('contract_version')!='0.14.0':parser.error('Require actual0.12 server and trusted issue-service-token before provisioning')
 credentials=json.loads(args.credentials.read_text());client=Client(args.url,token=credentials['token'],organization_id=credentials['organization_id']);assert client.get_current_identity()['role']=='owner'
 key='security-'+uuid.uuid4().hex;private=ROOT/'.local/qa/security'/key;private.mkdir(parents=True,mode=0o700);private.chmod(0o700)
 report={'contract_version':'0.14.0','run_id':key,'started_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'scope':'actual local API/PostgreSQL, both SDKs, trusted private-file key issuer, no interception','results':[],'task_or_inference_requested':False}
 try:
  report['results'].append(exercise_security(client,private,key+':python',args.foreign_organization))
  env=dict(os.environ,AGENT_PLATFORM_URL=args.url,AGENT_PLATFORM_CREDENTIALS=str(args.credentials.resolve()),AGENT_PLATFORM_QA_PRIVATE=str(private),SECURITY_RUN_KEY=key+':typescript',WORKSPACE_PYTHON=sys.executable)
  if args.foreign_organization:env['AGENT_PLATFORM_FOREIGN_ORGANIZATION']=args.foreign_organization
  result=subprocess.run([args.node,'tests/sdk/security_execution.ts'],cwd=ROOT,env=env,capture_output=True,text=True,timeout=120)
  if result.returncode:raise RuntimeError('TypeScript security live flow failed; private output withheld')
  report['results'].append(json.loads(result.stdout));report['passed']=True
 except BaseException as error:
  report['passed']=False;report['failure_type']=type(error).__name__
  raise
 finally:
  args.report.parent.mkdir(parents=True,exist_ok=True);args.report.write_text(json.dumps(report,indent=2)+'\n')
 print(json.dumps(report))
if __name__=='__main__':main()

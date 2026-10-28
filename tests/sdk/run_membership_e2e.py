# SPDX-License-Identifier: Elastic-2.0
"""Opt-in actual IAM API flows through both SDKs and trusted local token issuer."""
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
from urllib.request import build_opener,ProxyHandler,HTTPRedirectHandler
from membership_execution import exercise_members,Client
from agent_platform import API_VERSION
ROOT=Path(__file__).resolve().parents[2]
class NoRedirect(HTTPRedirectHandler):
 def redirect_request(self,*args,**kwargs):return None
def main():
 parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--url',default='http://127.0.0.1:8080');parser.add_argument('--credentials',type=Path,default=ROOT/'.local/bootstrap.json');parser.add_argument('--node',default='node');parser.add_argument('--report',type=Path);parser.add_argument('--preflight-only',action='store_true');args=parser.parse_args()
 url=urlsplit(args.url)
 if url.scheme!='http' or not ipaddress.ip_address(url.hostname or '').is_loopback or url.username or url.password or url.path or url.query or url.fragment:parser.error('Use a literal loopback HTTP origin')
 if not args.preflight_only and args.report is None:parser.error('--report is required unless --preflight-only')
 if args.report is not None and args.report.exists():parser.error('Use a new report path to preserve prior evidence')
 with build_opener(ProxyHandler({}),NoRedirect()).open(args.url+'/healthz',timeout=10) as response:health=json.load(response)
 if health.get('data',{}).get('contract_version')!=API_VERSION:parser.error(f'Require API {API_VERSION}; no live fixture was created')
 if args.preflight_only:
  print(json.dumps({'contract_version':API_VERSION,'mutations':0,'scope':'health version only; credentials, issuer and permissions not checked'}));return 0
 credentials=json.loads(args.credentials.read_text());client=Client(args.url,token=credentials['token'],organization_id=credentials['organization_id']);assert client.get_current_identity()['role']=='owner'
 key='members-'+uuid.uuid4().hex;private=ROOT/'.local/qa/members'/key;private.mkdir(parents=True,mode=0o700);private.chmod(0o700)
 report={'contract_version':API_VERSION,'run_id':key,'started_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'scope':'actual local API/PostgreSQL, both SDKs, trusted private-file credential issuance, no response interception','results':[],'paid_inference_or_task_execution':False}
 try:
  report['results'].append(exercise_members(client,private,key+':python'))
  env=dict(os.environ,AGENT_PLATFORM_URL=args.url,AGENT_PLATFORM_CREDENTIALS=str(args.credentials.resolve()),AGENT_PLATFORM_QA_OUTPUT=str(private),MEMBERSHIP_RUN_KEY=key+':typescript',WORKSPACE_PYTHON=sys.executable)
  result=subprocess.run([args.node,'tests/sdk/membership_execution.ts'],cwd=ROOT,env=env,capture_output=True,text=True,timeout=120)
  if result.returncode:raise RuntimeError('TypeScript IAM live flow failed; private process output withheld')
  report['results'].append(json.loads(result.stdout));report['passed']=True
 except BaseException as error:
  report['passed']=False;report['failure_type']=type(error).__name__
  raise
 finally:
  args.report.parent.mkdir(parents=True,exist_ok=True);args.report.write_text(json.dumps(report,indent=2)+'\n')
 print(json.dumps(report))
if __name__=='__main__':main()

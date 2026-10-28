#!/usr/bin/env python3
# SPDX-License-Identifier: Elastic-2.0
"""Read-only real CLI stdio probe. Never prints credentials or tool data."""
import argparse, json, pathlib, selectors, subprocess, sys, time
parser=argparse.ArgumentParser()
parser.add_argument('--repo',type=pathlib.Path,required=True)
parser.add_argument('--task',help='Optional known current-organization task for read-only event replay')
args=parser.parse_args();repo=args.repo.resolve()
sys.path.insert(0,str(repo/'scripts'));import dev
credential_path=repo/'.local/bootstrap.json'
credential=json.loads(credential_path.read_text())
process=subprocess.Popen([str(repo/'bin/platform'),'mcp-stdio','--credentials',str(credential_path)],cwd=repo,env=dev.environment(),stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,bufsize=0)
selector=selectors.DefaultSelector();selector.register(process.stdout,selectors.EVENT_READ)
version='2026-07-28';buffer=b''
def call(request_id,method,params):
 global buffer
 params=dict(params);params['_meta']={'io.modelcontextprotocol/protocolVersion':version,'io.modelcontextprotocol/clientCapabilities':{}}
 frame=json.dumps({'jsonrpc':'2.0','id':request_id,'method':method,'params':params},ensure_ascii=False).encode()+b'\n'
 process.stdin.write(frame);process.stdin.flush()
 deadline=time.monotonic()+10
 while b'\n' not in buffer:
  remaining=deadline-time.monotonic()
  if remaining<=0 or not selector.select(remaining):raise RuntimeError('MCP CLI response timeout')
  chunk=process.stdout.read(65536)
  if not chunk:raise RuntimeError('MCP CLI closed before response')
  buffer+=chunk
  if len(buffer)>4*1024*1024:raise RuntimeError('MCP CLI response limit exceeded')
 line,buffer=buffer.split(b'\n',1)
 response=json.loads(line)
 if response.get('id')!=request_id:raise RuntimeError('MCP CLI response ID mismatch')
 return response
try:
 discovery=call(1,'server/discover',{})
 if discovery.get('result',{}).get('supportedVersions')!=[version]:raise RuntimeError('MCP CLI wrong protocol revision')
 listing=call(2,'tools/list',{})
 tools=listing.get('result',{}).get('tools',[])
 names={tool['name'] for tool in tools}
 if len(tools)!=50 or len(names)!=50 or not {'platform_api_compatibility','platform_event_snapshot','platform_event_feed','platform_login_policy_get','platform_service_accounts_list','platform_service_accounts_get','platform_service_keys_list','platform_service_keys_get','platform_security_audit_list','platform_identity','platform_model_proposals_list','platform_model_proposals_get','platform_members_capabilities','platform_members_list','platform_members_get','platform_membership_audit_list'} <= names or 'platform_tasks_create' in names:raise RuntimeError('MCP CLI readonly catalog differs')
 configuration_reads={'platform_configuration_catalog','platform_configuration_destinations_list','platform_configuration_resources_list','platform_configuration_grants_list','platform_configuration_grants_get','platform_configuration_plans_list','platform_configuration_plans_get','platform_configuration_verifications_list','platform_configuration_verifications_get','platform_configuration_audit_list'}
 configuration_mutations={'platform_configuration_grants_create','platform_configuration_grants_revoke','platform_configuration_plans_create','platform_configuration_plans_validate','platform_configuration_plans_approve','platform_configuration_plans_apply','platform_configuration_plans_verify'}
 if not configuration_reads <= names or configuration_mutations & names:raise RuntimeError('MCP CLI configuration defaults differ')
 identity=call(3,'tools/call',{'name':'platform_identity','arguments':{}})
 data=identity.get('result',{}).get('structuredContent',{}).get('data',{})
 if data.get('organization_id')!=credential.get('organization_id') or not data.get('principal_id'):raise RuntimeError('MCP CLI identity is not current bootstrap organization')
 denied=call(4,'tools/call',{'name':'platform_tasks_create','arguments':{'idempotency_key':'cli-readonly-probe','input':{'goal':'must not execute','agent_id':'agt_unavailable','budget_microusd':1000}}})
 if 'error' not in denied and not denied.get('result',{}).get('isError'):raise RuntimeError('MCP CLI mutation unexpectedly enabled')
 compatibility=call(5,'tools/call',{'name':'platform_api_compatibility','arguments':{}})
 if compatibility.get('result',{}).get('structuredContent',{}).get('data',{}).get('api_version')!='0.14.0':raise RuntimeError('MCP CLI API version mismatch')
 if args.task:
  snapshot=call(6,'tools/call',{'name':'platform_event_snapshot','arguments':{'task_id':args.task}}).get('result',{}).get('structuredContent',{}).get('data',{})
  if snapshot.get('task_id')!=args.task or snapshot.get('organization_id')!=credential['organization_id']:raise RuntimeError('MCP CLI event snapshot scope mismatch')
  page=call(7,'tools/call',{'name':'platform_event_feed','arguments':{'task_id':args.task,'cursor':snapshot['replay_cursor'],'limit':100}}).get('result',{}).get('structuredContent',{}).get('data',{})
  if page.get('task_id')!=args.task or page.get('reset_required') or not isinstance(page.get('cursor'),str) or not all(isinstance(e.get('sequence'),str) for e in page.get('events',[])):raise RuntimeError('MCP CLI event replay lost precise contract')
 process.stdin.close();process.wait(timeout=3)
 if process.returncode!=0:raise RuntimeError('MCP CLI did not shut down cleanly on EOF')
 print(json.dumps({'status':'PASS','transport':'real CLI stdio','protocol':version,'readonly_tools':len(names),'current_identity_verified':True,'api_version_verified':True,'event_snapshot_feed_verified':bool(args.task),'mutation_disabled':True,'clean_eof':True,'secret_output':False}))
finally:
 selector.close()
 if process.poll() is None:
  process.terminate()
  try:process.wait(timeout=2)
  except subprocess.TimeoutExpired:process.kill();process.wait()

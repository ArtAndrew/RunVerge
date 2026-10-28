// SPDX-License-Identifier: Apache-2.0
// Explicit isolated fixture flow; no mutation retries or business acceptance.
import assert from 'node:assert/strict';import fs from 'node:fs/promises';import {constants} from 'node:fs';import {randomUUID} from 'node:crypto';
import {Client,APIError,API_VERSION} from '../../packages/typescript-sdk/dist/index.js';
async function privateJSON(path){const file=await fs.open(path,constants.O_RDONLY|constants.O_NOFOLLOW|constants.O_NONBLOCK);try{const info=await file.stat();assert.ok(info.isFile()&&(info.mode&0o777)===0o600&&info.uid===process.getuid()&&info.size>0&&info.size<=32768,'Unsafe private fixture metadata');return JSON.parse(await file.readFile('utf8'))}finally{await file.close()}}
const delay=()=>new Promise(resolve=>setTimeout(resolve,100));
async function execute(m){
 const owner=new Client({baseUrl:m.base_url,organizationId:m.organization_id,token:m.token}),agent=new Client({baseUrl:m.base_url,organizationId:m.organization_id,token:m.service_token}),key=()=>({idempotencyKey:'ts-config-'+randomUUID()}),initial=await privateJSON(m.observations_file),catalog=await owner.configurationCatalog();
 const ids=[m.model_destination_id,m.mcp_destination_id],destinations=(await owner.configurationDestinations()).data.filter(d=>ids.includes(d.id));assert.equal(destinations.length,2);
 const grant=await owner.createConfigurationGrant({subject_id:m.service_principal_id,actions:['inspect','plan','validate','apply','verify'],destinations:destinations.map(d=>({id:d.id,version:d.version})),ttl_seconds:600,max_plans:2,max_verifications:2,max_allocated_charge_microusd:'10000',reason:'Explicit isolated TS configuration flow'},key());
 const resources=(await agent.configurationResources()).data,operations=destinations.map(d=>({id:'ts_'+d.kind,destination_id:d.id,destination_version:d.version,expected_resource_version:Math.max(0,...resources.filter(r=>r.kind===d.kind&&r.id===d.target_id).map(r=>r.version)),active:true}));
 const plan=await agent.createConfigurationPlan({grant_id:grant.id,base_revision:catalog.revision,ttl_seconds:600,operations,reason:'Exact model and MCP slots; apply must have no I/O'},key());
 const binding={expected_version:plan.version,plan_digest:plan.digest},validation=await agent.validateConfigurationPlan(plan.id,binding,key());assert.ok(validation.valid&&['allow','requires_approval'].includes(validation.decision.outcome));
 const approval=await owner.approveConfigurationPlan(plan.id,{...binding,validation_id:validation.id,reason:'Owner reviewed exact TS plan'},key());assert.equal(approval.plan_digest,plan.digest);
 const applied=await agent.applyConfigurationPlan(plan.id,{...binding,validation_id:validation.id,reason:'Apply exact checked settings'},key());assert.equal(applied.status,'applied');assert.equal(applied.version,2);assert.ok(applied.receipt_id);assert.deepEqual(await privateJSON(m.observations_file),initial,'Static configuration unexpectedly contacted a peer');
 const receipts=[];
 for(const operation of applied.operations){
  const request={expected_plan_version:applied.version,plan_digest:applied.digest,operation_id:operation.id,max_charge_microusd:operation.scope.kind==='model'?'10000':'0',acknowledge_cost:true,reason:'One explicit synthetic connection check'};
  let receipt=await agent.startConfigurationVerification(plan.id,request,key()),deadline=Date.now()+25000;
  while(['queued','sent'].includes(receipt.status)&&Date.now()<deadline){await delay();receipt=await agent.configurationVerification(receipt.id)}assert.equal(receipt.status,'response_received','Safe connection response was not received');
  if(receipt.kind==='model'){
   let task=await agent.getTask(receipt.task_id);deadline=Date.now()+25000;while(['queued','running'].includes(task.status)&&Date.now()<deadline){await delay();task=await agent.getTask(receipt.task_id)}
   assert.equal(task.status,'waiting_input','Safe model candidate requires a separate human decision');assert.ok(task.candidate_id&&!task.accepted_decision_id);assert.ok((await agent.candidates(task.id)).data.some(c=>c.id===task.candidate_id));assert.ok(!(await agent.decisions(task.id)).data.some(d=>d.decision==='accept'));
   const cost=await agent.costs(task.id);assert.ok(BigInt(cost.reserved_microusd)>0n,'Estimated usage must not settle unknown billing');assert.ok(receipt.response_digest);assert.equal(receipt.cost_status,'unknown');
  }else{assert.equal(receipt.cost_status,'unmetered');assert.equal(receipt.tool_count,1);assert.ok(receipt.catalog_digest)}
  await assert.rejects(agent.startConfigurationVerification(plan.id,request,key()),e=>e instanceof APIError&&e.statusCode===409);
  receipts.push(Object.fromEntries(['id','kind','status','cost_status','max_charge_microusd'].map(field=>[field,receipt[field]])));
 }
 const usage=await owner.configurationGrant(grant.id);assert.equal(usage.used_plans,1);assert.equal(usage.used_verifications,2);assert.equal(usage.allocated_charge_microusd,'10000');
 const revoked=await owner.revokeConfigurationGrant(grant.id,{expected_version:usage.version,reason:'Finish isolated delegation'},key());assert.ok(revoked.revoked);assert.equal(revoked.allocated_charge_microusd,'10000');assert.equal((await agent.configurationPlan(plan.id)).id,plan.id);
 await assert.rejects(agent.createConfigurationPlan({grant_id:grant.id,base_revision:(await owner.configurationCatalog()).revision,ttl_seconds:600,operations,reason:'Must fail after revoke'},key()),e=>e instanceof APIError&&e.statusCode===403);
 const final=await privateJSON(m.observations_file),delta=Object.fromEntries(['model_calls','mcp_discover_calls','mcp_list_calls','tools_call_calls','authentication_failures'].map(field=>[field,final[field]-initial[field]]));assert.deepEqual(delta,{model_calls:1,mcp_discover_calls:1,mcp_list_calls:1,tools_call_calls:0,authentication_failures:0});
 return{passed:true,contract_version:API_VERSION,scope:'actual isolated PostgreSQL/API/worker + synthetic HTTP model/TLS MCP; not main/paid provider acceptance',plan_id:plan.id,grant_id:grant.id,receipts,physical_request_delta:delta,no_hidden_POST_retry:true};
}
assert.equal(process.argv.length,4,'Usage: node configuration_execution.mjs PRIVATE_METADATA NEW_REPORT');const report=await execute(await privateJSON(process.argv[2]));await fs.writeFile(process.argv[3],JSON.stringify(report,null,2)+'\n',{flag:'wx'});console.log(JSON.stringify({passed:true,plan_id:report.plan_id,report:process.argv[3]}));

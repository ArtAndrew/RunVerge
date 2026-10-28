// SPDX-License-Identifier: Elastic-2.0
// Actual SDK -> explicitly installed local synthetic model/read/write peers.
import assert from 'node:assert/strict'
import {createHash,randomUUID} from 'node:crypto'
import {Client,APIError} from '../../packages/typescript-sdk/dist/index.js'
import type {Task,ToolExecutionView,TaskAutomationState} from '../../packages/typescript-sdk/dist/index.js'
const client=new Client({baseUrl:process.env.AGENT_PLATFORM_URL!,token:process.env.AGENT_PLATFORM_TOKEN!,organizationId:process.env.AGENT_PLATFORM_ORGANIZATION_ID!}),key='ts-automation-'+randomUUID(),timeout=Number(process.env.FIXTURE_WAIT_SECONDS??60)*1000,hash=(v:string)=>createHash('sha256').update(v).digest('hex'),wait=()=>new Promise(resolve=>setTimeout(resolve,100)),opt=(suffix:string)=>({idempotencyKey:key+':'+suffix})
const model=(await client.models({limit:100})).data.find(m=>m.model==='native-local-fixture')!,installed=(await client.tools({limit:100})).data,read=installed.find(t=>t.id==='tool_local_read_fixture')!,write=installed.find(t=>t.id==='tool_local_action_fixture')!
assert.ok(model&&read&&write);assert.equal(read.effect,'read');assert.equal(read.automation_allowed,true);assert.equal(read.require_distinct_approver,false);assert.equal(read.active,true);assert.equal(read.max_charge_microusd,'9');assert.equal(write.effect,'write');assert.equal(write.max_charge_microusd,'17');assert.ok(model.max_charge_microusd>=1&&model.max_charge_microusd<=10000)
async function waitTask(id:string,predicate:(t:Task)=>boolean){const end=Date.now()+timeout;while(true){const t=await client.getTask(id);if(predicate(t))return t;if(Date.now()>=end)throw new Error('Automation task did not reach expected state: '+t.status+':'+t.waiting_reason);await wait()}}
const cas=(v:ToolExecutionView)=>({expectedTaskVersion:v.task.version,expectedExecutionVersion:v.execution.version,expectedApprovalVersion:v.approval.version,intentHash:v.execution.intent_hash}),results=[]
for(const scenario of ['read_success','read_unknown','prepare_success'] as const){
 const mode=scenario==='prepare_success'?'prepare_and_continue':'read_only',tool=scenario==='prepare_success'?write:read,prefix=scenario+':'
 let agent=await client.createAgent({name:'TypeScript automation '+scenario,instructions:'Use the explicitly pinned synthetic local tool then return its observation.',model:model.model,tools:[tool.id],budgetMicrousd:100000},opt(prefix+'agent'));agent=await client.publishAgent(agent.id,{expectedVersion:agent.version,...opt(prefix+'publish')})
 let task=await client.createTask({goal:'Local automatic '+scenario,agentId:agent.id,budgetMicrousd:100000,maxRuns:1,executionTimeoutSeconds:600},opt(prefix+'task'));const taskId=task.id
 task=await waitTask(taskId,t=>t.waiting_reason==='model_tools_pending');const run=task.current_run_id;assert.equal((await client.toolExecutions(taskId)).data.length,0)
 const initial=await client.automation(taskId);assert.deepEqual(initial.usage,{model_turns:0,tool_executions:0,allocated_microusd:'0'});assert.equal(initial.grant,undefined)
 const pin=agent.model_tools![0]!
 let state:TaskAutomationState=await client.createAutomation(taskId,{expectedTaskVersion:task.version,mode,toolPins:[{tool_id:pin.tool_id,tool_version:pin.tool_version,tool_profile_digest:pin.tool_profile_digest}],maxModelTurns:4,maxToolExecutions:4,maxChargeMicrousd:String(4*model.max_charge_microusd+4*Number(tool.max_charge_microusd)),expiresInSeconds:600,acknowledgeCost:true,reason:'Delegate bounded synthetic local scenario'},opt(prefix+'grant'));const grantId=state.grant!.id
 if(mode==='prepare_and_continue'){
  task=await waitTask(taskId,t=>t.waiting_reason==='tool_approval_pending');const executions=(await client.toolExecutions(taskId)).data;assert.equal(executions.length,1)
  let view=await client.getToolExecution(taskId,executions[0]!.id);assert.equal(view.execution.status,'awaiting_approval');assert.equal(view.receipt,undefined);assert.equal((await client.automation(taskId)).usage.model_turns,0)
  view=await client.decideToolExecution(taskId,view.execution.id,{...cas(view),decision:'approve',reason:'Separately inspected exact synthetic write'},opt(prefix+'approve'));assert.equal(view.execution.status,'approved');assert.equal(view.receipt,undefined)
  await client.dispatchToolExecution(taskId,view.execution.id,{...cas(view),acknowledgeCost:true},opt(prefix+'dispatch'))
 }
 const unknown=scenario==='read_unknown';let final=await waitTask(taskId,t=>t.waiting_reason===(unknown?'external_action_unknown':'model_candidate_pending'))
 const executions=(await client.toolExecutions(taskId)).data;assert.equal(executions.length,1);const view=await client.getToolExecution(taskId,executions[0]!.id);assert.equal(view.execution.automation_grant_id,grantId);assert.equal(view.execution.status,unknown?'unknown':'succeeded');assert.equal(view.approval.decision_method,mode==='read_only'?'task_automation_grant':'authorized_principal_decision')
 const metadata=(await client.modelProposals(taskId)).data;assert.equal(metadata.length,1);const proposal=await client.getModelProposal(taskId,metadata[0]!.id);assert.ok(proposal.arguments_json!.includes('9007199254740993'));assert.equal(hash(proposal.arguments_json!),proposal.arguments_digest);assert.equal(final.current_run_id,run);assert.equal(final.attempt_count,1);assert.equal(final.accepted_decision_id,undefined);assert.equal((await client.decisions(taskId)).data.length,0)
 const end=Date.now()+timeout;state=await client.automation(taskId);while(state.grant!.status==='active'&&Date.now()<end){await wait();state=await client.automation(taskId)}assert.notEqual(state.grant!.status,'active');assert.equal(state.usage.tool_executions,1);assert.equal(state.usage.model_turns,unknown?0:1);assert.equal(state.usage.allocated_microusd,String(Number(tool.max_charge_microusd)+(unknown?0:model.max_charge_microusd)))
 const operationsBefore=(await client.operations(taskId)).data.length;assert.equal(operationsBefore,unknown?1:2);let acceptedSeparately=false,deniedUnknownResend:string|null=null
 if(unknown){
  await assert.rejects(client.dispatchToolExecution(taskId,view.execution.id,{...cas(view),acknowledgeCost:true},opt(prefix+'no-resend')),e=>e instanceof APIError&&e.statusCode===409);deniedUnknownResend='409';assert.ok((await client.progressSignals(taskId,{limit:1})).data.some(s=>s.code==='unknown_effect'))
  await new Promise(resolve=>setTimeout(resolve,350));assert.equal((await client.operations(taskId)).data.length,operationsBefore);assert.equal((await client.toolExecutions(taskId)).data.length,1);assert.deepEqual((await client.automation(taskId)).usage,state.usage)
 }else{
  assert.equal(view.receipt!.method,'registered_tool_response');const receiptArtifact=await client.getArtifact(taskId,view.receipt!.artifact_id,{version:1})
  if(mode==='read_only'){const observed=JSON.parse(receiptArtifact.content);assert.equal(observed.value,42);assert.equal(observed.read_only,true);assert.equal(observed.real_business_effect,false)}
  const candidate=(await client.candidates(taskId)).data.find(c=>c.id===final.candidate_id)!;assert.equal(candidate.source,'model');assert.equal(candidate.run_id,run);const artifact=await client.getArtifact(taskId,candidate.artifact_id,{version:candidate.artifact_version});assert.equal(hash(artifact.content),candidate.content_digest);assert.ok(artifact.content.includes('registered_tool_response'))
  final=await client.reviewTask(taskId,{expectedVersion:final.version,candidateId:candidate.id,artifactId:candidate.artifact_id,artifactVersion:candidate.artifact_version,contentDigest:candidate.content_digest,decision:'accept',reason:'Separately accept the synthetic fixture candidate'},opt(prefix+'accept'));assert.equal(final.status,'succeeded');acceptedSeparately=true
 }
 const cost=await client.costs(taskId);assert.equal(cost.spent_microusd,'0');assert.equal(cost.reserved_microusd,String((unknown?1:2)*model.max_charge_microusd+Number(tool.max_charge_microusd)))
 results.push({scenario,mode,task_id:taskId,run_id:run,grant_id:grantId,execution_id:view.execution.id,action_id:view.execution.action_id,approval_method:view.approval.decision_method,final_status:final.status,final_accepted_separately:acceptedSeparately,unknown_resend:deniedUnknownResend,usage:state.usage,model_operations:operationsBefore,spent_microusd:cost.spent_microusd,reserved_microusd:cost.reserved_microusd})
}
console.log(JSON.stringify({sdk:'typescript',native_fixture_model:'native-local-fixture',read_only_tool:'tool_local_read_fixture',paid_provider_call:false,billing_settlement:false,results}))

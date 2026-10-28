// SPDX-License-Identifier: Elastic-2.0
// Opt-in scenario: the outer health-gated runner supplies explicit local fixture IDs.
import assert from 'node:assert/strict'
import {createHash,randomUUID} from 'node:crypto'
import {Client,APIError} from '../../packages/typescript-sdk/dist/index.js'
import type {ToolExecutionView,ToolInfo} from '../../packages/typescript-sdk/dist/index.js'
const client=new Client({baseUrl:process.env.AGENT_PLATFORM_URL!,token:process.env.AGENT_PLATFORM_TOKEN!,organizationId:process.env.AGENT_PLATFORM_ORGANIZATION_ID!})
const fixtures=JSON.parse(process.env.AGENT_PLATFORM_TOOL_FIXTURES_JSON!) as Record<'success'|'unknown',{tool_id:string;arguments_json:string}>
const key='ts-tool-'+randomUUID(),timeout=Number(process.env.FIXTURE_WAIT_SECONDS??60)*1000,hash=(text:string)=>createHash('sha256').update(text).digest('hex'),wait=()=>new Promise(resolve=>setTimeout(resolve,100)),opt=(suffix:string)=>({idempotencyKey:key+':'+suffix})
const catalog:ToolInfo[]=[];let cursor:string|undefined
do{const page=await client.tools({limit:1,...(cursor?{cursor}:{})});catalog.push(...page.data);cursor=page.next_cursor;assert.ok(catalog.length<=128)}while(cursor)
for(const fixture of Object.values(fixtures)){const tool=catalog.find(item=>item.id===fixture.tool_id)!;assert.ok(tool.active&&!tool.require_distinct_approver);assert.equal(tool.max_charge_microusd,'17');assert.equal('endpoint' in tool,false);assert.equal('credential_environment' in tool,false)}
let leaf=await client.createAgent({name:'TS tool fixture',instructions:'Echo synthetic evidence only.',model:'fixture:echo',tools:[],budgetMicrousd:10000},opt('leaf'));leaf=await client.publishAgent(leaf.id,{expectedVersion:leaf.version,...opt('leaf-publish')})
let agent=await client.createAgent({name:'TS exact tool team',instructions:'Aggregate synthetic evidence.',model:'team:coordinator',tools:[],budgetMicrousd:10000,team:{pattern:'parallel',members:[{id:'first',agent_id:leaf.id,agent_version:leaf.version},{id:'second',agent_id:leaf.id,agent_version:leaf.version}],join_policy:'all',max_depth:1,max_children:2,max_parallel:2}},opt('team'));agent=await client.publishAgent(agent.id,{expectedVersion:agent.version,...opt('publish')})
const results=[]
const cas=(view:ToolExecutionView)=>({expectedTaskVersion:view.task.version,expectedExecutionVersion:view.execution.version,expectedApprovalVersion:view.approval.version,intentHash:view.execution.intent_hash})
for(const mode of ['success','unknown'] as const){
 const fixture=fixtures[mode],tool=catalog.find(item=>item.id===fixture.tool_id)!,prefix=mode
 let task=await client.createTask({goal:'Check synthetic tool '+mode,agentId:agent.id,budgetMicrousd:10000,maxRuns:1,executionTimeoutSeconds:600},opt(prefix+':task'));const taskId=task.id
 let end=Date.now()+timeout;while(task.status!=='waiting_input'&&Date.now()<end){assert.ok(!['blocked','failed','cancelled'].includes(task.status));await wait();task=await client.getTask(taskId)}assert.equal(task.status,'waiting_input')
 const candidate=(await client.candidates(taskId)).data.find(item=>item.id===task.candidate_id)!,artifact=await client.getArtifact(taskId,candidate.artifact_id,{version:candidate.artifact_version});assert.equal(hash(artifact.content),candidate.content_digest)
 const before=await client.costs(taskId);assert.equal(before.spent_microusd,'2000');assert.equal(before.reserved_microusd,'0');const run=task.current_run_id,sourceCandidate=task.candidate_id
 let view=await client.prepareToolExecution(taskId,{occurrenceId:key+':'+mode,expectedTaskVersion:task.version,candidateId:candidate.id,artifactId:artifact.id,artifactVersion:artifact.version,contentDigest:candidate.content_digest,toolId:tool.id,toolVersion:tool.version,argumentsJson:fixture.arguments_json,expiresInSeconds:900},opt(prefix+':prepare'))
 const executionId=view.execution.id;assert.ok(view.task.version>task.version);assert.equal(view.task.status,'waiting_approval');assert.equal(view.execution.arguments_json,fixture.arguments_json);assert.equal('arguments_json' in (await client.toolExecutions(taskId,{limit:1})).data[0]!,false)
 view=await client.getToolExecution(taskId,executionId);const intent=view.execution.intent_hash
 view=await client.decideToolExecution(taskId,executionId,{...cas(view),decision:'approve',reason:'Exact local fixture intent reviewed. '+'x'.repeat(300)},opt(prefix+':approve'));assert.equal(view.execution.status,'approved');assert.equal(view.task.status,'waiting_approval');assert.equal(view.receipt,undefined)
 view=await client.dispatchToolExecution(taskId,executionId,{...cas(view),acknowledgeCost:true},opt(prefix+':dispatch'))
 const expected=mode==='success'?'succeeded':'unknown';end=Date.now()+timeout;while(view.execution.status!==expected&&Date.now()<end){assert.ok(['queued','leased','sent',expected].includes(view.execution.status));await wait();view=await client.getToolExecution(taskId,executionId)}assert.equal(view.execution.status,expected);assert.equal(view.execution.intent_hash,intent)
 let unknownResend:null|string=null
 if(mode==='unknown'){
  assert.equal(view.task.status,'blocked');assert.equal(view.task.waiting_reason,'external_action_unknown')
  await assert.rejects(client.dispatchToolExecution(taskId,executionId,{...cas(view),acknowledgeCost:true},opt(prefix+':forbidden-resend')),error=>error instanceof APIError&&error.statusCode===409);unknownResend='409'
  view=await client.getToolExecution(taskId,executionId)
  view=await client.reconcileToolExecution(taskId,executionId,{expectedTaskVersion:view.task.version,expectedExecutionVersion:view.execution.version,intentHash:intent,outcome:'rejected',effectReference:'local-fixture-operator-observation',result:'',evidenceDigest:hash(key+':'+mode+':assertion'),reason:'Synthetic operator assertion, not a verified external fact or bill.'},opt(prefix+':reconcile'));assert.equal(view.receipt!.method,'operator_assertion')
 }else assert.equal(view.receipt!.method,'registered_tool_response')
 const exact=await client.getArtifact(taskId,view.receipt!.artifact_id,{version:1});assert.equal(hash(exact.content),view.receipt!.content_digest)
 const after=await client.costs(taskId),fresh=await client.getTask(taskId);assert.equal(after.spent_microusd,before.spent_microusd);assert.equal(after.reserved_microusd,'17');assert.equal(fresh.status,'waiting_input');assert.equal(fresh.current_run_id,run);assert.equal(fresh.candidate_id,sourceCandidate);assert.equal(fresh.accepted_decision_id,undefined);assert.equal((await client.decisions(taskId)).data.length,0)
 results.push({mode,task_id:taskId,execution_id:executionId,execution_status:view.execution.status,task_status:fresh.status,receipt_method:view.receipt!.method,source_run_preserved:true,candidate_not_accepted:true,spent_microusd:after.spent_microusd,reserved_microusd:after.reserved_microusd,unknown_resend:unknownResend,arguments_digest:view.execution.arguments_digest})
}
console.log(JSON.stringify({sdk:'typescript',fixture_tool_only:true,paid_model_call:false,billing_settlement:false,results}))

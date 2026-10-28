// SPDX-License-Identifier: Apache-2.0
// Opt-in: actual platform API/worker, fixture:echo participants only.
import assert from 'node:assert/strict';
import {createHash,randomUUID} from 'node:crypto';
import {Client,APIError} from '../../packages/typescript-sdk/dist/index.js';
import type {Task} from '../../packages/typescript-sdk/dist/index.js';
const client=new Client({baseUrl:process.env.AGENT_PLATFORM_URL??'http://127.0.0.1:8080',token:process.env.AGENT_PLATFORM_TOKEN!,organizationId:process.env.AGENT_PLATFORM_ORGANIZATION_ID!});
const key='ts-review-'+randomUUID(),waitMs=Number(process.env.FIXTURE_WAIT_SECONDS??60)*1000;
let leaf=await client.createAgent({name:'TS review fixture',instructions:'Repeat execution input exactly.',model:'fixture:echo',tools:[],budgetMicrousd:10000},{idempotencyKey:key+':leaf'});
leaf=await client.publishAgent(leaf.id,{expectedVersion:leaf.version,idempotencyKey:key+':leaf-publish'});
let agent=await client.createAgent({name:'TS fixture team',instructions:'Aggregate synthetic materials.',model:'team:coordinator',tools:[],budgetMicrousd:10000,team:{pattern:'parallel',members:[{id:'first',agent_id:leaf.id,agent_version:leaf.version,task:'Prepare first evidence'},{id:'second',agent_id:leaf.id,agent_version:leaf.version,task:'Prepare second evidence'}],join_policy:'all',max_depth:1,max_children:2,max_parallel:2}},{idempotencyKey:key+':team'});
agent=await client.publishAgent(agent.id,{expectedVersion:agent.version,idempotencyKey:key+':publish'});
let task=await client.createTask({goal:'Exercise explicit review and a new attempt using only fixtures',agentId:agent.id,budgetMicrousd:10000,maxRuns:2,executionTimeoutSeconds:600},{idempotencyKey:key+':task'});
const taskId=task.id;
async function waitCandidate():Promise<Task>{const end=Date.now()+waitMs;while(Date.now()<end){const current=await client.getTask(taskId);if(current.status==='waiting_input'&&current.candidate_id)return current;assert.ok(!['failed','cancelled','blocked'].includes(current.status),'fixture team stopped before candidate: '+current.status+'/'+current.waiting_reason);await new Promise(r=>setTimeout(r,150))}throw new Error('fixture team timed out')}
async function binding(current:Task){const candidate=(await client.candidates(taskId)).data.find(v=>v.id===current.candidate_id)!;const artifact=await client.getArtifact(taskId,candidate.artifact_id,{version:candidate.artifact_version});assert.equal(candidate.source,'team');assert.equal(candidate.run_id,current.current_run_id);assert.equal(createHash('sha256').update(artifact.content).digest('hex'),candidate.content_digest);return {expectedVersion:current.version,candidateId:candidate.id,artifactId:artifact.id,artifactVersion:artifact.version,contentDigest:candidate.content_digest}}
task=await waitCandidate();const firstRun=task.current_run_id,initial=await client.costs(taskId);assert.equal(task.attempt_count,1);assert.equal(task.max_runs,2);assert.equal(initial.spent_microusd,'2000');
const blocked=await client.reviewTask(taskId,{...await binding(task),decision:'request_changes',reason:'Add an explicit limitations statement'},{idempotencyKey:key+':review'});
assert.equal(blocked.status,'blocked');assert.equal(blocked.current_run_id,firstRun);assert.equal(blocked.attempt_count,1);assert.equal((await client.costs(taskId)).spent_microusd,'2000');
const feedback='Label evidence as synthetic fixture and add its limitations.';
const retry={expectedVersion:blocked.version,reason:'Check a new bounded attempt',feedback,acknowledgeNewCost:true as const};
const queued=await client.retryTask(taskId,retry,{idempotencyKey:key+':retry'});assert.deepEqual(await client.retryTask(taskId,retry,{idempotencyKey:key+':retry'}),queued);
task=await waitCandidate();assert.equal(task.attempt_count,2);assert.notEqual(task.current_run_id,firstRun);const run=(await client.runs(taskId)).data.find(v=>v.id===task.current_run_id)!;assert.equal(run.feedback,feedback);assert.equal(run.source_candidate_id,blocked.candidate_id);assert.equal(run.input_digest?.length,64);
await assert.rejects(client.reviewTask(taskId,{...await binding(blocked),decision:'accept'},{idempotencyKey:key+':stale'}),e=>e instanceof APIError&&e.statusCode===409);
task=await client.reviewTask(taskId,{...await binding(task),decision:'accept',reason:'Accept the synthetic test output only'},{idempotencyKey:key+':accept'});assert.equal(task.status,'succeeded');assert.ok(task.accepted_decision_id);
const decisions=(await client.decisions(taskId)).data;assert.equal(decisions.length,2);assert.deepEqual(new Set(decisions.map(v=>v.decision)),new Set(['request_changes','accept']));assert.ok(decisions.every(v=>v.method==='authorized_user_decision'));
const children:Task[]=[];let cursor:string|undefined;do{const page=await client.children(taskId,{limit:1,...(cursor===undefined?{}:{cursor})});children.push(...page.data);cursor=page.next_cursor;assert.ok(children.length<=128)}while(cursor!==undefined);assert.equal(children.length,4);assert.equal(new Set(children.map(v=>v.id)).size,4);
const costs=await client.costs(taskId);assert.equal(costs.spent_microusd,'4000');assert.equal(costs.reserved_microusd,'0');assert.ok(costs.entries.every(v=>v.synthetic));
console.log(JSON.stringify({sdk:'typescript',task_id:taskId,status:task.status,attempt_count:task.attempt_count,children:children.length,candidates:(await client.candidates(taskId)).data.length,decisions:decisions.length,initial_synthetic_spent_microusd:initial.spent_microusd,synthetic_spent_microusd:costs.spent_microusd,reserved_microusd:costs.reserved_microusd,retry_replay:'same cached response',stale_candidate:'409',feedback_pinned:true,real_provider_call:false}));

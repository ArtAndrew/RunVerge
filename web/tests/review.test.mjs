import {test} from 'node:test'
import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {mayReviewTask,bindCandidate,freshExecutionMetadata,reviewPayload,retryPayload,retryBlockReason} from '../src/review.ts'

const text='Непроверенный черновик\n  Точные пробелы сохраняются.\n'
const digest=createHash('sha256').update(text,'utf8').digest('hex')
const task={id:'task',organization_id:'org',version:8,current_run_id:'run-1',attempt_count:1,max_runs:3,execution_deadline:'2099-01-01T00:00:00Z',candidate_id:'candidate-1',status:'waiting_input',waiting_reason:'model_candidate_pending',reserved_microusd:'150',spent_microusd:'0',budget_microusd:1000,financial_hold:false}
const candidate={id:'candidate-1',organization_id:'org',task_id:'task',run_id:'run-1',operation_id:'op-1',artifact_id:'artifact-1',artifact_version:2,content_digest:digest}
const artifact={id:'artifact-1',organization_id:'org',task_id:'task',version:2,content:text}

test('review binds exact UTF-8 bytes and a frozen current task/candidate/artifact snapshot',async()=>{
 const original={...task};const snapshot=await bindCandidate(original,[candidate],[artifact]);assert.ok(snapshot)
 original.version++;assert.equal(snapshot.task.version,8)
 assert.deepEqual(reviewPayload(snapshot,'request_changes','Добавьте обоснование'),{expected_version:8,candidate_id:'candidate-1',artifact_id:'artifact-1',artifact_version:2,content_digest:digest,decision:'request_changes',reason:'Добавьте обоснование'})
 assert.equal(snapshot.artifact.content,text)
})
test('wrong run tenant artifact version or changed text cannot become a reviewable result',async()=>{
 for(const change of [{run_id:'old-run'},{organization_id:'other-org'},{task_id:'other-task'},{artifact_version:1},{content_digest:digest+'\n'}])await assert.rejects(bindCandidate(task,[{...candidate,...change}],[artifact]))
 await assert.rejects(bindCandidate(task,[candidate],[{...artifact,content:text.trim()}]))
 await assert.rejects(bindCandidate(task,[candidate],[{...artifact,organization_id:'other-org'}]))
 assert.equal(await bindCandidate({...task,candidate_id:undefined},[candidate],[artifact]),null)
})
test('cached old response metadata is readable but never fabricates execution limits',async()=>{
 const cached={...task,attempt_count:undefined,max_runs:undefined,execution_deadline:undefined}
 assert.equal(freshExecutionMetadata(cached),false);await assert.rejects(bindCandidate(cached,[candidate],[artifact]))
 assert.match(retryBlockReason(cached),/Обновите/)
})
test('review reason is required for reject/change and bounded by UTF-8 bytes, not characters',async()=>{
 const snapshot=await bindCandidate(task,[candidate],[artifact])
 assert.doesNotThrow(()=>reviewPayload(snapshot,'accept',''))
 for(const decision of ['reject','request_changes'])assert.throws(()=>reviewPayload(snapshot,decision,' '))
 for(const reason of ['я'.repeat(2049),'a\0b','\ud800'])assert.throws(()=>reviewPayload(snapshot,'accept',reason))
 assert.throws(()=>reviewPayload(snapshot,'approve','Reason'))
})
test('new attempt keeps old hold values and requires explicit cost ack and corrective feedback',()=>{
 const blocked={...task,status:'blocked',waiting_reason:'candidate_changes_requested'}
 assert.throws(()=>retryPayload(blocked,'New attempt','Revise',false))
 assert.throws(()=>retryPayload(blocked,'New attempt',' ',true))
 const payload=retryPayload(blocked,'New attempt','Use these sources',true)
 assert.deepEqual(payload,{expected_version:8,reason:'New attempt',feedback:'Use these sources',acknowledge_new_cost:true})
 assert.equal(blocked.reserved_microusd,'150');assert.equal(blocked.spent_microusd,'0')
 for(const mutation of [{parent_id:'parent'},{financial_hold:true},{attempt_count:3},{execution_deadline:'2000-01-01T00:00:00Z'},{status:'failed'},{status:'cancelled'},{status:'running'},{waiting_reason:'arbitrary_policy_hold'}])assert.throws(()=>retryPayload({...blocked,...mutation},'New attempt','Revise',true))
})


test('known unsent automation and model cleanup permit only an explicit bounded new attempt',()=>{
 for(const waiting_reason of ['model_attempt_requires_attention','automation_authorization_revoked']){
  const blocked={...task,status:'blocked',waiting_reason};
  assert.equal(retryBlockReason(blocked),null);
  assert.throws(()=>retryPayload(blocked,'Reviewed unsent cleanup','Correct access',false));
  assert.equal(retryPayload(blocked,'Reviewed unsent cleanup','Correct access',true).acknowledge_new_cost,true);
  assert.throws(()=>retryPayload({...blocked,financial_hold:true},'Reviewed cleanup','Correct access',true));
 }
 assert.match(retryBlockReason({...task,status:'blocked',waiting_reason:'external_action_unknown'}),/Причина/);
});

test('human review requires explicit current kind exact organization and own or any permission',()=>{const target={...task,owner_id:'owner'},identity={organization_id:'org',principal_id:'owner',principal_kind:'human',role:'member',permissions:['tasks.review_own']};assert.equal(mayReviewTask(identity,target),true);assert.equal(mayReviewTask({...identity,principal_id:'other',permissions:['tasks.review_any']},target),true);for(const patch of[{principal_kind:undefined},{principal_kind:'unknown'},{principal_kind:'service'},{organization_id:'foreign'},{permissions:[]},{principal_id:'other'}])assert.equal(mayReviewTask({...identity,...patch},target),false);assert.equal(mayReviewTask(null,target),false)});

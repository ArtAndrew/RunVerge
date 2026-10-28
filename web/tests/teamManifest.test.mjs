// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {parseTeamResult,teamResultLimits,exactTextBytes} from '../src/teamManifest.ts'

const hash=content=>createHash('sha256').update(content).digest('hex')
const source=(task,content='  Точный текст 👋\n\tВторая строка\r\n')=>({task_id:task,candidate_id:`candidate-${task}`,artifact_id:`artifact-${task}`,artifact_version:2,content_digest:hash(content),content})
const member=(id='author')=>({id,task_id:`task-${id}`,run_id:`run-${id}`,status:'candidate',result:source(`task-${id}`)})
const manifest=()=>({version:1,pattern:'parallel',join_policy:'all',complete:true,all_fixture:false,verification:'awaiting_authorized_decision',members:[member('author'),member('verifier')]})
const parse=value=>parseTeamResult(JSON.stringify(value))
const rejected=value=>assert.equal(parse(value).ok,false)

test('canonical complete, partial and delegate manifests preserve exact quoted source bytes',()=>{
 const complete=manifest(),parsed=parse(complete);assert.equal(parsed.ok,true);assert.deepEqual(parsed.manifest,complete)
 assert.equal(parsed.manifest.members[0].result.content,complete.members[0].result.content)
 assert.equal(parsed.bytes,exactTextBytes(JSON.stringify(complete)))
 const partial={...manifest(),join_policy:'allow_partial',complete:false,members:[member(),{id:'verifier',task_id:'task-verifier',run_id:'run-verifier',status:'failed'}]};assert.equal(parse(partial).ok,true)
 assert.equal(parse({...manifest(),pattern:'delegate_verify'}).ok,true)
 assert.equal(parse({...manifest(),pattern:'sequence'}).ok,true)
})
test('JSON-looking nested team and hostile HTML are opaque quoted text',()=>{
 const nested='{"version":1,"members":'+ '['.repeat(100)+'0'+']'.repeat(100)+'}';
 const hostile='<img src="https://invalid.example/pixel" onerror="globalThis.compromised=1"><script>globalThis.compromised=1</script>'
 for(const content of [nested,hostile]){const value=manifest();value.members[0].result=source('task-author',content);const parsed=parse(value);assert.equal(parsed.ok,true);assert.equal(parsed.manifest.members[0].result.content,content)}
 assert.equal(globalThis.compromised,undefined)
})
test('reject malformed roots, unknown versions and wrong primitive types without coercion',()=>{
 for(const value of [null,[],{},'x',{...manifest(),version:2},{...manifest(),version:'1'},{...manifest(),pattern:['parallel']},{...manifest(),join_policy:['all']},{...manifest(),complete:'true'},{...manifest(),all_fixture:0},{...manifest(),verification:'verified'}, {...manifest(),extra:true}])rejected(value)
 for(const content of ['', '{', '{} garbage', '{"version":1,}', 'null','true','[[[]]]'])assert.equal(parseTeamResult(content).ok,false)
 const wrongStatus=manifest();wrongStatus.members[0].status=['candidate'];rejected(wrongStatus)
})
test('duplicate JSON property names and escaped aliases never silently change the summary',()=>{
 const good=JSON.stringify(manifest());
 for(const content of [good.replace('"version":1','"version":1,"version":1'),good.replace('"version":1','"version":1,"\\u0076ersion":1'),good.replace('"id":"author"','"id":"author","id":"verifier"'),good.replace('"content":','"content":"forged","content":'),good.replace('"version":1','"__proto__":{"polluted":true},"version":1')])assert.equal(parseTeamResult(content).ok,false)
 assert.equal({}.polluted,undefined)
})
test('presentation never turns a missing, running or inconsistent source into a complete result',()=>{
 for(const mutate of [v=>{v.complete=false},v=>{v.members[0].status='running'},v=>{delete v.members[0].result},v=>{delete v.members[0].run_id},v=>{v.members[0].result.task_id='other-task'},v=>{v.members[0].result.artifact_version=0},v=>{v.members[0].result.artifact_version=Number.MAX_SAFE_INTEGER+1},v=>{v.members[0].result.artifact_version='2'},v=>{v.members[0].result.content_digest+='\n'},v=>{v.members[0].result.content_digest='A'.repeat(64)},v=>{v.members[0].result.extra=true},v=>{v.members[0].status='failed'},v=>{v.members[0].run_id=null}]){const v=manifest();mutate(v);rejected(v)}
 const omitted={id:'failed',task_id:'failed-task',status:'failed'}
 rejected({...manifest(),members:[omitted],complete:false,join_policy:'all'})
 rejected({...manifest(),members:[omitted],complete:false,join_policy:'allow_partial'})
 rejected({...manifest(),members:[member(),omitted],complete:true,join_policy:'allow_partial'})
 rejected({...manifest(),members:[member(),omitted],complete:false,join_policy:'all'})
})
test('member identity uniqueness, count, required IDs and delegate arity are bounded',()=>{
 rejected({...manifest(),members:[]});rejected({...manifest(),members:Array.from({length:33},(_,n)=>member(`m${n}`))})
 assert.equal(parse({...manifest(),members:Array.from({length:32},(_,n)=>member(`m${n}`))}).ok,true)
 rejected({...manifest(),members:[member(),member()]})
 const duplicateTask=manifest();duplicateTask.members[1].task_id=duplicateTask.members[0].task_id;rejected(duplicateTask)
 for(const id of ['', ' ', 'x'.repeat(129),'я'.repeat(65),'nul\0id','\ud800']){const v=manifest();v.members[0].id=id;rejected(v)}
 rejected({...manifest(),pattern:'delegate_verify',members:[member()]})
 rejected({...manifest(),pattern:'delegate_verify',members:[member(),member('b'),member('c')]})
})
test('bounds apply before parse: UTF-8 size, container depth and number of values',()=>{
 assert.deepEqual(parseTeamResult(' '.repeat(teamResultLimits.bytes+1)),{ok:false,reason:'size'})
 assert.deepEqual(parseTeamResult('я'.repeat(teamResultLimits.bytes/2+1)),{ok:false,reason:'size'})
 assert.deepEqual(parseTeamResult('['.repeat(5000)+'0'+']'.repeat(5000)),{ok:false,reason:'depth'})
 assert.deepEqual(parseTeamResult('['+Array(5000).fill('0').join(',')+']'),{ok:false,reason:'count'})
 const value=manifest();value.members[0].result.content='я'.repeat(131000);assert.deepEqual(parse(value),{ok:false,reason:'size'})
})
test('source NUL and invalid Unicode are rejected while ordinary literal escapes are preserved',()=>{
 for(const content of ['a\0b','\ud800']){const value=manifest();value.members[0].result.content=content;rejected(value)}
 const value=manifest();value.members[0].result=source('task-author','literal \\u0000 and \\n');assert.equal(parse(value).manifest.members[0].result.content,'literal \\u0000 and \\n')
})

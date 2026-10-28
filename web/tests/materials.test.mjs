// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {bindMaterialContent,bindMaterialPublication,createMaterialPayload,publicationBlockReason,publishMaterialPayload,reviseMaterialPayload} from '../src/materials.ts'
const text='  Итоговый текст 👋\r\n',hash=content=>createHash('sha256').update(content).digest('hex')
const task={id:'task',organization_id:'org',version:8,current_run_id:'run',candidate_id:'original',status:'waiting_input',attempt_count:1,max_runs:3,execution_deadline:'2099-01-01T00:00:00Z',financial_hold:false}
const head={id:'material',task_id:'task',organization_id:'org',name:'result.md',media_type:'text/markdown',head_version:3,head_content_digest:hash(text),base_candidate_id:'original'}
const body={revision:{id:'revision-3',organization_id:'org',task_id:'task',material_id:'material',version:3,base_version:2,content_digest:hash(text)},content:text}

test('exact material binding checks task organization head version and digest',async()=>{
 assert.deepEqual(await bindMaterialContent(task,head,body),body)
 for(const altered of[{task_id:'other'},{organization_id:'other'},{id:''},{head_version:2},{head_content_digest:hash(text)+'\n'}])await assert.rejects(bindMaterialContent(task,{...head,...altered},body))
 for(const altered of[{task_id:'other'},{organization_id:'other'},{material_id:'other'},{version:2},{content_digest:'a'.repeat(64)}])await assert.rejects(bindMaterialContent(task,head,{...body,revision:{...body.revision,...altered}}))
 await assert.rejects(bindMaterialContent(task,head,{...body,content:text.trim()}))
})
test('draft saving permits empty or unfinished JSON but retains UTF-8 and name bounds',()=>{
 assert.equal(createMaterialPayload({name:'notes',media_type:'text/plain',content:''}).content,'')
 assert.equal(createMaterialPayload({name:'draft.json',media_type:'application/json',content:'{"unfinished":'}).content,'{"unfinished":')
 for(const change of[{name:' '},{name:'я'.repeat(61)},{media_type:'text/html'},{content:'я'.repeat(131073)},{content:'a\0b'},{content:'\ud800'},{reason:'я'.repeat(2049)}])assert.throws(()=>createMaterialPayload({name:'notes',media_type:'text/plain',content:'',...change}))
})
test('manual merge pins the latest head and original base without altering caller buffer',()=>{
 const content='My exact local buffer\r\n';assert.deepEqual(reviseMaterialPayload(head,content,'Manual merge',1),{expected_version:3,content,reason:'Manual merge',merge_base_version:1});assert.equal(head.head_version,3)
 for(const mergeBase of[0,4,1.1])assert.throws(()=>reviseMaterialPayload(head,content,'Manual merge',mergeBase))
 assert.throws(()=>reviseMaterialPayload(head,content,' '))
})
test('publication snapshots exact saved bytes, current parent and task version with no execution fields',async()=>{
 const current={...task},snapshot=await bindMaterialPublication(current,head,body);current.version++
 assert.deepEqual(publishMaterialPayload(snapshot),{expected_task_version:8,expected_material_version:3,content_digest:hash(text),parent_candidate_id:'original'})
 assert.equal(snapshot.content.content,text);assert.equal(snapshot.task.current_run_id,'run')
 assert.equal(publicationBlockReason({...task,financial_hold:true}),null)
})
test('publication requires nonblank content and valid JSON, independently of draft acceptance',async()=>{
 for(const content of['','   ','{"unfinished":']){
  const jsonHead={...head,media_type:'application/json',head_content_digest:hash(content)},jsonBody={content,revision:{...body.revision,content_digest:hash(content)}}
  assert.doesNotThrow(()=>createMaterialPayload({name:'notes',media_type:'application/json',content}));await assert.rejects(bindMaterialPublication(task,jsonHead,jsonBody))
 }
 const content='{"quoted":"<script>alert(1)</script>"}',digest=hash(content);assert.equal((await bindMaterialPublication(task,{...head,media_type:'application/json',head_content_digest:digest},{content,revision:{...body.revision,content_digest:digest}})).content.content,content)
})
test('stopped runs terminal tasks and missing execution metadata cannot publish',async()=>{
 for(const change of[{status:'blocked',waiting_reason:'candidate_changes_requested'},{status:'succeeded'},{status:'failed'},{status:'cancelled'},{status:'running'},{candidate_id:undefined},{current_run_id:undefined},{attempt_count:undefined}]){assert.ok(publicationBlockReason({...task,...change}));await assert.rejects(bindMaterialPublication({...task,...change},head,body))}
})

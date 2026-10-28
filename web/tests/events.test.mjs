import {test} from 'node:test'
import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {ApiClient} from '../src/api.ts'
import {EventJournal,eventReconnectDelay} from '../src/eventJournal.ts'
import {validateEvent} from '../../packages/typescript-sdk/src/events.ts'
const corpus=JSON.parse(readFileSync(new URL('../../contracts/event-conformance.json',import.meta.url),'utf8'))
const org='org_contract',task='task_contract',now='2026-10-24T12:00:00Z'
const event=(sequence='9007199254740993')=>({id:'e'+sequence,organization_id:org,task_id:task,sequence,type:'old.kind',payload_version:0,legacy_data_json:'{"n":9007199254740993}',created_at:now})
const batch=(events=[],cursor='cursor')=>({organization_id:org,task_id:task,events,cursor,has_more:false,reset_required:false,window:{after_sequence:'0',through_sequence:'9223372036854775807',version:1}})
const snapshot=events=>({organization_id:org,task_id:task,task:{id:task,organization_id:org,reserved_microusd:'0',spent_microusd:'0'},events,cursor:'head',replay_cursor:'floor',window:{after_sequence:'0',through_sequence:events.at(-1)?.sequence??'0',version:1},has_earlier_events:false,observed_at:now})
const wire=value=>`event: platform.events\nid: ${value.cursor}\ndata: ${JSON.stringify({frame_version:1,kind:'batch',batch:value})}\n\n`
const headers={'X-Platform-API-Version':'0.14.0','Content-Type':'text/event-stream'}
test('UI validates exactly the same public event corpus as both SDKs',()=>{for(const item of corpus.cases)if(item.valid)assert.deepEqual(validateEvent(item.event,org,task),item.event,item.name);else assert.throws(()=>validateEvent(item.event,org,task),undefined,item.name)})
test('bounded journal preserves exact order beyond 2^53, dedupes, rejects conflicting duplicates without adopting cursor',()=>{
 const journal=new EventJournal(2);journal.snapshot(snapshot([event()]));assert.equal(journal.batch(batch([event(),'9007199254740994','9007199254740995'].map(v=>typeof v==='string'?event(v):v),'next')).length,2);assert.equal(journal.truncated,true);assert.deepEqual(journal.current().map(v=>v.sequence),['9007199254740994','9007199254740995']);assert.equal(journal.cursor,'next')
 const before=journal.current();assert.throws(()=>journal.batch(batch([{...event('9007199254740995'),legacy_data_json:'changed'}],'bad')));assert.equal(journal.cursor,'next');assert.deepEqual(journal.current(),before);assert.throws(()=>journal.batch({...batch(),'reset_required':true}));assert.equal(journal.cursor,'next')
 const state={draft:'Unsaved text'};journal.snapshot({...snapshot([event('9223372036854775807')]),window:{after_sequence:'9007199254740992',through_sequence:'9223372036854775807',version:2}});assert.equal(state.draft,'Unsaved text');assert.equal(journal.cursor,'head')
})
test('reconnect delay is bounded and affects reads only',()=>{for(const attempt of[-100,0,1,5,10000]){const value=eventReconnectDelay(attempt,()=>1);assert.ok(value>=500&&value<=10250)}})
test('wire incompatibility after a POST retains its ambiguity key for explicit resubmission',async t=>{
 const calls=[];let version='0.10.0';t.mock.method(globalThis,'fetch',async(url,options)=>{calls.push({url,options});return Response.json({data:{id:'result'}},{headers:{'X-Platform-API-Version':version}})});const api=new ApiClient({baseUrl:'/api/v1',organizationId:org,token:'test'});await assert.rejects(api.createTask({goal:'Exact'}),e=>e.code==='incompatible_client');assert.equal(calls.length,1);version='0.14.0';await api.createTask({goal:'Exact'});assert.equal(calls.length,2);assert.equal(calls[0].options.headers['Idempotency-Key'],calls[1].options.headers['Idempotency-Key']);assert.equal(calls[1].options.headers['X-Platform-API-Version'],'0.14.0')
})
test('fetch stream uses bearer headers or same-origin cookie without query credentials, CSRF mutation or automatic requests',async t=>{
 const calls=[];let cancelled=0;t.mock.method(globalThis,'fetch',async(url,options)=>{calls.push({url,options});return new Response(new ReadableStream({start(c){c.enqueue(new TextEncoder().encode(wire(batch([event()]))))},cancel(){cancelled++}}),{headers})});
 for(const mode of ['bearer','session']){const api=new ApiClient({baseUrl:'/api/v1',organizationId:org,token:mode==='bearer'?'private-test':'',mode,csrfToken:'csrf-private'});for await(const frame of api.streamTaskEvents(task,'previous')){assert.equal(frame.batch.events[0].sequence,event().sequence);break}}
 assert.equal(calls.length,2);assert.equal(cancelled,2);for(const call of calls){assert.equal(call.options.method,undefined);assert.equal(call.options.headers['Idempotency-Key'],undefined);assert.equal(call.options.headers['X-CSRF-Token'],undefined);assert.equal(call.options.headers['X-Platform-API-Version'],'0.14.0');assert.ok(!call.url.includes('private'));assert.equal(call.options.redirect,'error');assert.equal(call.options.cache,'no-store')};assert.equal(calls[0].options.headers.Authorization,'Bearer private-test');assert.equal(calls[0].options.credentials,'omit');assert.equal(calls[1].options.headers.Authorization,undefined);assert.equal(calls[1].options.credentials,'same-origin')
})
test('stream wrong version or truncated frame fails without hidden reconnect or mutation',async t=>{
 let calls=0,version='0.10.0';t.mock.method(globalThis,'fetch',async()=>{calls++;return new Response(wire(batch([event()])).slice(0,-1),{headers:{...headers,'X-Platform-API-Version':version}})});const api=new ApiClient({baseUrl:'/api/v1',organizationId:org,token:'test'});await assert.rejects(api.streamTaskEvents(task).next(),e=>e.code==='incompatible_client');assert.equal(calls,1);version='0.14.0';await assert.rejects(api.streamTaskEvents(task).next(),e=>e.code==='invalid_event_stream');assert.equal(calls,2)
})

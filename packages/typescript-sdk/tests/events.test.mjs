// SPDX-License-Identifier: Apache-2.0
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createServer} from 'node:http';
import {once} from 'node:events';
import {Client,ProtocolError,APIError,TransportError} from '../dist/index.js';
import {EventStreamParser,validateEvent,validateEventBatch,validateEventSnapshot,readEventFrames,parseEventJSON} from '../dist/events.js';
const corpus=JSON.parse(await readFile(new URL('../../../contracts/event-conformance.json',import.meta.url),'utf8'));
const org='org_contract',task='task_contract',now='2026-10-24T12:00:00Z';
const legacy={id:'legacy',organization_id:org,task_id:task,sequence:'9007199254740993',type:'old.unknown',payload_version:0,legacy_data_json:'{ "wide":9007199254740993, "note":"я" }',created_at:now};
const batch=(events=[legacy],extra={})=>({organization_id:org,task_id:task,events,cursor:'opaque_cursor',has_more:false,reset_required:false,window:{after_sequence:'0',through_sequence:'9223372036854775807',version:1},...extra});
const frame=value=>`event: platform.events\r\nid: ${value.cursor}\r\ndata: ${JSON.stringify({frame_version:1,kind:'batch',batch:value})}\r\n\r\n`;

test('shared public corpus: every kind, variant, wide legacy value and negative case',()=>{
 assert.equal(corpus.contract_version,'0.14.0');for(const entry of corpus.cases){if(entry.valid)assert.deepEqual(validateEvent(entry.event,org,task),entry.event,entry.name);else assert.throws(()=>validateEvent(entry.event,org,task),ProtocolError,entry.name)}
});
test('arbitrary byte boundaries CR/LF/CRLF multiline data and heartbeats preserve exact wide values',()=>{
 const value=batch();const text=': heartbeat\r\n\r\n'+frame(value).replace('data: {','data: {\r\ndata: ');const bytes=new TextEncoder().encode(text);const parser=new EventStreamParser(org,task);const received=[];for(const byte of bytes)received.push(...parser.push(Uint8Array.of(byte)));parser.finish();assert.equal(received.length,1);assert.deepEqual(received[0].batch,value);assert.equal(received[0].batch.events[0].legacy_data_json,legacy.legacy_data_json);
 for(const delimiter of ['\n','\r','\r\n']){const parser=new EventStreamParser(org,task);assert.equal(parser.push(new TextEncoder().encode(frame(value).replaceAll('\r\n',delimiter))).length,1);parser.finish()}
});
test('partial, oversized, invalid UTF8, unknown/duplicate fields and mismatched ids never yield cursors',()=>{
 for(const text of [frame(batch()).slice(0,-2),frame(batch()).replace('id: opaque_cursor','id: different'),frame(batch()).replace('id: opaque_cursor','id: opaque_cursor\nid: opaque_cursor'),frame(batch()).replace('data:','retry: 1\ndata:'),'event: platform.events\ndata: '+ ' '.repeat(65536)]){const parser=new EventStreamParser(org,task);assert.throws(()=>{parser.push(new TextEncoder().encode(text));parser.finish()},ProtocolError)}
 const parser=new EventStreamParser(org,task);assert.throws(()=>parser.push(Uint8Array.of(255,10)),ProtocolError);
 for(const json of ['{"kind":"batch","kind":"error"}','{"a":9007199254740993}','{"a":9007199254740991.1}','{"a":1e99999}','['.repeat(40)+'0'+']'.repeat(40)])assert.throws(()=>parseEventJSON(json),ProtocolError);
});
test('reset and error frames have no id and never disguise unavailable history as an empty success',()=>{
 const reset=batch([],{cursor:'',reset_required:true,reset_reason:'history_expired'});const parser=new EventStreamParser(org,task);const text=`event: platform.reset\ndata: ${JSON.stringify({frame_version:1,kind:'reset_required',batch:reset})}\n\n`;assert.equal(parser.push(new TextEncoder().encode(text))[0].kind,'reset_required');parser.finish();assert.throws(()=>new EventStreamParser(org,task).push(new TextEncoder().encode(text.replace('data:','id: forbidden\ndata:'))),ProtocolError);
 const bad=[batch([],{cursor:''}),batch([],{reset_required:true}),batch([legacy,{...legacy,id:'another'}]),batch([{...legacy,organization_id:'foreign'}])];for(const value of bad)assert.throws(()=>validateEventBatch(value,org,task),ProtocolError);
 const snapshot={organization_id:org,task_id:task,task:{id:task,organization_id:org,reserved_microusd:'0',spent_microusd:'0'},events:[legacy],cursor:'head',replay_cursor:'floor',window:batch().window,has_earlier_events:false,observed_at:now};assert.deepEqual(validateEventSnapshot(snapshot,org,task),snapshot);assert.throws(()=>validateEventSnapshot({...snapshot,task:{id:task,organization_id:'foreign'}},org,task),ProtocolError);
});
test('response reader cancels on early iterator return, abort, idle timeout and wrong version',async()=>{
 let cancelled=0;const response=()=>new Response(new ReadableStream({start(c){c.enqueue(new TextEncoder().encode(frame(batch())))},cancel(){cancelled++}}),{headers:{'content-type':'text/event-stream','X-Platform-API-Version':'0.14.0'}});
 const iterator=readEventFrames(response(),org,task);assert.equal((await iterator.next()).value.kind,'batch');await iterator.return();assert.equal(cancelled,1);
 const controller=new AbortController(),aborted=readEventFrames(response(),org,task,{signal:controller.signal});await aborted.next();controller.abort();await assert.rejects(aborted.next(),TransportError);assert.equal(cancelled,2);
 const idle=readEventFrames(response(),org,task,{idleTimeoutMs:5});await idle.next();await assert.rejects(idle.next(),TransportError);assert.equal(cancelled,3);
 const invalid=new Response('',{headers:{'content-type':'text/event-stream'}});await assert.rejects(readEventFrames(invalid,org,task).next(),ProtocolError);
});
test('real HTTP single connection, scoped auth, exact cursor, explicit replay and 401 have no hidden retries',async()=>{
 const requests=[];let mode='stream';const server=createServer((req,res)=>{requests.push({url:req.url,headers:req.headers});res.setHeader('X-Platform-API-Version',mode==='version'?'0.10.0':'0.14.0');if(mode==='denied'){res.writeHead(401,{'content-type':'application/json'});res.end(JSON.stringify({error:{code:'unauthenticated',message:'Revoked',retryable:false}}));return}if(mode==='version'){res.writeHead(200,{'content-type':'application/json'});res.end('{"data":[]}');return}res.writeHead(200,{'content-type':'text/event-stream'});res.write(': ready\n\n');res.end(frame(batch()))});server.listen(0,'127.0.0.1');await once(server,'listening');const sdk=new Client({baseUrl:`http://127.0.0.1:${server.address().port}`,token:'synthetic-private',organizationId:org});
 try{for(let count=0;count<2;count++){let received=0;for await(const item of sdk.streamTaskEvents(task,{cursor:'previous_cursor'})){received++;assert.equal(item.batch.events[0].sequence,legacy.sequence)}assert.equal(received,1);assert.equal(requests.length,count+1)}const request=requests[0];assert.equal(request.headers.authorization,'Bearer synthetic-private');assert.equal(request.headers['x-platform-api-version'],'0.14.0');assert.equal(request.headers['idempotency-key'],undefined);assert.equal(request.url,`/api/v1/tasks/${task}/event-stream?cursor=previous_cursor`);assert.ok(!request.url.includes('synthetic-private'));mode='denied';await assert.rejects(sdk.streamTaskEvents(task).next(),e=>e instanceof APIError&&e.statusCode===401);assert.equal(requests.length,3);mode='version';await assert.rejects(sdk.listTasks(),ProtocolError);assert.equal(requests.length,4)}finally{server.closeAllConnections();server.close();await once(server,'close')}
});


test('window revision2 remains readable after history expiry; empty legacy and non-RFC3339 timestamps fail',()=>{
 const window={after_sequence:'9007199254740992',through_sequence:'9223372036854775807',version:2};
 const reset=batch([],{cursor:'',reset_required:true,reset_reason:'history_expired',window});assert.equal(validateEventBatch(reset,org,task).window.version,2);
 const value={organization_id:org,task_id:task,task:{id:task,organization_id:org,reserved_microusd:'0',spent_microusd:'0'},events:[legacy],cursor:'newhead',replay_cursor:'newfloor',window,has_earlier_events:true,observed_at:now};assert.equal(validateEventSnapshot(value,org,task).window.version,2);assert.throws(()=>validateEventSnapshot({...value,task:{...value.task,reserved_microusd:1}},org,task),ProtocolError);
 for(const type of ['__proto__','constructor','toString'])assert.throws(()=>validateEvent({...corpus.cases[0].event,type},org,task),ProtocolError);
 for(const patch of[{legacy_data_json:''},{created_at:'2026-10-24 12:00:00+00:00'},{created_at:'2026-02-30T12:00:00Z'}])assert.throws(()=>validateEvent({...legacy,...patch},org,task),ProtocolError);
 for(const version of[0,1.5,2147483648,true])assert.throws(()=>validateEventBatch(batch([],{window:{...window,version}}),org,task),ProtocolError);
});

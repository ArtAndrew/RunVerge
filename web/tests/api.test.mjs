import {Response} from './wireFixture.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ApiClient, ApiError, normalizeBaseUrl } from '../src/api.ts'
import { money, parseBudget } from '../src/format.ts'

test('USD input preserves micro-USD precision and rejects unsafe amounts', () => {
  assert.equal(parseBudget('0.000001'), 1)
  assert.equal(parseBudget('1,234567'), 1_234_567)
  assert.equal(parseBudget('1000000'), 1_000_000_000_000)
  assert.match(money(1000), /0,001\s*\$/)
  assert.match(money(1), /0,000001\s*\$/)
  for (const value of ['0', '-1', '1e6', '0.0000001', 'NaN', 'Infinity', '1000000.000001', '9007199254.740991']) {
    assert.throws(() => parseBudget(value), Error)
  }
})

test('financial facts format every micro-USD exactly beyond Number precision', () => {
  const normalize = value => money(value).replace(/\s/g, ' ')
  assert.equal(normalize('9223372036854775807'), '9 223 372 036 854,775807 $')
  assert.equal(normalize('-9223372036854775808'), '-9 223 372 036 854,775808 $')
  assert.equal(normalize('9007199254740993'), '9 007 199 254,740993 $')
  assert.equal(normalize('9007199254750000'), '9 007 199 254,75 $')
  assert.equal(normalize('-1'), '-0,000001 $')
  assert.equal(normalize('0'), '0,00 $')
  assert.equal(normalize('1000000'), '1,00 $')
  for (const value of ['-0', '+1', '01', '1.0', '1e6', ' 1', '1\n', '١', '', '9223372036854775808', '-9223372036854775809', null, NaN, Infinity, Number.MAX_SAFE_INTEGER, 1.5]) {
    assert.equal(money(value), 'Нет данных')
  }
})

test('connection URL rejects credential-bearing and insecure remote targets', () => {
  assert.equal(normalizeBaseUrl('/api/v1/'), '/api/v1')
  assert.equal(normalizeBaseUrl('http://127.0.0.1:8080/api/v1'), 'http://127.0.0.1:8080/api/v1')
  assert.equal(normalizeBaseUrl('https://agents.example/api/v1/'), 'https://agents.example/api/v1')
  for (const value of ['http://agents.example/api/v1', 'https://user:password@agents.example/api/v1', '//evil.example', 'javascript:alert(1)', '/api/v1?token=abc', '/\\evil.example']) {
    assert.throws(() => normalizeBaseUrl(value), Error)
  }
})

test('ambiguous mutation retry keeps its idempotency key; successful new action gets a new one', async t => {
  const calls = []
  let fail = true
  t.mock.method(globalThis, 'fetch', async (url, options) => {
    calls.push({ url, options })
    if (fail) { fail = false; throw new TypeError('network disconnected') }
    return Response.json({ data: { id: 'task-created' } })
  })
  const api = new ApiClient({ baseUrl: '/api/v1', organizationId: 'org-a', token: 'test-only-token' })
  const payload = { goal: 'Run fixture', agent_id: 'agent-a', budget_microusd: 1000 }
  await assert.rejects(api.createTask(payload), ApiError)
  await api.createTask(payload)
  await api.createTask(payload)
  assert.equal(calls[0].options.headers['Idempotency-Key'], calls[1].options.headers['Idempotency-Key'])
  assert.notEqual(calls[1].options.headers['Idempotency-Key'], calls[2].options.headers['Idempotency-Key'])
  assert.equal(calls[0].options.headers.Authorization, 'Bearer test-only-token')
  assert.equal(calls[0].options.headers['X-Organization-ID'], 'org-a')
  assert.equal(calls[0].options.redirect, 'error')
  assert.equal(calls[0].options.credentials, 'omit')
  assert.equal(calls[0].url, '/api/v1/tasks')
  assert.ok(!calls[0].url.includes('test-only-token'))
})

test('server error preserves request id and never becomes an empty successful list', async t => {
  t.mock.method(globalThis, 'fetch', async () => Response.json({ error: { code: 'forbidden', message: 'Access denied', request_id: 'request-a', retryable: false } }, { status: 403 }))
  const api = new ApiClient({ baseUrl: '/api/v1', organizationId: 'org-a', token: 'test-only-token' })
  await assert.rejects(api.tasks(), error => error instanceof ApiError && error.code === 'forbidden' && error.requestId === 'request-a')
})

test('invalid JSON envelope and non-list response produce explicit errors', async t => {
  const fetchMock = t.mock.method(globalThis, 'fetch', async () => Response.json(null))
  const api = new ApiClient({ baseUrl: '/api/v1', organizationId: 'org-a', token: 'test-only-token' })
  await assert.rejects(api.tasks(), error => error instanceof ApiError && error.code === 'invalid_response')
  fetchMock.mock.mockImplementation(async () => Response.json({ data: {} }))
  await assert.rejects(api.tasks(), error => error instanceof ApiError && error.code === 'invalid_response')
})

test('review and retry preserve exact snapshots, CSRF and idempotency after ambiguity', async t => {
  const calls=[];let first=true
  t.mock.method(globalThis,'fetch',async(url,options)=>{calls.push({url,options});if(first){first=false;throw new TypeError('lost response')}return Response.json({data:{id:'task',version:9}})})
  const api=new ApiClient({baseUrl:'/api/v1',organizationId:'org',token:'',mode:'session',csrfToken:'csrf-test'})
  const payload={expected_version:8,candidate_id:'candidate',artifact_id:'artifact',artifact_version:2,content_digest:'a'.repeat(64),decision:'request_changes',reason:'Revise'}
  await assert.rejects(api.reviewTask('task',payload),ApiError)
  await api.reviewTask('task',payload)
  assert.equal(calls[0].options.headers['Idempotency-Key'],calls[1].options.headers['Idempotency-Key'])
  assert.equal(calls[1].options.headers['X-CSRF-Token'],'csrf-test')
  assert.equal(calls[1].options.headers.Authorization,undefined)
  assert.equal(calls[1].options.credentials,'same-origin')
  assert.deepEqual(JSON.parse(calls[1].options.body),payload)
  assert.equal(calls[1].url,'/api/v1/tasks/task/review')
  await assert.rejects(api.retryTask('task',{expected_version:9,reason:'Revise',acknowledge_new_cost:false}),ApiError)
  await api.retryTask('task',{expected_version:9,reason:'Revise',feedback:'Use evidence',acknowledge_new_cost:true})
  assert.equal(calls[2].url,'/api/v1/tasks/task/retry')
  assert.equal(JSON.parse(calls[2].options.body).acknowledge_new_cost,true)
  assert.notEqual(calls[1].options.headers['Idempotency-Key'],calls[2].options.headers['Idempotency-Key'])
})

test('review collections preserve opaque cursors and read-only semantics', async t => {
 const calls=[];t.mock.method(globalThis,'fetch',async(url,options)=>{calls.push({url,options});return Response.json({data:[],next_cursor:'next+/='})})
 const api=new ApiClient({baseUrl:'/api/v1',organizationId:'org',token:'test-only'})
 for(const method of ['candidates','decisions','children']){const result=await api[method]('task','cursor+/=');assert.equal(result.next_cursor,'next+/=');const call=calls.shift();const url=new URL(call.url,'http://localhost');assert.equal(url.pathname,`/api/v1/tasks/task/${method}`);assert.equal(url.searchParams.get('cursor'),'cursor+/=');assert.equal(call.options.headers['Idempotency-Key'],undefined)}
})

test('materials preserve exact draft/merge/publication wire data and separate idempotency keys',async()=>{
 const calls=[];const original=globalThis.fetch
 globalThis.fetch=async(url,init)=>{calls.push({url,init});return new Response(JSON.stringify({data:{id:'result'}}),{status:200})}
 try{
  const api=new ApiClient({baseUrl:'http://127.0.0.1:8080/api/v1',organizationId:'org',token:'test'})
  await api.createMaterial('task',{name:'notes',media_type:'text/plain',content:''})
  await api.reviseMaterial('task','material',{expected_version:2,merge_base_version:1,content:'  exact\r\n',reason:'Manual merge'})
  await api.publishMaterial('task','material',{expected_task_version:8,expected_material_version:3,content_digest:'a'.repeat(64),parent_candidate_id:'original'})
  assert.deepEqual(calls.map(v=>new URL(v.url).pathname),['/api/v1/tasks/task/materials','/api/v1/tasks/task/materials/material/revisions','/api/v1/tasks/task/materials/material/publish'])
  assert.equal(JSON.parse(calls[0].init.body).content,'');assert.equal(JSON.parse(calls[1].init.body).content,'  exact\r\n');assert.equal(JSON.parse(calls[1].init.body).merge_base_version,1)
  assert.equal(new Set(calls.map(v=>v.init.headers['Idempotency-Key'])).size,3)
  assert.equal(JSON.parse(calls[2].init.body).acknowledge_new_cost,undefined)
 }finally{globalThis.fetch=original}
})
test('material and exact artifact reads retain independent cursors and revision numbers',async()=>{
 const urls=[],original=globalThis.fetch;globalThis.fetch=async url=>{urls.push(url);return new Response(JSON.stringify({data:String(url).includes('limit=')?[]:{id:'read'},next_cursor:'next+/='}),{status:200})}
 try{const api=new ApiClient({baseUrl:'http://127.0.0.1:8080/api/v1',organizationId:'org',token:'test'});await api.materials('task','cursor+/=');await api.materialRevisions('task','material','revision+/=');await api.materialVersion('task','material',3);await api.artifacts('task',undefined,'artifact+/=');await api.artifact('task','exact',1);assert.equal(new URL(urls[0]).searchParams.get('cursor'),'cursor+/=');assert.equal(new URL(urls[1]).searchParams.get('cursor'),'revision+/=');assert.equal(new URL(urls[2]).pathname,'/api/v1/tasks/task/materials/material/revisions/3');assert.equal(new URL(urls[3]).searchParams.get('cursor'),'artifact+/=');assert.equal(new URL(urls[4]).searchParams.get('version'),'1')}finally{globalThis.fetch=original}
})

test('current identity is scoped and refuses an identity from another organization',async()=>{const original=globalThis.fetch;const calls=[];let identity={principal_id:'principal',organization_id:'org',role:'member',permissions:['task:read']};globalThis.fetch=async(url,options)=>{calls.push({url,options});return new Response(JSON.stringify({data:identity}),{headers:{'content-type':'application/json'}})};try{const api=new ApiClient({baseUrl:'/api/v1',organizationId:'org',token:'test'});assert.deepEqual(await api.currentIdentity(),identity);assert.equal(calls[0].url,'/api/v1/me');assert.equal(calls[0].options.headers['Idempotency-Key'],undefined);identity={...identity,organization_id:'foreign'};await assert.rejects(api.currentIdentity(),e=>e.code==='invalid_response');identity={...identity,organization_id:'org',principal_id:''};await assert.rejects(api.currentIdentity(),e=>e.code==='invalid_response')}finally{globalThis.fetch=original}})

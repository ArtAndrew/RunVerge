// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {Response} from './wireFixture.mjs'
import {ApiClient} from '../src/api.ts'
import {validateCapabilities,capabilityContext} from '../src/capabilities.ts'
const fixture=JSON.parse(readFileSync(new URL('./capabilities-fixture.json',import.meta.url)))
const copy=()=>structuredClone(fixture)
const client=()=>new ApiClient({baseUrl:'/api/v1',organizationId:'org',token:'synthetic-capability-only'})

test('normal catalogue preserves six personas, explicit unsupported provider and non-authorizing observations',()=>{
 const f=copy(),result=validateCapabilities(f,'org',f.identity)
 assert.equal(result,f);assert.equal(result.onboarding.length,6);assert.equal(result.roles.length,6)
 assert.equal(result.onboarding.find(p=>p.id==='provider').status,'not_implemented')
 assert.equal(result.profiles.find(p=>p.id==='provider_lifecycle').status,'not_implemented')
 assert.equal(result.operations[0].permission_alternatives.length,0)
 assert.equal(capabilityContext(f.identity),capabilityContext({...f.identity,permissions:[...f.identity.permissions].reverse()}))
})

test('bounded wire validation rejects malformed enums, arrays, refs, duplicate bindings and identity mismatches',()=>{
 const mutations=[
 f=>f.read_only=false,f=>f.catalog_version='2',f=>f.api_contract='0.12.0',f=>f.assessment='authorization_permit',
 f=>f.identity.organization_id='foreign',f=>f.identity.principal_id='other',f=>f.identity.role='owner',f=>f.identity.permissions.push('configuration.manage'),
 f=>f.identity.principal_kind='robot',f=>f.identity.principal_kind='service',f=>f.observed_at='yesterday',f=>f.observed_at='2026-02-31T12:00:00Z',
 f=>f.operations[0].path='/api/v1/capabilities?execute=true',f=>f.operations[0].method='get',f=>f.operations[0].runtime_guards=[],
 f=>f.operations[0].response_kind='unknown',f=>f.operations[0].success_status=500,f=>f.operations[0].bindings[5]=f.operations[0].bindings[0],
 f=>f.operations[0].bindings.find(b=>b.status==='not_exposed').name='invented',f=>f.operations[0].permission_alternatives=[[]],
 f=>f.operations.push({...f.operations[0]}),f=>f.operations=Array(129).fill(f.operations[0]),f=>f.operations[0].runtime_guards=['x'.repeat(4097)],
 f=>f.onboarding[0].steps[0].operation_ids=['invented'],f=>f.onboarding[0].steps[0].commands=[[]],f=>f.onboarding[0].steps[0].commands=[['node','-e','run()']],f=>f.remediation_command=['sh','-c','run'],f=>f.onboarding.pop(),
 f=>f.profiles[0].documentation=['https://external.example'],f=>f.profiles[0].documentation=['docs/../secret'],f=>f.profiles[0].summary='bad\0text',
 f=>f.profiles[0].summary='\ud800',f=>f.roles[5].role='owner',f=>f.roles[5]=f.roles[0],f=>f.extension={credential_reference:'must-not-appear'},
 f=>{let v={};f.extension=v;for(let i=0;i<22;i++)v.next=v={}},f=>f.extension='x'.repeat(4*1024*1024),
 ]
 for(const mutate of mutations){const f=copy();mutate(f);assert.throws(()=>validateCapabilities(f,'org',fixture.identity),Error)}
})

test('commands and markup are inert bounded text, never evaluated during validation',()=>{
 const f=copy(),text='<img src=x onerror="globalThis.executed=true">'
 f.onboarding[0].steps[0].instruction=text;f.onboarding[0].steps[0].commands=[['platform','doctor','<script>globalThis.executed=true</script>']]
 delete globalThis.executed
 assert.equal(validateCapabilities(f,'org').onboarding[0].steps[0].instruction,text);assert.equal(globalThis.executed,undefined)
})

test('explicit catalogue read uses exact queryless read-only auth and fresh identity; session has no bearer or CSRF',async t=>{
 const calls=[];t.mock.method(globalThis,'fetch',async(url,options)=>{calls.push({url,options});return Response.json({data:url.endsWith('/me')?fixture.identity:copy()})})
 const api=client();await api.capabilities();assert.deepEqual(calls.map(c=>c.url),['/api/v1/me','/api/v1/capabilities'])
 for(const {options}of calls){assert.equal(options.method,undefined);assert.equal(options.body,undefined);assert.equal(options.headers['Idempotency-Key'],undefined);assert.equal(options.headers['X-CSRF-Token'],undefined);assert.equal(options.headers['X-Platform-API-Version'],'0.14.0');assert.equal(options.headers['X-Organization-ID'],'org');assert.equal(options.headers.Authorization,'Bearer synthetic-capability-only');assert.equal(options.cache,'no-store');assert.equal(options.redirect,'error')}
 calls.length=0;await new ApiClient({baseUrl:'/api/v1',organizationId:'org',token:'',mode:'session',csrfToken:'unused'}).capabilities()
 for(const {options}of calls){assert.equal(options.headers.Authorization,undefined);assert.equal(options.headers['X-CSRF-Token'],undefined);assert.equal(options.credentials,'same-origin')}
})

test('malformed or foreign observation never becomes success and has no automatic retry',async t=>{
 let f=copy();const calls=[];t.mock.method(globalThis,'fetch',async(url)=>{calls.push(url);return Response.json({data:url.endsWith('/me')?fixture.identity:f})})
 for(const change of [f=>f.identity.organization_id='other',f=>f.identity.principal_id='other',f=>f.identity.permissions=[],f=>f.read_only=false]){
  f=copy();change(f);const before=calls.length;await assert.rejects(client().capabilities(),e=>e.code==='invalid_response');assert.equal(calls.length,before+2)
 }
 f=copy();assert.deepEqual(await client().capabilities(),f)
})

test('known principal change stops before catalogue and ignored abort cannot publish delayed data',async t=>{
 let me=fixture.identity,resolveCatalog;const calls=[]
 t.mock.method(globalThis,'fetch',async(url)=>{calls.push(url);if(url.endsWith('/me'))return Response.json({data:me});return await new Promise(resolve=>{resolveCatalog=()=>resolve(Response.json({data:copy()}))})})
 const api=client();await api.currentIdentity();me={...fixture.identity,principal_id:'new-principal'}
 for(let i=0;i<2;i++)await assert.rejects(api.capabilities(),e=>e.code==='unauthenticated');assert.equal(calls.filter(p=>p.endsWith('/capabilities')).length,0)
 me=fixture.identity;const controller=new AbortController(),pending=client().capabilities(controller.signal)
 while(!resolveCatalog)await new Promise(resolve=>setImmediate(resolve))
 controller.abort();resolveCatalog();await assert.rejects(pending,e=>e.name==='AbortError')
 assert.equal(calls.filter(p=>p.endsWith('/capabilities')).length,1)
})

// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { Client, APIError, ProtocolError, TransportError } from "../dist/index.js";
import { capabilityFacts } from "../dist/capabilities.js";

const fixture = JSON.parse(await readFile(new URL("../../../tests/sdk/capabilities-fixtures.json", import.meta.url), "utf8"));
function changed(c) {
  const value = structuredClone(fixture.normal);
  for (const change of [c, ...(c.extra ?? [])]) {
    let parent = value;
    for (const key of change.path.slice(0, -1)) parent = parent[key];
    parent[change.path.at(-1)] = "value" in change ? structuredClone(change.value) : Array.from({length:change.repeat},()=>structuredClone(fixture.normal.operations[0]));
  }
  return value;
}
const client = options => new Client({baseUrl:"http://127.0.0.1:1", token:"synthetic-private", organizationId:"org", ...options});
const reply = data => new Response(JSON.stringify({data}), {headers:{"X-Platform-API-Version":"0.14.0","Content-Type":"application/json"}});

test("capabilities uses current authentication, no query/body/idempotency, and does not execute command data", async () => {
  const original = globalThis.fetch, calls=[];
  globalThis.fetch = async (url, init) => { calls.push({url,init}); return reply(fixture.normal); };
  try {
    const signal = new AbortController().signal;
    assert.deepEqual(await client().capabilities({signal}), fixture.normal);
    assert.equal(calls.length,1);
    const {url,init}=calls[0];
    assert.equal(String(url),"http://127.0.0.1:1/api/v1/capabilities");
    assert.equal(init.method,"GET"); assert.equal(init.body,undefined);
    assert.equal(init.headers.Authorization,"Bearer synthetic-private");
    assert.equal(init.headers["X-Organization-ID"],"org");
    assert.equal(init.headers["X-Platform-API-Version"],"0.14.0");
    assert.equal(init.headers["Idempotency-Key"],undefined);
    assert.equal(init.redirect,"error"); assert.equal(init.credentials,"omit"); assert.equal(init.cache,"no-store");
  } finally { globalThis.fetch=original; }
});

test("shared malformed catalogue corpus rejects authority, scope, version, enum and unsafe templates; display instructions stay inert", () => {
  for (const c of fixture.cases) {
    const value=changed(c);
    if(c.valid) assert.equal(capabilityFacts(value,"org"),value,c.name);
    else assert.throws(()=>capabilityFacts(value,"org"),e=>e instanceof ProtocolError&&e.message==="Invalid product capabilities response",c.name);
  }
  for(const field of Object.keys(fixture.normal)) {
    const value=structuredClone(fixture.normal); delete value[field];
    assert.throws(()=>capabilityFacts(value,"org"),ProtocolError,`missing ${field}`);
  }
  for(const value of [null,[],{}, {...fixture.normal,profiles:[null]}, {...fixture.normal,observed_at:"\ud800"}, {...fixture.normal,future:{nested:Array(50001).fill(0)}}]) assert.throws(()=>capabilityFacts(value,"org"),ProtocolError);
});

test("known fixed-token principal remains bound; callback credentials may refresh the active principal", async () => {
  const original=globalThis.fetch, foreign=structuredClone(fixture.normal); foreign.identity.principal_id="another";
  let count=0;
  globalThis.fetch=async()=>reply(++count===1?fixture.normal.identity:foreign);
  try {
    const sdk=client(); await sdk.getCurrentIdentity(); await assert.rejects(sdk.capabilities(),ProtocolError); assert.equal(count,2);
    count=0;const refreshing=client({token:()=>"rotating-synthetic"}); await refreshing.getCurrentIdentity();
    assert.equal((await refreshing.capabilities()).identity.principal_id,"another");
  } finally { globalThis.fetch=original; }
});

test("128 operations with full guard lists fit the bounded catalogue", () => {
  const value=structuredClone(fixture.normal);
  value.operations=Array.from({length:128},(_,index)=>({...structuredClone(fixture.normal.operations[0]), id:index===0?"get_capabilities":`operation_${index}`,path:`/api/v1/route_${index}`,runtime_guards:Array(32).fill("Current authorization and resource checks")}));
  assert.equal(capabilityFacts(value,"org"),value);
});

test("generated full catalogue accepts all six roles and names real SDK bindings", async () => {
  const actual=JSON.parse(await readFile(new URL("../../../tests/contracts/fixtures/product-capabilities.json",import.meta.url),"utf8"));
  assert.equal(capabilityFacts(actual,"org_catalog_fixture","catalog_fixture"),actual);
  const catalog=JSON.parse(await readFile(new URL("../../../contracts/capability-catalog.json",import.meta.url),"utf8"));
  for(const role of catalog.roles) {
    const value={...catalog,catalog_version:"1",assessment:"support_and_role_observation",read_only:true,observed_at:"2026-10-26T10:00:00Z",identity:{...role,organization_id:"org",principal_id:"actor"}};
    assert.equal(capabilityFacts(value,"org"),value,`${role.principal_kind}:${role.role}`);
  }
  for(const op of catalog.operations) {
    const binding=op.bindings.find(b=>b.surface==="typescript");
    if(binding.status==="available") assert.equal(typeof Client.prototype[binding.name],"function",op.id);
    else assert.equal(binding.name,"",op.id);
  }
});

test("capability errors and aborts use existing bounded transport and never retry", async () => {
  const original=globalThis.fetch;let calls=0;
  globalThis.fetch=async()=>{calls++;return new Response(JSON.stringify({error:{code:"forbidden",message:"Denied",retryable:false}}),{status:403,headers:{"X-Platform-API-Version":"0.14.0","Content-Type":"application/json"}})};
  try {
    await assert.rejects(client().capabilities(),e=>e instanceof APIError&&e.statusCode===403);assert.equal(calls,1);
    const controller=new AbortController();controller.abort();
    await assert.rejects(client().capabilities({signal:controller.signal}),TransportError); assert.equal(calls,1);
  } finally { globalThis.fetch=original; }
});

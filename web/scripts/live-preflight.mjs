// SPDX-License-Identifier: Elastic-2.0
// Shared by opt-in live helpers. This check grants no permission to mutate.
import assert from 'node:assert/strict'
import {API_VERSION,API_VERSION_HEADER} from '../../packages/typescript-sdk/dist/index.js'
export {API_VERSION,API_VERSION_HEADER}

export async function livePreflight(origin,args=process.argv.slice(2)) {
  assert.ok(args.length===0||(args.length===1&&args[0]==='--preflight-only'),'Only --preflight-only is supported')
  const url=new URL(origin)
  assert.equal(url.protocol,'http:');assert.ok(['127.0.0.1','[::1]'].includes(url.hostname))
  assert.equal(url.pathname,'/');assert.equal(url.username,'');assert.equal(url.password,'');assert.equal(url.search,'');assert.equal(url.hash,'')
  const response=await fetch(new URL('/healthz',url),{redirect:'error',signal:AbortSignal.timeout(10000)})
  assert.equal(response.status,200,'Live helper requires a healthy local API')
  const health=await response.json()
  assert.equal(health?.data?.contract_version,API_VERSION,`Require API ${API_VERSION}; no live fixture was created`)
  if(args.length) {
    console.log(JSON.stringify({contract_version:API_VERSION,mutations:0,scope:'health version only; credentials, browser, peers and permissions not checked'}))
    return true
  }
  return false
}

// SPDX-License-Identifier: Elastic-2.0
// Read-only reopen of the current live cleanup scenario; no replay of mutations.
import assert from 'node:assert/strict'
import {API_VERSION,livePreflight} from './live-preflight.mjs'
import {createHash} from 'node:crypto'
import fs from 'node:fs/promises'
import path from 'node:path'
import {fileURLToPath} from 'node:url'
import {createRequire} from 'node:module'
import {Client} from '../../packages/typescript-sdk/dist/index.js'
const web=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..'),repo=path.dirname(web),output=path.resolve(process.env.AGENT_PLATFORM_QA_OUTPUT??path.join(repo,'.local/qa/automation')),origin=process.env.AGENT_PLATFORM_URL??'http://127.0.0.1:8080'
const url=new URL(origin);assert.equal(url.protocol,'http:');assert.ok(['127.0.0.1','[::1]'].includes(url.hostname));assert.equal(url.username,'');assert.equal(url.password,'');assert.equal(url.pathname,'/');assert.equal(url.search,'');assert.equal(url.hash,'')
if(await livePreflight(origin)) process.exit(0)
const {chromium}=createRequire(import.meta.url)(process.env.WORKSPACE_PLAYWRIGHT_PATH||'playwright')
const original=JSON.parse(await fs.readFile(process.env.AGENT_PLATFORM_AUTOMATION_REPORT??path.join(output,'automation-live-report.json'),'utf8')),credentials=JSON.parse(await fs.readFile(process.env.AGENT_PLATFORM_CREDENTIALS??path.join(repo,'.local/bootstrap.json'),'utf8')),client=new Client({baseUrl:origin,token:credentials.token,organizationId:credentials.organization_id}),task=await client.getTask(original.task_id),before=await client.automation(task.id),posts=[],errors=[]
assert.equal(before.grant.id,original.grant_id);assert.equal(before.grant.status,'revoked')
let browser
try{
 browser=await chromium.launch({headless:true,...(process.env.WORKSPACE_BROWSER_PATH?{executablePath:process.env.WORKSPACE_BROWSER_PATH}:{})});const page=await browser.newPage({viewport:{width:1440,height:1050}});page.on('pageerror',e=>errors.push(e.message));page.on('request',req=>{if(req.method()==='POST')posts.push(new URL(req.url()).pathname)})
 await page.goto(origin);await page.getByRole('button',{name:'Подключить пространство',exact:true}).click();await page.getByLabel('Организация',{exact:true}).fill(credentials.organization_id);await page.getByLabel('Токен доступа',{exact:true}).fill(credentials.token);await page.getByRole('button',{name:'Подключить пространство',exact:true}).click();await page.getByRole('button',{name:task.goal,exact:true}).click();await page.getByRole('tab',{name:'Автоматизация',exact:true}).click();await page.locator('.automation-controls').getByText('Отозвано',{exact:true}).waitFor();await page.evaluate(()=>{document.activeElement?.blur();window.scrollTo(0,0)});await page.screenshot({path:path.join(output,'automation-reopened-desktop.png'),fullPage:true});await page.setViewportSize({width:390,height:844});await page.waitForFunction(()=>document.querySelector('.sidebar').getBoundingClientRect().right<=0);assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await page.screenshot({path:path.join(output,'automation-reopened-mobile.png'),fullPage:true})
 assert.deepEqual(posts,[]);assert.deepEqual(errors,[]);const after=await client.automation(task.id);assert.equal(after.grant.version,before.grant.version);assert.deepEqual(after.usage,before.usage)
 const view=await client.getToolExecution(task.id,original.execution_id);assert.equal(view.execution.status,'cancelled');assert.equal(view.execution.reason,'automation_grant_inactive');assert.equal(view.approval.status,'expired');const cost=await client.costs(task.id);assert.equal(cost.reserved_microusd,original.reserved_microusd);assert.equal(cost.spent_microusd,original.spent_microusd);assert.equal(view.receipt,undefined)
 const files=(await fs.readdir(path.join(web,'dist/assets'))).filter(n=>/\.(js|css)$/.test(n)),build={};for(const name of files)build['web/dist/assets/'+name]=createHash('sha256').update(await fs.readFile(path.join(web,'dist/assets',name))).digest('hex')
 const report={contract:API_VERSION,result:'pass',scope:'read-only actual browser/API reopen of current cleanup flow; exact revoked grant, cancelled unsent action and released reservation',task_id:task.id,grant_id:before.grant.id,grant_status:after.grant.status,grant_version:after.grant.version,usage:after.usage,business_mutations:posts.length,browser_errors:errors.length,execution_status:view.execution.status,reserved_microusd:cost.reserved_microusd,build_hashes:build,screenshots:['automation-reopened-desktop.png','automation-reopened-mobile.png']}
 await fs.writeFile(path.join(output,'automation-reopen-report.json'),JSON.stringify(report,null,2)+'\n');console.log(JSON.stringify(report))
}finally{await browser?.close()}

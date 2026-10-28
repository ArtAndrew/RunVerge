// SPDX-License-Identifier: Elastic-2.0
// Compiled UI against a controlled streaming HTTP fixture; no real API/PG/provider claim.
import assert from 'node:assert/strict'
import {createServer} from 'node:http'
import {once} from 'node:events'
import {createHash,randomUUID} from 'node:crypto'
import fs from 'node:fs/promises'
import path from 'node:path'
import {fileURLToPath} from 'node:url'
import {createRequire} from 'node:module'
const {chromium}=createRequire(import.meta.url)(process.env.WORKSPACE_PLAYWRIGHT_PATH||'playwright')
const web=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..'),output=path.resolve(process.env.AGENT_PLATFORM_QA_OUTPUT??path.join(web,'../.local/qa/events-mock',randomUUID()))
await fs.mkdir(output,{recursive:true});await assert.rejects(fs.access(path.join(output,'events-mock-report-0.11.json')))
const now=new Date().toISOString(),org='org-events',principal='human-events',token='synthetic-browser-only'
const task={id:'task-events',organization_id:org,owner_id:principal,agent_id:'agent',agent_version:1,goal:'Проверка живого журнала и сохранения текста',status:'waiting_input',waiting_reason:'model_candidate_pending',budget_microusd:10000,reserved_microusd:'150',spent_microusd:'0',version:8,created_at:now,updated_at:now,root_id:'task-events',depth:0,financial_hold:false,current_run_id:'run',attempt_count:1,max_runs:3,execution_deadline:new Date(Date.now()+86400000).toISOString()}
const agent={id:'agent',organization_id:org,name:'Тестовый исполнитель',instructions:'Controlled UI fixture',model:'fixture:echo',tools:[],budget_microusd:10000,version:1,published:true,created_at:now}
const event=sequence=>({id:'event-'+sequence,organization_id:org,task_id:task.id,sequence,type:'historic.opaque',payload_version:0,legacy_data_json:'{ "number":9007199254740993 }',created_at:now})
const base=event('9007199254740993'),one=event('9007199254740994'),two=event('9007199254740995')
let revision=1,holdSnapshot=false,revoked=false,snapshots=0,stage='boot';const streamRequests=[],mutations=[],streams=new Set(),timers=new Set(),browserErrors=[]
const window=()=>({after_sequence:revision===1?'0':'9007199254740994',through_sequence:'9007199254740995',version:revision})
const batch=(events,cursor)=>({organization_id:org,task_id:task.id,events,cursor,has_more:false,reset_required:false,window:window()})
const encode=value=>`event: platform.events\nid: ${value.cursor}\ndata: ${JSON.stringify({frame_version:1,kind:'batch',batch:value})}\n\n`
const server=createServer(async(req,res)=>{
 const url=new URL(req.url,'http://localhost'),p=url.pathname
 const json=(data,status=200,error)=>{res.writeHead(status,{'Content-Type':'application/json','X-Platform-API-Version':'0.13.0','Cache-Control':'no-store'});res.end(JSON.stringify(error?{error}:{data}))}
 try{
  if(p==='/auth/config')return json({oidc_enabled:false})
  if(p.startsWith('/api/v1/')){
   if(req.headers.authorization!==`Bearer ${token}`||req.headers['x-organization-id']!==org)return json(null,401,{code:'unauthenticated',message:'Synthetic fixture auth denied',retryable:false})
   if(req.headers['x-platform-api-version']!=='0.13.0')return json(null,409,{code:'incompatible_client',message:'Wire version required',retryable:false})
   if(revoked)return json(null,401,{code:'unauthenticated',message:'Доступ отозван',retryable:false})
   if(req.method!=='GET'){mutations.push(p);return json(null,409,{code:'invalid_transition',message:'No fixture mutations accepted',retryable:false})}
   if(p.endsWith('/me'))return json({principal_id:principal,organization_id:org,principal_kind:'human',role:'owner',permissions:['tasks.read','tasks.review_any','materials.write']})
   if(p==='/api/v1/tasks')return json([task]);if(p==='/api/v1/agents')return json([agent]);if(p===`/api/v1/tasks/${task.id}`)return json(task)
   if(p.endsWith('/event-snapshot')){snapshots++;if(holdSnapshot)return json(null,503,{code:'unavailable',message:'Снимок временно недоступен',retryable:true});return json({organization_id:org,task_id:task.id,task,events:revision===1?[base]:[two],cursor:revision===1?'snapshot_head':'new_snapshot_head',replay_cursor:'replay_floor',window:{...window(),through_sequence:revision===1?base.sequence:two.sequence},has_earlier_events:revision>1,observed_at:now})}
   if(p.endsWith('/event-stream')){
    streamRequests.push({cursor:url.searchParams.get('cursor'),wire_version:req.headers['x-platform-api-version']});res.writeHead(200,{'Content-Type':'text/event-stream','X-Platform-API-Version':'0.13.0','Cache-Control':'no-store'});res.flushHeaders();streams.add(res)
    const heartbeat=setInterval(()=>{if(!res.destroyed)res.write(': heartbeat\n\n')},1000);req.on('close',()=>{clearInterval(heartbeat);streams.delete(res)})
    if(streamRequests.length===1){res.write(encode(batch([one],'processed_one')));res.write(encode(batch([two],'incomplete_two')).slice(0,-10));const timer=setTimeout(()=>{timers.delete(timer);res.destroy()},150);timers.add(timer)}
    else if(revision===1)res.write(encode(batch([one,two],'processed_two')))
    else res.write(encode(batch([],'new_snapshot_head')))
    return
   }
   return json([])
  }
  const file=path.resolve(web,'dist',p.slice(1)||'index.html');assert.ok(file.startsWith(path.join(web,'dist')+path.sep));res.writeHead(200,{'Content-Type':file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html'});res.end(await fs.readFile(file))
 }catch{if(!res.headersSent)res.writeHead(404);res.end()}
})
server.listen(0,'127.0.0.1');await once(server,'listening');let browser
try{
 browser=await chromium.launch({headless:true,...(process.env.WORKSPACE_BROWSER_PATH?{executablePath:process.env.WORKSPACE_BROWSER_PATH}:{})});const context=await browser.newContext({viewport:{width:1440,height:1050}}),page=await context.newPage();page.on('pageerror',error=>browserErrors.push(error.name))
 await page.goto(`http://127.0.0.1:${server.address().port}`);await page.getByRole('button',{name:'Подключить пространство',exact:true}).click();await page.getByLabel('Организация',{exact:true}).fill(org);await page.getByLabel('Токен доступа',{exact:true}).fill(token);await page.getByRole('button',{name:'Подключить пространство',exact:true}).click();await page.getByRole('button',{name:task.goal,exact:true}).click();await page.getByRole('heading',{name:task.goal,exact:true}).waitFor();stage='reconnect'
 await page.getByRole('tab',{name:/Ход работы/}).click();await page.getByText('Данные события · #9007199254740995',{exact:true}).waitFor({timeout:15000});assert.equal(streamRequests[0].cursor,'snapshot_head');assert.equal(streamRequests[1].cursor,'processed_one');assert.equal(await page.locator('.timeline li').count(),3);assert.equal(await page.getByText('Данные события · #9007199254740994',{exact:true}).count(),1)
 stage='draft';await page.getByRole('tab',{name:/Материалы/}).click();await page.getByRole('button',{name:'Новый материал',exact:true}).click();let dialog=page.getByRole('dialog');const draft='Несохранённый текст · 9007199254740993\n  Точные пробелы остаются.';await dialog.getByLabel('Название материала',{exact:true}).fill('Локальный черновик');await dialog.getByLabel('Текст материала',{exact:true}).fill(draft);await dialog.getByLabel('Причина изменения материала',{exact:true}).fill('Причина не должна потеряться');assert.equal(mutations.length,0)
 stage='reset';revision=2;holdSnapshot=true;for(const stream of streams){stream.end(`event: platform.reset\ndata: ${JSON.stringify({frame_version:1,kind:'reset_required',batch:{...batch([],''),reset_required:true,reset_reason:'history_expired'}})}\n\n`)}
 await dialog.getByRole('button',{name:'Обновить состояние',exact:true}).waitFor();assert.equal(await dialog.getByLabel('Текст материала',{exact:true}).inputValue(),draft);assert.equal(await dialog.getByLabel('Текст материала',{exact:true}).evaluate(e=>Boolean(e.closest('[inert]'))),true);assert.equal(snapshots,1,'reset must not silently request a replacement snapshot')
 await dialog.getByRole('button',{name:'Обновить состояние',exact:true}).click();await page.getByText('Снимок временно недоступен',{exact:true}).waitFor();assert.equal(snapshots,2);assert.equal(await dialog.getByLabel('Текст материала',{exact:true}).evaluate(e=>Boolean(e.closest('[inert]'))),true);assert.equal(await dialog.getByLabel('Причина изменения материала',{exact:true}).inputValue(),'Причина не должна потеряться');assert.equal(mutations.length,0)
 await page.screenshot({path:path.join(output,'events-reset-0.11-desktop.png'),fullPage:true});await page.setViewportSize({width:390,height:844});await page.waitForFunction(()=>document.querySelector('.sidebar').getBoundingClientRect().right<=0);assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await dialog.getByRole('button',{name:'Обновить состояние',exact:true}).scrollIntoViewIfNeeded();await page.screenshot({path:path.join(output,'events-reset-0.11-mobile.png'),fullPage:true});await page.setViewportSize({width:1440,height:1050})
 holdSnapshot=false;await dialog.getByRole('button',{name:'Обновить состояние',exact:true}).click();await page.waitForFunction(()=>!document.querySelector('[aria-label="Текст материала"]').closest('[inert]'));assert.equal(await dialog.getByLabel('Текст материала',{exact:true}).inputValue(),draft);assert.equal(await dialog.getByLabel('Причина изменения материала',{exact:true}).inputValue(),'Причина не должна потеряться');assert.equal(streamRequests.at(-1).cursor,'new_snapshot_head');assert.equal(mutations.length,0)
 stage='invalid-kind';const beforeInvalid=streamRequests.length;const invalid={...two,id:'invalid-kind',sequence:'9007199254740996',type:'__proto__',payload_version:1,data:{}};delete invalid.legacy_data_json;for(const stream of streams)stream.write(encode(batch([invalid],'must_not_adopt')));await dialog.getByRole('button',{name:'Обновить состояние',exact:true}).waitFor();assert.equal(await dialog.getByLabel('Текст материала',{exact:true}).evaluate(e=>Boolean(e.closest('[inert]'))),true);await new Promise(resolve=>setTimeout(resolve,800));assert.equal(streamRequests.length,beforeInvalid,'malformed known protocol must stop, not reconnect as network error');await dialog.getByRole('button',{name:'Обновить состояние',exact:true}).click();await page.waitForFunction(()=>!document.querySelector('[aria-label="Текст материала"]').closest('[inert]'));assert.equal(await dialog.getByLabel('Текст материала',{exact:true}).inputValue(),draft);
 stage='revocation';revoked=true;for(const stream of streams)stream.end(`event: platform.error\ndata: ${JSON.stringify({frame_version:1,kind:'error',error:{code:'unauthenticated',message:'Доступ отозван',retryable:false}})}\n\n`);await page.getByRole('heading',{name:task.goal,exact:true}).waitFor({state:'detached'});assert.equal(await page.getByRole('dialog').count(),0);assert.equal(mutations.length,0);assert.deepEqual(browserErrors,[])
 const report={contract_version:'0.13.0',passed:true,scope:'compiled UI with controlled local streaming HTTP fixture; no actual Go API/PG/provider',checks:['wire header','exact wide sequence','midframe network disconnect resumes last complete cursor','duplicate dedupe','explicit reset only','policy window revision2','failed snapshot keeps modal inert','draft and reason survive successful reset','mobile390 no overflow','unknown prototype kind stops without reconnect','revocation clears scoped data','zero POST'],stream_cursors:streamRequests.map(v=>v.cursor),snapshots,mutations:0,browser_errors:0,script_sha256:createHash('sha256').update(await fs.readFile(fileURLToPath(import.meta.url))).digest('hex')};await fs.writeFile(path.join(output,'events-mock-report-0.11.json'),JSON.stringify(report,null,2)+'\n');console.log(JSON.stringify(report))
}catch(error){await fs.writeFile(path.join(output,'events-mock-failure.json'),JSON.stringify({passed:false,stage,error_type:error.name,stream_count:streamRequests.length,snapshots,mutations:mutations.length,script_sha256:createHash('sha256').update(await fs.readFile(fileURLToPath(import.meta.url))).digest('hex')},null,2)+'\n');throw error}
finally{for(const timer of timers)clearTimeout(timer);for(const stream of streams)stream.destroy();await browser?.close();server.closeAllConnections();server.close();await once(server,'close')}

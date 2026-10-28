// Real built UI -> local API -> PostgreSQL -> worker. No business-response interception.
// Participants use fixture:echo; no external model or billing claim.
import fs from 'node:fs/promises'
import path from 'node:path'
import {createRequire} from 'node:module'
import {createHash,randomUUID} from 'node:crypto'
import assert from 'node:assert/strict'
import {isIP} from 'node:net'

const require=createRequire(import.meta.url)
const {chromium}=require(process.env.WORKSPACE_PLAYWRIGHT_PATH||'playwright')
const bootstrap=JSON.parse(await fs.readFile(process.env.WORKSPACE_BOOTSTRAP||'../.local/bootstrap.json','utf8'))
const baseUrl=(process.env.WORKSPACE_URL||'http://127.0.0.1:8080').replace(/\/$/,'')
const origin=new URL(baseUrl),hostname=origin.hostname.replace(/^\[|\]$/g,'')
assert.ok(origin.protocol==='http:'&&isIP(hostname)&&((hostname==='::1')||hostname.startsWith('127.'))&&!origin.username&&!origin.password&&origin.pathname==='/'&&!origin.search&&!origin.hash,'Only literal loopback HTTP origin is permitted')
const output=path.resolve('.qa');await fs.mkdir(output,{recursive:true});let browser
const key='ui-review-'+randomUUID(),observations=[]
async function api(method,route,body){
 const response=await fetch(baseUrl+'/api/v1'+route,{method,headers:{Authorization:`Bearer ${bootstrap.token}`,'X-Platform-API-Version':'0.13.0','X-Organization-ID':bootstrap.organization_id,...(body?{'Content-Type':'application/json','Idempotency-Key':key+':'+randomUUID()}: {})},...(body?{body:JSON.stringify(body)}:{}),redirect:'error',signal:AbortSignal.timeout(10000)})
 assert.ok(response.ok,`Fixture setup/read failed: ${method} ${route} status ${response.status}`)
 return (await response.json()).data
}
async function publishAgent(body){const draft=await api('POST','/agents',body);return api('POST',`/agents/${draft.id}/publish`,{expected_version:draft.version})}
try{
 const health=await fetch(baseUrl+'/healthz',{redirect:'error',signal:AbortSignal.timeout(10000)});assert.equal((await health.json()).data.contract_version,'0.6.0','Require main API0.6 before mutations')
 const leaf=await publishAgent({name:'UI live fixture '+Date.now(),instructions:'Repeat supplied execution input for an explicit synthetic fixture.',model:'fixture:echo',tools:[],budget_microusd:10000})
 const team=await publishAgent({name:'UI live review team '+Date.now(),instructions:'Aggregate synthetic fixture evidence and retain limitations.',model:'team:coordinator',tools:[],budget_microusd:10000,team:{pattern:'parallel',members:[{id:'first',agent_id:leaf.id,agent_version:leaf.version,task:'Prepare first synthetic material'},{id:'second',agent_id:leaf.id,agent_version:leaf.version,task:'Prepare second synthetic material'}],join_policy:'all',max_depth:1,max_children:2,max_parallel:2}})
 browser=await chromium.launch({headless:true,...(process.env.WORKSPACE_BROWSER_PATH?{executablePath:process.env.WORKSPACE_BROWSER_PATH}:{})})
 const page=await browser.newPage({viewport:{width:1440,height:1050}}),errors=[]
 page.on('pageerror',e=>errors.push(e.message))
 page.on('response',response=>{const url=new URL(response.url());if(response.request().method()==='POST'&&url.pathname.startsWith('/api/v1/tasks'))observations.push({path:url.pathname,status:response.status()})})
 await page.goto(baseUrl)
 await page.getByRole('button',{name:'Подключить пространство',exact:true}).click()
 await page.getByLabel('Организация',{exact:true}).fill(bootstrap.organization_id)
 await page.getByLabel('Токен доступа',{exact:true}).fill(bootstrap.token)
 await page.getByRole('button',{name:'Подключить пространство',exact:true}).click()
 await page.getByText('API подключён',{exact:true}).waitFor()
 await page.getByRole('button',{name:'Новая задача',exact:true}).click()
 const goal='Проверка полного пути решения: два синтетических материала и явная доработка.'
 await page.getByLabel('Что нужно сделать?',{exact:true}).fill(goal)
 await page.getByLabel('Исполнитель',{exact:true}).selectOption(team.id)
 await page.getByLabel('Бюджет задачи, USD',{exact:true}).fill('0.01')
 await page.getByLabel('Предел попыток',{exact:true}).fill('2')
 const createdPromise=page.waitForResponse(r=>r.url()===baseUrl+'/api/v1/tasks'&&r.request().method()==='POST')
 await page.getByRole('button',{name:'Создать задачу',exact:true}).click()
 const createdResponse=await createdPromise;assert.equal(createdResponse.status(),201);const created=(await createdResponse.json()).data,taskId=created.id
 await page.getByRole('heading',{name:goal,exact:true}).waitFor()
 await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent==='Принять результат'&&!b.disabled),{},{timeout:60000})
 let task=await api('GET',`/tasks/${taskId}`)
 assert.equal(task.status,'waiting_input');assert.equal(task.attempt_count,1)
 const firstRun=task.current_run_id,firstCandidate=task.candidate_id
 const firstReceipt=(await api('GET',`/tasks/${taskId}/candidates`)).find(v=>v.id===firstCandidate)
 assert.equal(firstReceipt.source,'team')
 await page.locator('.review-candidate .team-result').getByText('Открыть весь исходный артефакт',{exact:false}).click()
 const shown=await page.locator('.review-candidate [data-team-exact="artifact"]').textContent()
 assert.equal(createHash('sha256').update(shown).digest('hex'),firstReceipt.content_digest)
 await page.locator('.review-candidate .team-result').getByText('Открыть весь исходный артефакт',{exact:false}).click()
 const before=await api('GET',`/tasks/${taskId}/costs`);assert.equal(before.spent_microusd,'2000');assert.equal(before.reserved_microusd,'0')
 const reviewResponse=page.waitForResponse(r=>r.url()===`${baseUrl}/api/v1/tasks/${taskId}/review`&&r.request().method()==='POST')
 await page.getByRole('button',{name:'Запросить доработку',exact:true}).click()
 await page.getByRole('dialog').getByLabel('Причина решения',{exact:true}).fill('Укажите ограничения синтетических материалов.')
 await page.getByRole('dialog').getByRole('button',{name:'Запросить доработку',exact:true}).click()
 assert.equal((await reviewResponse).status(),200)
 await page.getByRole('heading',{name:'История решений',exact:true}).waitFor()
 task=await api('GET',`/tasks/${taskId}`);assert.equal(task.status,'blocked');assert.equal(task.attempt_count,1);assert.equal(task.current_run_id,firstRun)
 assert.equal(observations.filter(v=>v.path.endsWith('/retry')).length,0)
 assert.equal((await api('GET',`/tasks/${taskId}/costs`)).spent_microusd,'2000')
 await page.getByRole('button',{name:'Подготовить новую попытку',exact:true}).click()
 let dialog=page.getByRole('dialog'),launch=dialog.getByRole('button',{name:'Запустить новую попытку',exact:true})
 assert.equal(await launch.isDisabled(),true)
 await dialog.getByLabel('Причина новой попытки',{exact:true}).fill('Проверка явной доработки результата')
 const feedback='Отметьте, что материалы синтетические и не подтверждают фактическую точность.'
 await dialog.getByLabel('Указания исполнителю',{exact:true}).fill(feedback)
 await dialog.getByRole('checkbox').check()
 const retryResponse=page.waitForResponse(r=>r.url()===`${baseUrl}/api/v1/tasks/${taskId}/retry`&&r.request().method()==='POST')
 await launch.click();assert.equal((await retryResponse).status(),200)
 await page.waitForFunction(()=>document.querySelector('.review-candidate .muted')?.textContent?.includes('попытка 2')&&[...document.querySelectorAll('button')].some(b=>b.textContent==='Принять результат'&&!b.disabled),{},{timeout:60000})
 task=await api('GET',`/tasks/${taskId}`);assert.equal(task.attempt_count,2);assert.notEqual(task.current_run_id,firstRun);assert.notEqual(task.candidate_id,firstCandidate)
 const run=(await api('GET',`/tasks/${taskId}/runs`)).find(v=>v.id===task.current_run_id)
 assert.equal(run.feedback,feedback);assert.equal(run.source_candidate_id,firstCandidate)
 await page.getByRole('button',{name:'Принять результат',exact:true}).click()
 const acceptResponse=page.waitForResponse(r=>r.url()===`${baseUrl}/api/v1/tasks/${taskId}/review`&&r.request().method()==='POST')
 await page.getByRole('dialog').getByLabel('Причина решения',{exact:true}).fill('Принимаю результат локальной проверки интерфейса.')
 await page.getByRole('dialog').getByRole('button',{name:'Принять результат',exact:true}).click()
 assert.equal((await acceptResponse).status(),200)
 await page.getByText('Принятый материал',{exact:true}).waitFor()
 await page.locator('.decision-history').getByText('Принято',{exact:true}).waitFor()
 await page.locator('.decision-history').getByText('Запрошена доработка',{exact:true}).waitFor()
 task=await api('GET',`/tasks/${taskId}`);const costs=await api('GET',`/tasks/${taskId}/costs`),decisions=await api('GET',`/tasks/${taskId}/decisions`),children=await api('GET',`/tasks/${taskId}/children`)
 assert.equal(task.status,'succeeded');assert.ok(task.accepted_decision_id);assert.equal(costs.spent_microusd,'4000');assert.equal(costs.reserved_microusd,'0');assert.ok(costs.entries.every(v=>v.synthetic));assert.equal(decisions.length,2);assert.equal(children.length,4)
 assert.ok(decisions.every(v=>v.method==='authorized_user_decision'));assert.deepEqual(new Set(decisions.map(v=>v.decision)),new Set(['request_changes','accept']))
 assert.match(await page.locator('.budget-panel .cost-breakdown > div').filter({hasText:'Потрачено'}).locator('dd').innerText(),/0,004\s*\$/)
 await page.screenshot({path:path.join(output,'review-live-0.6-desktop.png'),fullPage:true})
 await page.setViewportSize({width:390,height:844});await page.waitForFunction(()=>document.querySelector('.sidebar').getBoundingClientRect().right<=0)
 assert.deepEqual(await page.evaluate(()=>[...document.querySelectorAll('main,.detail-heading,.budget-panel,.review-candidate')].filter(e=>e.getBoundingClientRect().right>innerWidth).map(e=>e.className)),[])
 await page.screenshot({path:path.join(output,'review-live-0.6-mobile.png'),fullPage:true})
 const persisted=await page.evaluate(()=>JSON.stringify({local:{...localStorage},session:{...sessionStorage}}));assert.ok(!persisted.includes(bootstrap.token));assert.deepEqual(errors,[])
 assert.equal(observations.filter(v=>v.path.endsWith('/review')).length,2);assert.equal(observations.filter(v=>v.path.endsWith('/retry')).length,1)
 const report={contract_version:'0.6.0',result:'pass',executed_at:new Date().toISOString(),scope:'Actual built UI -> local API -> PostgreSQL -> fixture worker/coordinator; no business responses intercepted',provider:'fixture:echo participants',real_model_call:false,synthetic_cost:true,task_id:taskId,status:task.status,attempt_count:task.attempt_count,children:children.length,decisions:decisions.length,initial_spent_microusd:before.spent_microusd,spent_microusd:costs.spent_microusd,reserved_microusd:costs.reserved_microusd,feedback_pinned:true,explicit_new_cost_ack:true,request_changes_did_not_launch:true,browser_errors:errors.length,token_persisted:false,requests:observations,screenshots:['review-live-0.6-desktop.png','review-live-0.6-mobile.png'],limits:['Synthetic fixtures do not establish model quality or paid billing','Token-mode local UI flow does not prove corporate SSO or human presence']}
 await fs.writeFile(path.join(output,'review-live-report-0.6.json'),JSON.stringify(report,null,2)+'\n');console.log(JSON.stringify(report))
}catch(error){console.error((error?.stack||String(error)).split(bootstrap.token).join('[REDACTED]'));process.exitCode=1}finally{await browser?.close()}

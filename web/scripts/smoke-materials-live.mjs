// SPDX-License-Identifier: Elastic-2.0
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
const key='ui-material-'+randomUUID(),observations=[]
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
 const goal='Проверка редактора: исходные материалы команды и отдельный итоговый текст.'
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
 const originalArtifact=await api('GET',`/tasks/${taskId}/artifacts/${firstReceipt.artifact_id}?version=${firstReceipt.artifact_version}`)
 const before=await api('GET',`/tasks/${taskId}/costs`);assert.equal(before.spent_microusd,'2000');assert.equal(before.reserved_microusd,'0')
 await page.getByRole('button',{name:'Подготовить итоговый текст',exact:true}).click();let dialog=page.getByRole('dialog');await dialog.getByLabel('Текст материала',{exact:true}).waitFor();assert.equal(await dialog.getByLabel('Текст материала',{exact:true}).inputValue(),'')
 const draft='Первоначальный итог локальной проверки. Материалы синтетические.\n';await dialog.getByLabel('Текст материала',{exact:true}).fill(draft)
 const createResponse=page.waitForResponse(r=>r.url()===`${baseUrl}/api/v1/tasks/${taskId}/materials`&&r.request().method()==='POST');await dialog.getByRole('button',{name:'Сохранить черновик',exact:true}).click();const createdMaterialResponse=await createResponse;assert.equal(createdMaterialResponse.status(),201);let material=(await createdMaterialResponse.json()).data;await dialog.getByText('result.md · прочитана версия 1',{exact:true}).waitFor();assert.equal((await api('GET',`/tasks/${taskId}`)).candidate_id,firstCandidate)
 const other='Правка второго клиента: не считать синтетический текст доказательством.\n';material=await api('POST',`/tasks/${taskId}/materials/${material.id}/revisions`,{expected_version:1,content:other,reason:'Concurrent fixture editor'})
 const local='Локальная правка, которую нельзя потерять при конфликте.\n';await dialog.getByLabel('Текст материала',{exact:true}).fill(local);await dialog.getByLabel('Причина изменения материала',{exact:true}).fill('Уточнение итогового вывода')
 const conflictResponse=page.waitForResponse(r=>r.url()===`${baseUrl}/api/v1/tasks/${taskId}/materials/${material.id}/revisions`&&r.request().method()==='POST');await dialog.getByRole('button',{name:'Сохранить новую версию',exact:true}).click();assert.equal((await conflictResponse).status(),409);await dialog.getByText('Материал изменён другим запросом. Ваш текст сохранён в редакторе. Автоматического объединения нет.',{exact:true}).waitFor();assert.equal(await dialog.getByLabel('Текст материала',{exact:true}).inputValue(),local)
 await dialog.getByRole('button',{name:'Загрузить актуальную версию для сравнения',exact:true}).click();await dialog.locator('[data-material-latest]').waitFor();assert.equal(await dialog.locator('[data-material-latest]').textContent(),other);assert.equal(await dialog.getByLabel('Текст материала',{exact:true}).inputValue(),local)
 const merged=other+'Итог редактора: интерфейс сохраняет исходные версии и требует отдельного решения.\n';await dialog.getByLabel('Текст материала',{exact:true}).fill(merged)
 const mergeResponse=page.waitForResponse(r=>r.url()===`${baseUrl}/api/v1/tasks/${taskId}/materials/${material.id}/revisions`&&r.request().method()==='POST');await dialog.getByRole('button',{name:'Сохранить объединённую версию поверх v2',exact:true}).click();assert.equal((await mergeResponse).status(),201);await dialog.getByText('result.md · прочитана версия 3',{exact:true}).waitFor()
 const versions=await api('GET',`/tasks/${taskId}/materials/${material.id}/revisions?limit=100`);assert.equal(versions.length,3);assert.equal(versions.find(v=>v.version===3).merge_base_version,1);assert.equal((await api('GET',`/tasks/${taskId}/materials/${material.id}/revisions/1`)).content,draft);assert.equal((await api('GET',`/tasks/${taskId}/materials/${material.id}/revisions/2`)).content,other)
 await dialog.getByRole('button',{name:'Подготовить публикацию',exact:true}).click();await dialog.locator('[data-material-publication]').waitFor();assert.equal(await dialog.locator('[data-material-publication]').textContent(),merged)
 const publicationResponse=page.waitForResponse(r=>r.url()===`${baseUrl}/api/v1/tasks/${taskId}/materials/${material.id}/publish`&&r.request().method()==='POST');await dialog.getByRole('button',{name:'Предложить как результат',exact:true}).click();assert.equal((await publicationResponse).status(),200);await page.getByRole('button',{name:'Создать редактируемую копию',exact:true}).waitFor();assert.equal(await page.locator('.review-candidate>.prose-output').textContent(),merged)
 task=await api('GET',`/tasks/${taskId}`);const receipt=(await api('GET',`/tasks/${taskId}/candidates`)).find(v=>v.id===task.candidate_id);assert.equal(receipt.source,'material');assert.equal(receipt.parent_candidate_id,firstCandidate);assert.equal(receipt.review_basis_candidate_id,firstCandidate);assert.equal(receipt.material_version,3);assert.equal(task.current_run_id,firstRun);assert.equal(task.attempt_count,1);assert.equal(task.status,'waiting_input');assert.equal(receipt.content_digest,createHash('sha256').update(merged).digest('hex'))
 const exact=await api('GET',`/tasks/${taskId}/artifacts/${receipt.artifact_id}?version=${receipt.artifact_version}`);assert.equal(exact.content,merged);assert.equal((await api('GET',`/tasks/${taskId}/artifacts/${firstReceipt.artifact_id}?version=${firstReceipt.artifact_version}`)).content,originalArtifact.content)
 await page.getByText('Исходная основа результата · сохранённые материалы',{exact:true}).click();await page.locator('.material-basis').getByRole('heading',{name:'Материалы всей команды',exact:true}).waitFor();assert.equal(await page.locator('.material-basis .team-result-member').count(),2)
 await page.getByRole('button',{name:'Принять результат',exact:true}).click();dialog=page.getByRole('dialog');const reviewResponse=page.waitForResponse(r=>r.url()===`${baseUrl}/api/v1/tasks/${taskId}/review`&&r.request().method()==='POST');await dialog.getByRole('button',{name:'Принять результат',exact:true}).click();assert.equal((await reviewResponse).status(),200);await page.getByText('Принятый материал',{exact:true}).waitFor()
 task=await api('GET',`/tasks/${taskId}`);const costs=await api('GET',`/tasks/${taskId}/costs`),decisions=await api('GET',`/tasks/${taskId}/decisions`);assert.equal(task.status,'succeeded');assert.equal(task.result,merged);assert.equal(task.current_run_id,firstRun);assert.equal(task.attempt_count,1);assert.equal(costs.spent_microusd,before.spent_microusd);assert.equal(costs.reserved_microusd,before.reserved_microusd);assert.equal(decisions.length,1);assert.equal(decisions[0].candidate_id,receipt.id);assert.equal(decisions[0].content_digest,receipt.content_digest)
 await page.evaluate(()=>{document.activeElement?.blur();window.scrollTo(0,0)});await page.screenshot({path:path.join(output,'materials-live-0.6-desktop.png'),fullPage:true});await page.setViewportSize({width:390,height:844});await page.waitForFunction(()=>document.querySelector('.sidebar').getBoundingClientRect().right<=0);assert.deepEqual(await page.evaluate(()=>[...document.querySelectorAll('main,.detail-heading,.budget-panel,.review-candidate')].filter(e=>e.getBoundingClientRect().right>innerWidth).map(e=>e.className)),[]);await page.screenshot({path:path.join(output,'materials-live-0.6-mobile.png'),fullPage:true})
 const persisted=await page.evaluate(()=>JSON.stringify({local:{...localStorage},session:{...sessionStorage}}));assert.ok(!persisted.includes(bootstrap.token));assert.deepEqual(errors,[]);assert.equal(observations.filter(v=>v.path.endsWith('/retry')).length,0);assert.equal(observations.filter(v=>v.path.endsWith('/publish')).length,1)
 const report={contract_version:'0.6.0',result:'pass',executed_at:new Date().toISOString(),scope:'Actual built UI -> local API -> PostgreSQL -> fixture worker/coordinator; no business responses intercepted',provider:'fixture:echo participants',real_model_call:false,synthetic_cost:true,task_id:taskId,material_id:material.id,published_version:3,status:task.status,attempt_count:task.attempt_count,decisions:decisions.length,initial_spent_microusd:before.spent_microusd,spent_microusd:costs.spent_microusd,reserved_microusd:costs.reserved_microusd,run_preserved:true,old_revision_content_preserved:true,original_team_basis_preserved:true,stale_edit:'409',local_buffer_preserved:true,manual_merge_base:1,browser_errors:errors.length,token_persisted:false,requests:observations,screenshots:['materials-live-0.6-desktop.png','materials-live-0.6-mobile.png'],limits:['Synthetic fixtures do not establish model quality or paid billing','Token-mode local UI flow does not prove corporate SSO or human presence']};await fs.writeFile(path.join(output,'materials-live-report-0.6.json'),JSON.stringify(report,null,2)+'\n');console.log(JSON.stringify(report))
}catch(error){console.error((error?.stack||String(error)).split(bootstrap.token).join('[REDACTED]'));process.exitCode=1}finally{await browser?.close()}

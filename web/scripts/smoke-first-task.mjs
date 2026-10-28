// SPDX-License-Identifier: Elastic-2.0
// Built UI with intercepted, synthetic API responses. No real server/model/key.
import assert from 'node:assert/strict'
import {createServer} from 'node:http'
import {once} from 'node:events'
import fs from 'node:fs/promises'
import path from 'node:path'
import {fileURLToPath} from 'node:url'
import {createRequire} from 'node:module'
const {chromium}=createRequire(import.meta.url)(process.env.WORKSPACE_PLAYWRIGHT_PATH||'playwright')
const web=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..')
const output=path.resolve(process.env.AGENT_PLATFORM_QA_OUTPUT||path.join(web,'.qa/first-task'))
await fs.mkdir(output,{recursive:true})
const server=createServer(async(req,res)=>{try{const relative=new URL(req.url,'http://localhost').pathname.slice(1)||'index.html',file=path.resolve(web,'dist',relative);assert.ok(file.startsWith(path.join(web,'dist')+path.sep));const data=await fs.readFile(file);res.writeHead(200,{'content-type':file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html'});res.end(data)}catch{res.writeHead(404).end()}})
server.listen(0,'127.0.0.1');await once(server,'listening')
const agents=[],posts=[],errors=[],now=new Date().toISOString()
const identity={principal_id:'ui-owner',principal_kind:'human',organization_id:'ui-org',role:'owner',permissions:JSON.parse(await fs.readFile(path.join(web,'../contracts/permissions.json'),'utf8')).roles.owner}
let browser
try {
 browser=await chromium.launch({headless:true,...(process.env.WORKSPACE_BROWSER_PATH?{executablePath:process.env.WORKSPACE_BROWSER_PATH}:{})})
 const context=await browser.newContext({locale:'ru-RU',viewport:{width:1365,height:1000}})
 const page=await context.newPage();page.setDefaultTimeout(10000);page.on('pageerror',error=>errors.push(error.message))
 await page.route('**/auth/config',route=>route.fulfill({headers:{'X-Platform-API-Version':'0.14.0'},json:{data:{oidc_enabled:false}}}))
 await page.route('**/api/v1/**',async route=>{
  const req=route.request(),p=new URL(req.url()).pathname,send=data=>route.fulfill({headers:{'X-Platform-API-Version':'0.14.0'},json:{data}})
  assert.equal(req.headers().authorization,'Bearer synthetic-ui-token');assert.equal(req.headers()['x-organization-id'],'ui-org')
  if(req.method()==='POST'){
   posts.push(p);const body=req.postDataJSON();assert.ok(req.headers()['idempotency-key'])
   if(p==='/api/v1/agents'){assert.equal(body.model,'fixture:echo');assert.deepEqual(body.tools,[]);agents.push({...body,id:'ui-agent',organization_id:'ui-org',version:1,published:false,created_at:now});return send(agents[0])}
   if(p==='/api/v1/agents/ui-agent/publish'){assert.equal(body.expected_version,1);agents[0]={...agents[0],published:true,version:2};return send(agents[0])}
   throw new Error('Unexpected mutation '+p)
  }
  if(p==='/api/v1/me')return send(identity)
  if(p==='/api/v1/agents')return send(agents)
  if(['/api/v1/tasks','/api/v1/models','/api/v1/tools'].includes(p))return send([])
  throw new Error('Unexpected read '+p)
 })
 await page.goto(`http://127.0.0.1:${server.address().port}`)
 await page.getByRole('button',{name:'Подключить пространство',exact:true}).click()
 await page.getByLabel('Организация',{exact:true}).fill('ui-org');await page.getByLabel('Токен доступа',{exact:true}).fill('synthetic-ui-token')
 await page.getByRole('button',{name:'Подключить пространство',exact:true}).click()
 await page.getByRole('heading',{name:'От подключения к первому результату'}).waitFor()
 await page.screenshot({path:path.join(output,'first-task-ru-desktop.png'),fullPage:true})
 await page.getByLabel('Interface language').selectOption('en')
 await page.getByRole('heading',{name:'From setup to your first result'}).waitFor()
 await page.setViewportSize({width:390,height:844})
 await page.waitForFunction(()=>document.querySelector('.sidebar').getBoundingClientRect().right<=1)
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'mobile overflow')
 await page.screenshot({path:path.join(output,'first-task-en-mobile.png'),fullPage:true})
 await page.getByRole('button',{name:'Set up your first agent'}).click()
 const dialog=page.getByRole('dialog')
 await dialog.getByText('Connect a model to get a useful answer',{exact:true}).waitFor()
 await dialog.getByRole('button',{name:'Use suggested agent instructions'}).click()
 const instructions=await dialog.getByLabel('Instructions',{exact:true}).inputValue()
 assert.ok(instructions.includes('supplied materials'));assert.equal(await dialog.getByLabel('Model',{exact:true}).inputValue(),'')
 assert.equal(posts.length,0)
 for (const code of ['es','pt-BR','de','fr','zh-CN']) {
  await page.getByLabel('Interface language').selectOption(code)
  await page.locator('select[aria-label="Interface language"]:enabled').waitFor()
  assert.equal(await page.locator('html').getAttribute('lang'),code)
  assert.equal(await dialog.locator('textarea').inputValue(),instructions)
  assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'translated mobile overflow: '+code)
 }
 await page.getByLabel('Interface language').selectOption('ru')
 assert.equal(await dialog.getByLabel('Инструкции',{exact:true}).inputValue(),instructions)
 assert.equal(await dialog.getByRole('button',{name:'Использовать заготовку агента'}).count(),0)
 await dialog.getByLabel('Название',{exact:true}).fill('Edited source assistant')
 await dialog.getByLabel('Модель',{exact:true}).selectOption('fixture:echo')
 await dialog.getByRole('button',{name:'Сохранить черновик',exact:true}).click()
 await page.getByRole('heading',{name:'Edited source assistant',exact:true}).waitFor()
 assert.equal(agents[0].published,false);assert.deepEqual(posts,['/api/v1/agents'])
 await page.setViewportSize({width:1365,height:1000})
 await page.getByRole('button',{name:'Задачи',exact:true}).click()
 await page.getByRole('button',{name:'Открыть агентов для публикации'}).click()
 await page.getByRole('button',{name:'Опубликовать',exact:true}).click()
 await page.getByText('Опубликован',{exact:true}).waitFor()
 await page.getByRole('button',{name:'Задачи',exact:true}).click()
 await page.getByRole('button',{name:'Перейти к первой задаче'}).click()
 await dialog.getByRole('button',{name:'Резюме текста',exact:true}).click()
 assert.ok((await dialog.getByLabel('Что нужно сделать?',{exact:true}).inputValue()).includes('Нина'))
 assert.deepEqual(posts,['/api/v1/agents','/api/v1/agents/ui-agent/publish'])
 assert.ok(await dialog.getByText('Этот агент использует тестовую модель',{exact:false}).count())
 await dialog.getByRole('button',{name:'Закрыть',exact:true}).first().click()
 const storage=await page.evaluate(()=>JSON.stringify({local:{...localStorage},session:{...sessionStorage}}))
 assert.ok(!storage.includes(instructions)&&!storage.includes('synthetic-ui-token'))
 assert.deepEqual(errors,[])
 const report={scope:'built UI with synthetic intercepted API only',passed:true,checks:['guided empty workspace','missing-model guidance','starter does not select model or mutate API','all seven languages preserve editable instructions','save navigates to unpublished agent','explicit publication','sample task prepared without execution','390px layout','no credential or draft persistence'],real_model_called:false,posts}
 await fs.writeFile(path.join(output,'report.json'),JSON.stringify(report,null,2)+'\n');console.log(JSON.stringify(report))
} finally {await browser?.close();server.closeAllConnections();server.close();await once(server,'close')}

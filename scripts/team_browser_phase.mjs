// SPDX-License-Identifier: Elastic-2.0
// Real built Workspace and API in the owned pilot container. No interception,
// saved profile, developer credentials, direct inference requests or API writes
// outside the UI. Button clicks below deliberately start the bounded model work.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import {createHash} from 'node:crypto';
import {chromium} from '/opt/browser-tools/node_modules/playwright/index.mjs';

let browser, config, step='read_private_config';
const digest=text=>createHash('sha256').update(text).digest('hex');
const phases=['create','request_changes','retry','accept','copy','restart'];
async function main(){
  const file=process.argv[2],stat=await fs.lstat(file);
  assert.ok(stat.isFile()&&!stat.isSymbolicLink()&&(stat.mode&0o077)===0&&stat.size<65536);
  config=JSON.parse(await fs.readFile(file,'utf8'));
  assert.ok(phases.includes(config.phase));
  const origin=new URL(config.base_url);
  assert.equal(origin.protocol,'http:');assert.equal(origin.hostname,'127.0.0.1');assert.ok(origin.port);
  assert.equal(origin.origin,config.base_url);assert.equal(origin.username,'');assert.equal(origin.password,'');
  assert.notEqual(config.owner.principal_id,config.member.principal_id);
  assert.notEqual(config.owner.token,config.member.token);
  assert.equal(config.owner.organization_id,config.member.organization_id);
  const posts=[],errors=[];
  async function read(identity,route){
    const response=await fetch(config.base_url+route,{headers:{Authorization:`Bearer ${identity.token}`,
      'X-Organization-ID':identity.organization_id,'X-Platform-API-Version':config.api_version},
      redirect:'error',signal:AbortSignal.timeout(10000)});
    assert.equal(response.status,200);return (await response.json()).data;
  }
  step='launch_browser';browser=await chromium.launch({headless:true});
  async function login(identity){
    const context=await browser.newContext({locale:'ru-RU',viewport:{width:1440,height:1050}});
    await context.grantPermissions(['clipboard-read','clipboard-write'],{origin:config.base_url});
    const page=await context.newPage();page.setDefaultTimeout(20000);
    page.on('pageerror',()=>errors.push('page_error'));
    page.on('request',request=>{if(request.method()==='POST'&&new URL(request.url()).pathname.startsWith('/api/v1/'))
      posts.push({path:new URL(request.url()).pathname,body:request.postDataJSON(),has_key:Boolean(request.headers()['idempotency-key'])});});
    await page.goto(config.base_url);
    await page.getByRole('button',{name:'Подключить пространство',exact:true}).click();
    await page.getByLabel('Организация',{exact:true}).fill(identity.organization_id);
    await page.getByLabel('Токен доступа',{exact:true}).fill(identity.token);
    await page.getByRole('button',{name:'Подключить пространство',exact:true}).click();
    await page.getByText('API подключён',{exact:true}).waitFor();
    assert.equal(await page.locator('html').getAttribute('lang'),'ru');
    assert.equal((await read(identity,'/api/v1/me')).principal_id,identity.principal_id);
    return page;
  }
  async function openTask(page){
    await page.getByRole('button',{name:config.task_goal,exact:true}).click();
    await page.getByRole('heading',{name:config.task_goal,exact:true}).waitFor();
  }
  async function inspectTeam(page){
    const section=page.locator('.review-candidate .team-result');
    await section.getByText('Открыть весь исходный артефакт',{exact:false}).click();
    assert.equal(digest(await section.locator('[data-team-exact="artifact"]').textContent()),config.candidate_digest);
    await section.getByText('Открыть весь исходный артефакт',{exact:false}).click();
  }
  step='connect_'+config.phase;
  const page=await login(config.phase==='create'?config.member:config.owner);
  let result=null,other=null;
  const responseForCommand=()=>page.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname===config.route);
  if(config.phase==='create'){
    step='create_team_task';await page.getByRole('button',{name:'Новая задача',exact:true}).click();
    const dialog=page.getByRole('dialog'),payload=config.payload;
    assert.equal(payload.execution_timeout_seconds,3600);
    await dialog.getByLabel('Что нужно сделать?',{exact:true}).fill(payload.goal);
    await dialog.getByLabel('Исполнитель',{exact:true}).selectOption(payload.agent_id);
    const amount=BigInt(payload.budget_microusd);
    await dialog.getByLabel('Бюджет задачи, USD',{exact:true}).fill(`${amount/1000000n}.${(amount%1000000n).toString().padStart(6,'0')}`);
    await dialog.getByLabel('Предел попыток',{exact:true}).fill(String(payload.max_runs));
    await dialog.getByLabel('Срок выполнения, часы',{exact:true}).fill('1');
    assert.equal(posts.length,0);const pending=responseForCommand();
    await dialog.getByRole('button',{name:'Создать задачу',exact:true}).click();
    const response=await pending;assert.equal(response.status(),201);result=(await response.json()).data;
    assert.equal(result.owner_id,config.member.principal_id);
  }else{
    step='open_saved_task';await openTask(page);
    if(config.phase==='copy'){
      step='copy_exact_mcp_result';await page.getByRole('tab',{name:'Действия',exact:true}).click();
      await page.getByRole('button',{name:'Открыть точное действие',exact:true}).click();
      await page.getByRole('button',{name:'Показать точный результат',exact:true}).click();
      assert.equal(await page.locator('.tool-detail .artifact-preview').textContent(),config.artifact_content);
      await page.locator('.tool-detail .copy-text-control').getByRole('button',{name:'Скопировать точный текст',exact:true}).click();
      await page.getByText('Точный текст скопирован.',{exact:true}).waitFor();
      assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),config.artifact_content);
    }else if(config.phase==='restart'){
      step='inspect_both_after_restart';await inspectTeam(page);other=await login(config.member);await openTask(other);await inspectTeam(other);
      await other.locator('.decision-history').filter({hasText:config.owner.principal_id}).waitFor();
      const saved=await read(config.member,config.route);
      assert.equal(saved.accepted_decision_id,config.accepted_decision_id);
    }else{
      if(config.phase==='accept'){
        other=await login(config.member);await openTask(other);await inspectTeam(other);
      }
      if(config.phase!=='retry')await inspectTeam(page);
      step='prepare_'+config.phase;
      await page.getByRole('button',{name:config.phase==='retry'?'Подготовить новую попытку':config.phase==='accept'?'Принять результат':'Запросить доработку',exact:true}).click();
      const dialog=page.getByRole('dialog');
      await dialog.getByLabel(config.phase==='retry'?'Причина новой попытки':'Причина решения',{exact:true}).fill(config.payload.reason);
      const button=dialog.getByRole('button',{name:config.phase==='retry'?'Запустить новую попытку':config.phase==='accept'?'Принять результат':'Запросить доработку',exact:true});
      if(config.phase==='retry'){
        await dialog.getByLabel('Указания исполнителю',{exact:true}).fill(config.payload.feedback);
        assert.equal(await button.isDisabled(),true);assert.equal(posts.length,0);
        await dialog.getByRole('checkbox').check();
      }
      assert.equal(posts.length,0);step='submit_'+config.phase;const pending=responseForCommand();await button.click();
      const response=await pending;assert.equal(response.status(),200);result=(await response.json()).data;
      if(other){
        await other.locator('.decision-history li').filter({hasText:config.candidate_digest}).waitFor();
        assert.equal((await read(config.member,config.route.replace(/\/review$/,''))).accepted_decision_id,result.accepted_decision_id);
      }
    }
  }
  step='verify_exact_browser_mutation';
  if(['copy','restart'].includes(config.phase))assert.equal(posts.length,0);
  else{assert.equal(posts.length,1);assert.equal(posts[0].path,config.route);assert.ok(posts[0].has_key);assert.deepEqual(posts[0].body,config.payload);}
  assert.deepEqual(errors,[]);
  await fs.writeFile(config.output,JSON.stringify({passed:true,phase:config.phase,data:result,mutation_count:posts.length,
    separate_contexts:Boolean(other),browser_version:browser.version()})+'\n',{flag:'wx',mode:0o600});
}
for(const signal of ['SIGTERM','SIGINT'])process.once(signal,async()=>{await browser?.close().catch(()=>{});process.exit(1);});
try{await main();}catch(error){
  if(config?.output)await fs.writeFile(config.output,JSON.stringify({passed:false,phase:config.phase,step,error_type:error?.name==='AssertionError'?'AssertionError':'BrowserError'})+'\n',{flag:'wx',mode:0o600}).catch(()=>{});
  process.exitCode=1;
}finally{await browser?.close().catch(()=>{process.exitCode=1;});}

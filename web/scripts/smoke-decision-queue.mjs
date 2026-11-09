// SPDX-License-Identifier: Elastic-2.0
// Built Workspace with an intercepted, read-only fixture API.
import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {once} from 'node:events'
import {readFile} from 'node:fs/promises'
import {createServer} from 'node:http'
import {createRequire} from 'node:module'
import {fileURLToPath} from 'node:url'
import path from 'node:path'
import {API_VERSION} from '../../packages/typescript-sdk/src/events.ts'

const {chromium} = createRequire(import.meta.url)(process.env.WORKSPACE_PLAYWRIGHT_PATH || 'playwright')
const dist = fileURLToPath(new URL('../dist/', import.meta.url))
const server = createServer(async (request, response) => {
  try {
    const file = path.resolve(dist, new URL(request.url, 'http://localhost').pathname.slice(1) || 'index.html')
    assert.ok(file.startsWith(dist))
    const bytes = await readFile(file)
    const type = file.endsWith('.js') ? 'text/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html'
    response.writeHead(200, {'content-type': type}).end(bytes)
  } catch {
    response.writeHead(404).end()
  }
})
server.listen(0, '127.0.0.1')
await once(server, 'listening')

const org = 'queue-fixture-org'
const otherOrg = 'queue-other-org'
const principal = 'queue-fixture-user'
const now = new Date().toISOString()
const task = (id, goal, status, waiting_reason) => ({
  id, root_id: id, depth: 0, goal, status, waiting_reason, owner_id: principal, organization_id: org,
  agent_id: 'queue-fixture-agent', agent_version: 1, version: 4, created_at: now, updated_at: now,
  budget_microusd: 10000, reserved_microusd: '1000', spent_microusd: '2000', financial_hold: false,
})
const tasks = [
  {...task('draft', 'Inspect failed CI run\nRun URL: https://ci.example.invalid/runs/4101\nL20 got status=401', 'waiting_input', 'model_candidate_pending'),
    candidate_id: 'ci-draft-candidate', current_run_id: 'ci-draft-run', attempt_count: 1, max_runs: 1, execution_deadline: now},
  task('approval', 'Confirm destination', 'waiting_approval', 'tool_approval_pending'),
  task('unknown', 'Reconcile comment receipt', 'blocked', 'external_action_unknown'),
  task('budget', 'Check remaining funds', 'blocked', 'budget_exceeded'),
  task('sent', 'Await remote receipt', 'waiting_approval', 'tool_execution_pending'),
  task('running', 'Background analysis', 'running'),
]
const draftText = 'See https://ci.example.invalid/runs/4101#L20 and #L21.'
const artifact = {id: 'ci-draft-artifact', organization_id: org, task_id: 'draft', version: 1, name: 'model-candidate.txt', content: draftText}
const otherTask = {...task('other-draft', 'Review the other workspace', 'waiting_input', 'model_candidate_pending'), organization_id: otherOrg}
const candidate = {id: 'ci-draft-candidate', organization_id: org, task_id: 'draft', run_id: 'ci-draft-run', source: 'model',
  artifact_id: artifact.id, artifact_version: artifact.version, content_digest: createHash('sha256').update(draftText).digest('hex')}
const headers = {'X-Platform-API-Version': API_VERSION}
const unexpected = [], detailReads = [], mutations = []
const attentionReads = []
const nextCursor = 'attention.fixture.page2'
let supportsAttention = true
let resolvedBudget = false
let holdOldQueue
let browser
try {
  browser = await chromium.launch({headless: true, ...(process.env.WORKSPACE_BROWSER_PATH ? {executablePath: process.env.WORKSPACE_BROWSER_PATH} : {})})
  const page = await browser.newPage({locale: 'ru-RU', viewport: {width: 1365, height: 920}})
  page.on('pageerror', error => unexpected.push(error.message))
  await page.route('**/auth/config', route => route.fulfill({headers, json: {data: {oidc_enabled: false}}}))
  await page.route('**/api/v1/**', async route => {
    const request = route.request()
    const url = new URL(request.url())
    const routePath = url.pathname
    const send = data => route.fulfill({headers, json: {data}})
    if (request.method() !== 'GET') {
      mutations.push(routePath)
      return route.fulfill({status: 405, headers, json: {error: {code: 'forbidden'}}})
    }
    const organization = request.headers()['x-organization-id']
    assert.ok(organization === org || organization === otherOrg)
    if (routePath === '/api/v1/me') return send({principal_id: principal, principal_kind: 'human', organization_id: organization, role: 'viewer', permissions: ['tasks.read']})
    if (routePath === '/api/v1/tasks') {
      const cursor = url.searchParams.get('cursor')
      if (url.searchParams.get('view') === 'attention') {
        if (organization === otherOrg) return route.fulfill({headers, json: {data: [otherTask], task_view: 'attention'}})
        if (holdOldQueue) {
          const hold = holdOldQueue
          holdOldQueue = undefined
          hold.started.resolve()
          await hold.release.promise
          // The old connection may already have aborted this request.
          await route.fulfill({status: 401, headers, json: {error: {code: 'unauthenticated'}}}).catch(() => {})
          hold.sent.resolve()
          return
        }
        attentionReads.push(cursor)
        if (!supportsAttention) return send([tasks[3]])
        assert.ok(cursor === null || cursor === nextCursor)
        const json = cursor === nextCursor ? {data: resolvedBudget ? [] : [tasks[3]]} :
          {data: tasks.slice(0, 3), ...(resolvedBudget ? {} : {next_cursor: nextCursor})}
        return route.fulfill({headers, json: {...json, task_view: 'attention'}})
      }
      assert.equal(cursor, null, 'ordinary task pages are not scanned for decisions')
      return send(organization === org ? tasks.slice(4) : [])
    }
    if (['/api/v1/agents', '/api/v1/tools'].includes(routePath)) return send([])
    const [, id, suffix] = /^\/api\/v1\/tasks\/([^/]+)(?:\/([^/]+))?$/.exec(routePath) ?? []
    const selected = tasks.find(item => item.id === id)
    if (selected) {
      detailReads.push(routePath)
      const window = {after_sequence: '0', through_sequence: '0', version: 1}
      if (suffix === 'event-snapshot') return send({organization_id: org, task_id: id, task: selected, events: [], cursor: 'fixture_head', replay_cursor: 'fixture_floor', window, has_earlier_events: false, observed_at: now})
      if (suffix === 'event-stream') {
        const batch = {organization_id: org, task_id: id, events: [], cursor: 'fixture_head', has_more: false, reset_required: false, window}
        return route.fulfill({headers, contentType: 'text/event-stream', body: `event: platform.events\nid: fixture_head\ndata: ${JSON.stringify({frame_version: 1, kind: 'batch', batch})}\n\n`})
      }
      if (!suffix) return send(selected)
      if (suffix === 'costs') return send({entries: [], unknown_reservation_count: '0'})
      if (id === 'draft' && suffix === 'artifacts') return send([artifact])
      if (id === 'draft' && suffix === 'candidates') return send([candidate])
      if (['artifacts', 'candidates', 'decisions', 'children', 'tool-executions'].includes(suffix)) return send([])
    }
    unexpected.push(routePath)
    return route.fulfill({status: 404, headers, json: {error: {code: 'not_found'}}})
  })
  await page.goto(`http://127.0.0.1:${server.address().port}`)
  await page.getByRole('button', {name: 'Подключить пространство', exact: true}).click()
  await page.getByLabel('Организация', {exact: true}).fill(org)
  await page.getByLabel('Токен доступа', {exact: true}).fill('synthetic-queue-token')
  await page.getByRole('button', {name: 'Подключить пространство', exact: true}).click()
  await page.getByRole('button', {name: 'Background analysis', exact: true}).waitFor()
  assert.equal(await page.getByText('Inspect failed CI run', {exact: false}).count(), 0)
  await page.getByRole('button', {name: 'Нужно внимание', exact: true}).click()
  const queue = page.getByRole('list', {name: 'Нужно внимание'})
  await queue.getByRole('heading', {name: 'Inspect failed CI run'}).waitFor()
  assert.equal(await queue.getByRole('listitem').count(), 3)
  await page.getByRole('button', {name: 'Загрузить ещё', exact: true}).click()
  await queue.getByRole('heading', {name: 'Check remaining funds'}).waitFor()
  assert.equal(await queue.getByRole('listitem').count(), 4)
  assert.ok(attentionReads.includes(nextCursor))
  assert.equal(detailReads.length, 0, 'listing should use task summaries only')
  await queue.getByText('Повторная отправка остановлена.', {exact: false}).waitFor()
  for (const [goal, tab] of [['Confirm destination', 'Действия'], ['Inspect failed CI run', 'Результат'], ['Check remaining funds', 'Ход работы']]) {
    await queue.getByRole('button', {name: new RegExp(goal)}).click()
    await page.getByRole('tab', {name: tab, exact: false, selected: true}).waitFor()
    if (goal === 'Inspect failed CI run') {
      const check = page.getByRole('region', {name: 'Ссылки на журнал CI'})
      await check.getByText('Некоторые ссылки неполные или не совпадают со строками исходного журнала.').waitFor()
      await check.getByText('Ссылки и строки журнала', {exact: true}).click()
      await check.getByText('L20 got status=401', {exact: true}).waitFor()
      await check.getByText('Совпадения нет', {exact: true}).waitFor()
      assert.equal(await check.getByRole('link').count(), 0)
    }
    await page.getByRole('button', {name: 'Нужно внимание', exact: true}).click()
    await queue.getByRole('heading', {name: goal}).waitFor()
  }
  await page.getByLabel('Interface language').selectOption('en')
  const englishQueue = page.getByRole('list', {name: 'Needs attention'})
  await englishQueue.getByText('Resending is blocked.', {exact: false}).waitFor()
  resolvedBudget = true
  await page.getByRole('button', {name: 'Refresh list', exact: true}).click()
  await englishQueue.getByRole('heading', {name: 'Check remaining funds'}).waitFor({state: 'detached'})
  assert.equal(await englishQueue.getByRole('listitem').count(), 3)
  supportsAttention = false
  await page.getByRole('button', {name: 'Refresh list', exact: true}).click()
  await page.getByText('The server did not confirm support for the decision queue. Check your RunVerge version.', {exact: true}).waitFor()
  assert.equal(await englishQueue.count(), 0, 'an unmarked response must not display an incomplete decision queue')
  supportsAttention = true
  await page.getByRole('button', {name: 'Retry', exact: true}).click()
  await englishQueue.getByText('Resending is blocked.', {exact: false}).waitFor()
  const held = {started: Promise.withResolvers(), release: Promise.withResolvers(), sent: Promise.withResolvers()}
  holdOldQueue = held
  try {
    await page.getByRole('button', {name: 'Refresh list', exact: true}).click()
    await held.started.promise
    await page.getByRole('button', {name: 'Connection', exact: true}).click()
    await page.getByLabel('Organization', {exact: true}).fill(otherOrg)
    await page.getByLabel('Access token', {exact: true}).fill('synthetic-other-token')
    await page.getByRole('button', {name: 'Connect workspace', exact: true}).click()
    await englishQueue.getByRole('heading', {name: otherTask.goal}).waitFor()
  } finally {
    held.release.resolve()
  }
  await held.sent.promise
  await englishQueue.getByRole('heading', {name: otherTask.goal}).waitFor()
  assert.equal(await englishQueue.getByRole('listitem').count(), 1, 'a late reply from the former connection must not change the active queue')
  await page.setViewportSize({width: 390, height: 844})
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'mobile overflow')
  assert.deepEqual(mutations, [])
  assert.deepEqual(unexpected, [])
  console.log(JSON.stringify({passed: true, paginated_attention_items: 4, resolved_task_removed: true, old_server_fails_closed: true, delayed_previous_connection_ignored: true, initial_detail_reads: 0, mutations: 0, mobile_width: 390, languages: ['ru', 'en']}))
} finally {
  await browser?.close()
  server.closeAllConnections()
  server.close()
  await once(server, 'close')
}

// Real local API + worker verification. No credential values, traces, or settings screenshots.
// Requires Playwright, a running server/worker, and a bootstrap file readable by this user.
import fs from 'node:fs/promises'
import path from 'node:path'
import { createRequire } from 'node:module'
import assert from 'node:assert/strict'
import {fileURLToPath} from 'node:url'
import {API_VERSION_HEADER,API_VERSION,livePreflight} from './live-preflight.mjs'

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const baseUrl = (process.env.AGENT_PLATFORM_URL || process.env.WORKSPACE_URL || 'http://127.0.0.1:8080').replace(/\/$/, '')
if(await livePreflight(baseUrl)) process.exit(0)
const require = createRequire(import.meta.url)
const { chromium } = require(process.env.WORKSPACE_PLAYWRIGHT_PATH || 'playwright')
const bootstrap = JSON.parse(await fs.readFile(process.env.AGENT_PLATFORM_CREDENTIALS || process.env.WORKSPACE_BOOTSTRAP || path.join(repo,'.local/bootstrap.json'), 'utf8'))
const output = path.resolve(process.env.AGENT_PLATFORM_QA_OUTPUT || path.join(repo,'.local/qa/completion'))
await fs.mkdir(output, { recursive: true })
let browser

async function readAPI(route) {
  const response = await fetch(`${baseUrl}/api/v1${route}`, {
    headers: { Authorization: `Bearer ${bootstrap.token}`, [API_VERSION_HEADER]:API_VERSION,'X-Organization-ID': bootstrap.organization_id },
    redirect: 'error', signal: AbortSignal.timeout(10000),
  })
  assert.equal(response.status, 200, 'Read-only verification request failed')
  assert.equal(response.headers.get(API_VERSION_HEADER),API_VERSION,'Incompatible API response')
  return (await response.json()).data
}

try {
  browser = await chromium.launch({ headless: true, ...(process.env.WORKSPACE_BROWSER_PATH ? { executablePath: process.env.WORKSPACE_BROWSER_PATH } : {}) })
  const page = await browser.newPage({ viewport: { width: 1440, height: 1050 }, deviceScaleFactor: 1 })
  const pageErrors = []
  page.on('pageerror', error => pageErrors.push(error.message))
  await page.goto(baseUrl)
  await page.getByRole('button', { name: 'Подключить пространство', exact: true }).click()
  await page.getByLabel('Организация', { exact: true }).fill(bootstrap.organization_id)
  await page.getByLabel('Токен доступа', { exact: true }).fill(bootstrap.token)
  await page.getByRole('button', { name: 'Подключить пространство', exact: true }).click()
  await page.getByText('API подключён', { exact: true }).waitFor()

  const agentName = `Проверка исполнения ${Date.now()}`
  await page.getByRole('button', { name: 'Агенты', exact: true }).click()
  await page.getByRole('button', { name: 'Создать агента', exact: true }).click()
  await page.getByLabel('Название', { exact: true }).fill(agentName)
  await page.getByLabel('Инструкции', { exact: true }).fill('Тестовый fixture:echo. Проверить выполнение, журнал, артефакт и синтетический учёт; не выдавать это за работу LLM.')
  await page.getByLabel('Модель', { exact: true }).selectOption('fixture:echo')
  await page.getByRole('button', { name: 'Сохранить черновик', exact: true }).click()
  const card = page.locator('.agent-card').filter({ has: page.getByRole('heading', { name: agentName, exact: true }) })
  await card.getByRole('button', { name: 'Опубликовать', exact: true }).click()
  await card.getByText('Опубликован', { exact: true }).waitFor()
  await card.getByText('Тестовый', { exact: true }).waitFor()

  await page.locator('.sidebar nav').getByRole('button', { name: /^Задачи/ }).click()
  await page.getByRole('button', { name: 'Новая задача', exact: true }).click()
  const goal = 'Проверить сохранение результата, материалов и расходов тестового запуска.'
  await page.getByLabel('Что нужно сделать?', { exact: true }).fill(goal)
  await page.getByLabel('Исполнитель', { exact: true }).selectOption({ label: `${agentName} · v1 · тестовый` })
  await page.getByLabel('Бюджет задачи, USD', { exact: true }).fill('0.01')
  const createResponse = page.waitForResponse(response => response.url() === `${baseUrl}/api/v1/tasks` && response.request().method() === 'POST')
  await page.getByRole('button', { name: 'Создать задачу', exact: true }).click()
  const created = (await (await createResponse).json()).data
  assert.equal(typeof created.id, 'string')
  await page.getByRole('heading', { name: goal, exact: true }).waitFor()
  await page.getByText('Завершена', { exact: true }).waitFor({ timeout: 30000 })
  const expectedResult = `Тестовый результат\n${goal}`
  assert.equal(await page.locator('.review-candidate .prose-output').innerText(), expectedResult)
  await page.getByText(/Расходы синтетические: деньги не списываются/).waitFor()

  const task = await readAPI(`/tasks/${encodeURIComponent(created.id)}`)
  assert.equal(task.status, 'succeeded')
  assert.equal(task.spent_microusd, '1000')
  assert.equal(task.reserved_microusd, '0')
  assert.equal(task.budget_microusd, 10000)
  assert.equal(task.result, expectedResult)
  const spentText = await page.locator('.cost-breakdown > div').filter({ hasText: 'Потрачено' }).locator('dd').innerText()
  assert.match(spentText, /0,001\s*\$/)

  await page.getByRole('tab', { name: /Ход работы/ }).click()
  const events = await readAPI(`/tasks/${encodeURIComponent(created.id)}/events?after=0&limit=100`)
  const types = events.map(event => event.type)
  for (const type of ['task.created', 'operation.claimed', 'cost.settled', 'task.succeeded']) assert.ok(types.includes(type), `Missing event ${type}`)
  const costEvent = events.find(event => event.type === 'cost.settled')
  assert.equal(costEvent.payload_version, 1)
  assert.equal(costEvent.data.synthetic_cost, true)
  assert.equal(costEvent.data.actual_microusd, '1000')
  await page.locator('.timeline').getByText('Расход подтверждён', { exact: true }).waitFor()
  await page.locator('.timeline').getByText('Работа завершена', { exact: true }).waitFor()

  await page.getByRole('tab', { name: /Материалы/ }).click()
  await page.locator('.artifact').getByText('fixture-result.txt', { exact: true }).waitFor()
  assert.equal(await page.locator('.artifact-preview').innerText(), expectedResult)
  await page.getByText('Происхождение материала', { exact: true }).click()
  const provenance = JSON.parse(await page.locator('.artifact .json-output').innerText())
  assert.equal(provenance.adapter, 'fixture:echo')
  assert.equal(provenance.verified, true)
  assert.equal(provenance.verification, 'exact_goal_echo')
  assert.equal(provenance.synthetic_cost, true)
  assert.equal(typeof provenance.operation_id, 'string')
  await page.screenshot({ path: path.join(output, 'task-completed-0.11-desktop.png'), fullPage: true })

  const persisted = await page.evaluate(() => ({ local: Object.fromEntries(Object.entries(localStorage)), session: Object.fromEntries(Object.entries(sessionStorage)) }))
  assert.ok(!JSON.stringify(persisted).includes(bootstrap.token), 'Token was persisted')
  assert.deepEqual(Object.keys(persisted.local), ['agent-platform.organization'])
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForFunction(() => document.querySelector('.sidebar').getBoundingClientRect().right <= 1)
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'Mobile document overflow')
  await page.getByRole('tab', { name: 'Результат', exact: true }).click()
  assert.equal(await page.locator('.review-candidate .prose-output').innerText(), expectedResult)
  await page.screenshot({ path: path.join(output, 'task-completed-0.11-mobile.png'), fullPage: true })
  await page.getByRole('button', { name: 'Открыть навигацию', exact: true }).click()
  await page.getByRole('button', { name: 'Подключение', exact: true }).click()
  await page.getByRole('button', { name: 'Отключиться', exact: true }).click()
  await page.reload()
  await page.getByRole('heading', { name: 'Начните с подключения', exact: true }).waitFor()
  assert.deepEqual(pageErrors, [])

  const report = {
    executed_at: new Date().toISOString(), status: 'passed', contract: API_VERSION,
    adapter: 'fixture:echo', real_llm_call: false, synthetic_cost: true,
    task_id: task.id, task_status: task.status, spent_microusd: task.spent_microusd,
    reserved_microusd: task.reserved_microusd, budget_microusd: task.budget_microusd,
    artifact_verification: provenance.verification, event_types: types,
    browser_errors: pageErrors.length, mobile_width: 390, mobile_overflow: false,
    token_persisted: false, disconnect_reload_verified: true,
    screenshots: ['task-completed-0.11-desktop.png', 'task-completed-0.11-mobile.png'],
  }
  await fs.writeFile(path.join(output, 'completion-report-0.11.json'), `${JSON.stringify(report, null, 2)}\n`)
  console.log('PASS: published fixture agent → task succeeded → exact result/artifact/provenance → synthetic cost 1000 micro-USD and reserve 0 → journal → mobile → token memory-only/disconnect. No LLM or real billing claim.')
} catch (error) {
  console.error((error?.stack || String(error)).split(bootstrap.token).join('[REDACTED]'))
  process.exitCode = 1
} finally {
  await browser?.close()
}

// SPDX-License-Identifier: Elastic-2.0
// Real compiled UI -> TLS HTTP API -> isolated PostgreSQL -> local OIDC fixture.
// No real IdP credentials, bearer tokens, network traces or credential screenshots.
import fs from 'node:fs/promises'
import path from 'node:path'
import assert from 'node:assert/strict'
import { createRequire } from 'node:module'

const require = createRequire(import.meta.url)
const { chromium } = require(process.env.WORKSPACE_PLAYWRIGHT_PATH || 'playwright')
const metadataPath = process.env.BROWSER_AUTH_SMOKE_METADATA
if (!metadataPath) throw new Error('BROWSER_AUTH_SMOKE_METADATA is required from the Go TLS/PG fixture')
const metadata = JSON.parse(await fs.readFile(metadataPath, 'utf8'))
const output = path.resolve('.qa')
await fs.mkdir(output, { recursive: true })
let browser
let page
const responses = []
let blockedByCSP = false
const pageErrors = []
const mutationChecks = []
try {
  browser = await chromium.launch({ headless: true, args: [`--host-resolver-rules=MAP ${metadata.host} 127.0.0.1`, '--no-proxy-server'], ...(process.env.WORKSPACE_BROWSER_PATH ? { executablePath: process.env.WORKSPACE_BROWSER_PATH } : {}) })
  // Only this test browser accepts the fixture's self-signed certificates. The
  // server's OIDC HTTP client still verifies the configured fixture CA normally.
  const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 1000 } })
  page = await context.newPage()
  page.on('response', response => { const url = new URL(response.url()); responses.push({ path: url.pathname, status: response.status() }) })
  page.on('console', message => { if (message.text().includes('Content Security Policy') || message.text().includes('form-action')) blockedByCSP = true })
  page.on('pageerror', error => pageErrors.push(error.message))
  page.on('request', request => {
    if (request.method() === 'POST' && request.url().startsWith(`${metadata.url}/api/`)) {
      const headers = request.headers()
      mutationChecks.push({ csrf: Boolean(headers['x-csrf-token']), bearer: Boolean(headers.authorization) })
    }
  })
  await page.goto(metadata.url)
  await page.getByRole('button', { name: 'Войти через организацию', exact: true }).click()
  await page.getByText('API подключён', { exact: true }).waitFor()
  assert.equal(new URL(page.url()).origin, metadata.url)
  const cookie = (await context.cookies()).find(item => item.name === '__Host-agent-platform-session')
  assert.ok(cookie?.httpOnly && cookie.secure && cookie.sameSite === 'Lax')
  assert.equal(await page.getByLabel('Токен доступа', { exact: true }).count(), 0)
  await page.getByRole('button', { name: 'Агенты', exact: true }).click()
  await page.getByRole('button', { name: 'Создать агента', exact: true }).click()
  const agent = 'SSO fixture · Chromium'
  await page.getByLabel('Название', { exact: true }).fill(agent)
  await page.getByLabel('Инструкции', { exact: true }).fill('Тестовая проверка входа и прав; не реальная работа LLM.')
  await page.getByLabel('Модель', { exact: true }).selectOption('fixture:echo')
  await page.getByRole('button', { name: 'Сохранить черновик', exact: true }).click()
  const card = page.locator('.agent-card').filter({ has: page.getByRole('heading', { name: agent, exact: true }) })
  await card.getByRole('button', { name: 'Опубликовать', exact: true }).click()
  await card.getByText('Опубликован', { exact: true }).waitFor()
  await page.getByRole('button', { name: 'Задачи', exact: true }).click()
  await page.getByRole('button', { name: 'Новая задача', exact: true }).click()
  await page.getByLabel('Что нужно сделать?', { exact: true }).fill('Проверить защищённое создание и отмену через SSO.')
  await page.getByLabel('Бюджет задачи, USD', { exact: true }).fill('0.01')
  await page.getByRole('button', { name: 'Создать задачу', exact: true }).click()
  await page.getByRole('heading', { name: 'Проверить защищённое создание и отмену через SSO.', exact: true }).waitFor()
  await page.getByRole('button', { name: 'Отменить задачу', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Отменить задачу', exact: true }).click()
  await page.getByText('Отменена', { exact: true }).waitFor()
  await page.screenshot({ path: path.join(output, 'oidc-desktop.png'), fullPage: true })
  await page.getByRole('button', { name: 'Подключение', exact: true }).click()
  const organizations = page.getByLabel('Организация для работы', { exact: true })
  assert.match(await organizations.innerText(), /OIDC fixture organization/)
  const rotating = page.waitForResponse(response => new URL(response.url()).pathname === '/auth/session/rotate' && response.request().method() === 'POST')
  await page.getByRole('button', { name: 'Обновить сессию', exact: true }).click()
  await (await rotating).finished()
  await page.getByRole('heading', { name: 'Задачи', exact: true }).waitFor()
  await page.getByText('API подключён', { exact: true }).waitFor()
  const replaced = (await context.cookies()).find(item => item.name === '__Host-agent-platform-session')
  assert.ok(replaced?.value && replaced.value !== cookie.value, 'rotation must replace the cookie')
  await page.setViewportSize({ width: 390, height: 844 })
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
  await page.getByRole('button', { name: 'Открыть навигацию', exact: true }).click()
  await page.getByRole('button', { name: 'Подключение', exact: true }).click()
  await page.getByRole('button', { name: 'Выйти', exact: true }).click()
  await page.getByRole('button', { name: 'Войти через организацию', exact: true }).waitFor()
  assert.equal((await context.cookies()).some(item => item.name === '__Host-agent-platform-session'), false)
  await page.reload()
  await page.getByRole('button', { name: 'Войти через организацию', exact: true }).waitFor()
  assert.equal(await page.evaluate(() => Object.keys(localStorage).every(key => key === 'agent-platform.organization') && sessionStorage.length === 0), true)
  assert.equal(mutationChecks.length, 4)
  assert.ok(mutationChecks.every(check => check.csrf && !check.bearer))
  assert.deepEqual(pageErrors, [])
  const report = { date: new Date().toISOString(), result: 'PASS', scope: 'compiled UI + actual TLS API + isolated PostgreSQL + local OIDC fixture', checks: ['code+PKCE login navigation', 'secure HttpOnly SameSite cookie', 'membership organization selector', 'cookie mutations with CSRF and no Bearer', 'fixture agent create/publish', 'queued task create/cancel', 'session rotation', 'logout and reload', 'mobile overflow', 'no credential storage', 'no JavaScript errors'], realCorporateIdP: false, actualLLMExecution: false, screenshot: 'oidc-desktop.png' }
  await fs.writeFile(path.join(output, 'oidc-report.json'), JSON.stringify(report, null, 2))
  process.stdout.write('PASS: compiled Workspace OIDC browser flow, real TLS API and isolated PostgreSQL\n')
} catch (error) {
  if (page) {
    await page.screenshot({ path: path.join(output, 'oidc-failure.png'), fullPage: true }).catch(() => {})
    process.stderr.write(JSON.stringify({ path: new URL(page.url()).pathname, responses, blockedByCSP }) + '\n')
  }
  throw error
} finally {
  if (browser) await browser.close()
  await fs.writeFile(`${metadataPath}.done`, 'finished\n', { mode: 0o600 })
}

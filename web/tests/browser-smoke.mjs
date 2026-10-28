// Optional real-server smoke test. Requires Playwright and an explicit local bootstrap file.
// No token, network trace, or connection-form screenshot is emitted.
import fs from 'node:fs/promises'
import path from 'node:path'
import { createRequire } from 'node:module'
import assert from 'node:assert/strict'

const require = createRequire(import.meta.url)
const { chromium } = require(process.env.WORKSPACE_PLAYWRIGHT_PATH || 'playwright')
const bootstrap = JSON.parse(await fs.readFile(process.env.WORKSPACE_BOOTSTRAP || '../.local/bootstrap.json', 'utf8'))
const baseUrl = process.env.WORKSPACE_URL || 'http://127.0.0.1:8080'
const output = path.resolve('.qa')
await fs.mkdir(output, { recursive: true })
let browser
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
  const unique = `QA ${Date.now()}`
  await page.getByRole('button', { name: 'Агенты', exact: true }).click()
  await page.getByRole('button', { name: 'Создать агента', exact: true }).click()
  await page.getByLabel('Название', { exact: true }).fill(unique)
  await page.getByLabel('Инструкции', { exact: true }).fill('Явная проверка интерфейса с fixture:echo. Не имитировать результат LLM.')
  await page.getByRole('button', { name: 'Сохранить черновик', exact: true }).click()
  const card = page.locator('.agent-card').filter({ has: page.getByRole('heading', { name: unique, exact: true }) })
  await card.getByRole('button', { name: 'Опубликовать', exact: true }).click()
  await card.getByText('Опубликован', { exact: true }).waitFor()
  await page.screenshot({ path: path.join(output, 'agents-desktop.png'), fullPage: true })
  await page.getByRole('button', { name: 'Задачи', exact: true }).click()
  await page.getByRole('button', { name: 'Новая задача', exact: true }).click()
  const goal = `Проверить рабочее пространство · ${unique}`
  await page.getByLabel('Что нужно сделать?', { exact: true }).fill(goal)
  await page.getByLabel('Исполнитель', { exact: true }).selectOption({ label: `${unique} · v1 · тестовый` })
  await page.getByLabel('Бюджет задачи, USD', { exact: true }).fill('0.01')
  await page.getByRole('button', { name: 'Создать задачу', exact: true }).click()
  await page.getByRole('heading', { name: goal, exact: true }).waitFor()
  await page.getByRole('tab', { name: /Ход работы/ }).click()
  await page.locator('.timeline li').first().waitFor()
  await page.screenshot({ path: path.join(output, 'task-desktop.png'), fullPage: true })
  await page.getByRole('button', { name: 'Отменить задачу', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Отменить задачу', exact: true }).click()
  await page.getByText('Отменена', { exact: true }).waitFor()
  await page.getByRole('button', { name: 'Все задачи', exact: true }).click()
  await page.getByRole('button', { name: goal, exact: true }).waitFor()
  await page.screenshot({ path: path.join(output, 'tasks-desktop.png'), fullPage: true })
  const persisted = await page.evaluate(() => ({ local: Object.fromEntries(Object.entries(localStorage)), session: Object.fromEntries(Object.entries(sessionStorage)) }))
  assert.ok(!JSON.stringify(persisted).includes(bootstrap.token), 'Token was persisted')
  assert.deepEqual(Object.keys(persisted.local), ['agent-platform.organization'])
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForFunction(() => document.querySelector('.sidebar').getBoundingClientRect().right <= 1)
  await page.screenshot({ path: path.join(output, 'tasks-mobile.png'), fullPage: true })
  const mobileFits = await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
  if (!mobileFits) console.log(await page.evaluate(() => ({ width: innerWidth, document: document.documentElement.scrollWidth, overflow: [...document.querySelectorAll('main,main *,header,.main-shell,.app-shell')].filter(x => x.getBoundingClientRect().right > innerWidth + 1).slice(0, 20).map(x => ({ tag: x.tagName, class: x.className, width: x.getBoundingClientRect().width, right: x.getBoundingClientRect().right })) })))
  assert.ok(mobileFits, 'Mobile document overflow')
  await page.getByRole('button', { name: 'Открыть навигацию', exact: true }).click()
  await page.getByRole('button', { name: 'Подключение', exact: true }).click()
  await page.getByRole('button', { name: 'Отключиться', exact: true }).click()
  await page.getByRole('heading', { name: 'Начните с подключения', exact: true }).waitFor()
  await page.reload()
  await page.getByRole('heading', { name: 'Начните с подключения', exact: true }).waitFor()
  assert.deepEqual(pageErrors, [])
  console.log('PASS: real API connection, agent create/publish, task create/events/cancel, mobile layout, token memory-only, disconnect/reload; no browser errors.')
} catch (error) {
  const message = (error?.stack || String(error)).split(bootstrap.token).join('[REDACTED]')
  console.error(message)
  process.exitCode = 1
} finally {
  await browser?.close()
}

// SPDX-License-Identifier: Elastic-2.0
// Verify the installed Workspace preparation path with an owned local fixture.
import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import {createRequire} from 'node:module'

const config = JSON.parse(await readFile(process.argv[2], 'utf8'))
const origin = new URL(config.base_url)
assert.equal(origin.hostname, '127.0.0.1')
assert.equal(origin.origin, config.base_url)
const {chromium} = createRequire(import.meta.url)(config.playwright)
let browser
try {
  browser = await chromium.launch({headless: true, executablePath: config.chromium})
  const page = await browser.newPage({locale: 'ru-RU'})
  page.setDefaultTimeout(12000)
  const writes = []
  await page.route('**/*', route => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.origin !== origin.origin) return route.abort()
    if (request.method() === 'POST') writes.push(url.pathname)
    return route.continue()
  })
  await page.goto(origin.origin)
  await page.getByRole('button', {name: 'Подключить пространство', exact: true}).click()
  await page.getByLabel('Организация', {exact: true}).fill(config.credentials.organization_id)
  await page.getByLabel('Токен доступа', {exact: true}).fill(config.credentials.token)
  await page.getByRole('button', {name: 'Подключить пространство', exact: true}).click()
  await page.getByRole('button', {name: 'Нужно внимание', exact: true}).click()
  await page.getByRole('list', {name: 'Нужно внимание'}).getByRole('button', {name: /runs\/4102/}).click()
  await page.getByRole('tab', {name: 'Действия', exact: true}).click()
  await page.getByRole('button', {name: 'Подготовить действие', exact: true}).click()
  let dialog = page.getByRole('dialog')
  await dialog.getByRole('heading', {name: /Publish GitHub CI comment/}).waitFor()
  assert.equal(await dialog.getByLabel('Точные аргументы JSON').count(), 0)
  await dialog.getByRole('button', {name: 'Проверить намерение', exact: true}).click()
  dialog = page.getByRole('dialog', {name: 'Проверить и зарезервировать'})
  const args = JSON.parse(await dialog.locator('pre.artifact-preview').first().textContent())
  assert.deepEqual(args, {
    repository_id: 10, workflow_id: 20, run_id: 4102, run_attempt: 1, branch: 'main',
    commit_sha: 'a'.repeat(40), conclusion: 'failure', body: config.draft,
  })
  assert.deepEqual(writes, [])
  await dialog.getByRole('button', {name: 'Подготовить и зарезервировать', exact: true}).click()
  await page.getByRole('region', {name: 'Точное действие'}).waitFor()
  assert.deepEqual(writes, [`/api/v1/tasks/${config.task_id}/tool-executions`])
  console.log(JSON.stringify({exact_preview: true, prepared_only: true, mutations: writes.length}))
} catch (error) {
  console.error((error?.stack ?? String(error)).split(config.credentials.token).join('[REDACTED]'))
  process.exitCode = 1
} finally {
  await browser?.close()
}

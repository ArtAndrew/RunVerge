// SPDX-License-Identifier: Elastic-2.0
// Exercise the installed UI against the verifier's disposable local API.
import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import {createRequire} from 'node:module'

const config = JSON.parse(await readFile(process.argv[2], 'utf8'))
const {chromium} = createRequire(import.meta.url)(config.playwright)
const origin = new URL(config.base_url)
assert.equal(origin.origin, config.base_url)
assert.equal(origin.hostname, '127.0.0.1')
assert.equal(origin.protocol, 'http:')

let browser
try {
  browser = await chromium.launch({headless: true, executablePath: config.chromium})
  const page = await browser.newPage({locale: 'ru-RU', viewport: {width: 1365, height: 960}})
  page.setDefaultTimeout(12000)
  const errors = []
  const mutations = []
  page.on('pageerror', () => errors.push('page error'))
  page.on('response', response => {
    if (response.request().method() === 'POST') {
      mutations.push({path: new URL(response.url()).pathname, status: response.status()})
    }
  })
  await page.route('**/*', route => new URL(route.request().url()).origin === origin.origin ? route.continue() : route.abort())
  await page.goto(origin.origin)
  await page.getByRole('button', {name: 'Подключить пространство', exact: true}).click()
  await page.getByLabel('Организация', {exact: true}).fill(config.credentials.organization_id)
  await page.getByLabel('Токен доступа', {exact: true}).fill(config.credentials.token)
  await page.getByRole('button', {name: 'Подключить пространство', exact: true}).click()
  const queueResponse = page.waitForResponse(response => {
    const url = new URL(response.url())
    return response.ok() && url.pathname === '/api/v1/tasks' && url.searchParams.get('view') === 'attention'
  })
  await page.getByRole('button', {name: 'Нужно внимание', exact: true}).click()
  const queue = page.getByRole('list', {name: 'Нужно внимание'})
  const observed = await (await queueResponse).json()
  assert.equal(observed.task_view, 'attention')
  assert.ok(observed.data.some(task => task.id === config.task_id && task.organization_id === config.credentials.organization_id))
  if (config.background_task_id) {
    assert.ok(observed.data.every(task => task.id !== config.background_task_id))
  }
  await queue.getByRole('button', {name: /runs\/4102/}).click()
  const check = page.getByRole('region', {name: 'Ссылки на журнал CI'})
  await check.waitFor()
  assert.equal(await page.locator('.review-candidate > .prose-output').textContent(), config.original)
  const incomplete = await check.getByText(/Некоторые ссылки неполные|В черновике нет ссылок/).count() > 0

  await page.getByRole('button', {name: 'Создать редактируемую копию', exact: true}).click()
  let dialog = page.getByRole('dialog')
  const editor = dialog.getByLabel('Текст материала', {exact: true})
  await editor.waitFor()
  assert.equal(await editor.inputValue(), config.original)
  const withoutRunUrl = config.corrected.replace('https://ci.example.invalid/runs/4102', '')
  await editor.fill(withoutRunUrl)
  await dialog.getByText('Предложенные исправления ссылок', {exact: true}).click()
  await dialog.getByRole('button', {name: 'Дополнить ссылки в черновике', exact: true}).click()
  assert.equal(await editor.inputValue(), config.corrected)
  assert.deepEqual(mutations, [])
  await dialog.getByText('Ссылки совпадают со строками исходного журнала.', {exact: true}).waitFor()
  await dialog.getByRole('button', {name: 'Сохранить черновик', exact: true}).click()
  await dialog.getByRole('button', {name: 'Подготовить публикацию', exact: true}).click()
  assert.equal(await dialog.locator('[data-material-publication]').textContent(), config.corrected)
  await dialog.getByRole('button', {name: 'Предложить как результат', exact: true}).click()
  await check.getByText('Ссылки совпадают со строками исходного журнала.', {exact: true}).waitFor()
  await check.getByText('Ссылки и строки журнала', {exact: true}).click()
  await check.getByText('No space left on device', {exact: false}).waitFor()
  await page.getByRole('button', {name: 'Принять результат', exact: true}).click()
  dialog = page.getByRole('dialog')
  assert.equal(await dialog.locator('.artifact-preview').textContent(), config.corrected)
  await dialog.getByRole('button', {name: 'Принять результат', exact: true}).click()
  await page.getByText('Принятый материал', {exact: true}).waitFor()
  await page.getByRole('button', {name: 'Нужно внимание', exact: true}).click()
  await queue.getByRole('button', {name: /runs\/4102/}).waitFor({state: 'detached'})
  await queue.getByRole('button', {name: /runs\/4101/}).waitFor()
  if (config.background_task_id) {
    await page.getByRole('button', {name: 'Все', exact: true}).click()
    const background = page.getByRole('row', {name: /runs\/4103/})
    await background.getByText('В работе', {exact: true}).waitFor()
  }

  assert.deepEqual(errors, [])
  assert.deepEqual(mutations.map(item => item.status), [201, 200, 200])
  assert.equal(mutations[0].path, `/api/v1/tasks/${config.task_id}/materials`)
  assert.ok(mutations[1].path.startsWith(mutations[0].path + '/') && mutations[1].path.endsWith('/publish'))
  assert.equal(mutations[2].path, `/api/v1/tasks/${config.task_id}/review`)
  console.log(JSON.stringify({passed: true, organization_queue_verified: true, accepted_task_left_queue: true,
    background_visible_as_running: Boolean(config.background_task_id),
    original_references_incomplete: incomplete, link_suggestion_applied: true, mutations: 3}))
} catch (error) {
  // The temporary configuration contains a scoped test credential.
  console.error((error?.stack ?? String(error)).split(config.credentials.token).join('[REDACTED]'))
  process.exitCode = 1
} finally {
  await browser?.close()
}

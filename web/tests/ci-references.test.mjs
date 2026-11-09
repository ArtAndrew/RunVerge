// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import {checkCIReferences} from '../src/ciReferences.ts'

const goal = readFileSync(new URL('../../examples/ci-triage/01-auth-regression.txt', import.meta.url), 'utf8')
const url = 'https://ci.example.invalid/runs/4101'

test('matches complete same-run Markdown references and shows exact source lines', () => {
  const result = checkCIReferences(goal, `[Trace](${url}#L20-L21). Also ${url}#L21.`)
  assert.equal(result.status, 'matched')
  assert.deepEqual(result.references[0].lines, [
    {number: 20, text: 'expected status=200; got status=401'},
    {number: 21, text: 'response: {"error":"invalid_token"}'},
  ])
})

test('bare lines, a different run and an invalid range remain unresolved', () => {
  const result = checkCIReferences(goal,
    `#L20 ${url}7#L20 ${url}#L20-L30 ${url}#L21-L19 ${url}#L20-#L21 ${url}`)
  assert.equal(result.status, 'issues')
  assert.equal(result.references.length, 6)
  assert.ok(result.references.every(item => !item.lines.length))
})

test('refuses a range with a missing line even if both ends are present', () => {
  const sparse = goal.split('\n').filter(line => !line.startsWith('L20 ')).join('\n')
  assert.equal(checkCIReferences(sparse, `${url}#L19-L21`).status, 'issues')
})

test('offers exact replacements for resolvable bare anchors and preserves all other draft text', () => {
  const before = `Finding: 401.\nEvidence: #L20-#L21, #L999. Foreign: https://other.invalid/run#L20.\nNext check: compare issuers.`
  const report = checkCIReferences(goal, before)
  assert.deepEqual(report.repair.replacements, [{before: '#L20-#L21', after: `${url}#L20-L21`}])
  assert.equal(report.repair.draft, before.replace('#L20-#L21', `${url}#L20-L21`))
  assert.equal(checkCIReferences(goal, report.repair.draft).status, 'issues')
})

test('does not repair code, foreign URL fragments or partial and missing ranges', () => {
  const draft = [
    '```text', '#L20', '```', '~~~', '#L20', '~~~', '`example #L20`', '    #L20',
    'destination=#L20', '#L20-L21suffix', 'https://foreign.invalid/run#L20', '#L20-L999',
  ].join('\n')
  assert.equal(checkCIReferences(goal, draft).repair, undefined)
})

test('does not offer an incomplete repair when expanding anchors would exceed the limit', () => {
  const draft = '#L20 '.repeat(4000)
  const report = checkCIReferences(goal, draft)
  assert.equal(report.status, 'issues')
  assert.equal(report.repair, undefined)
})

test('does not infer CI mode for ordinary tasks or present an ambiguous log as checked', () => {
  assert.equal(checkCIReferences('Write a note', `${url}#L20`), null)
  assert.equal(checkCIReferences(goal, 'The result is unknown.').status, 'missing')
  assert.equal(checkCIReferences(goal + 'L20 duplicate', `${url}#L20`).status, 'unavailable')
  assert.equal(checkCIReferences(goal + `Run URL: ${url}`, `${url}#L20`).status, 'unavailable')
  assert.equal(checkCIReferences(goal.replace(url, 'https://user:secret@ci.example.invalid/runs/4101'), `${url}#L20`).status, 'unavailable')
})

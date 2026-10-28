// SPDX-License-Identifier: Elastic-2.0
import {test} from 'node:test'
import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {comparisonArtifact, comparisonDecisions} from '../src/comparison.ts'

const content = 'Original draft\n  Точные пробелы. 中文\n'
const digest = createHash('sha256').update(content).digest('hex')
const candidate = {id:'c-old', task_id:'task', organization_id:'org', run_id:'old-run', artifact_id:'a-old', artifact_version:1, content_digest:digest}
const artifact = {id:'a-old', task_id:'task', organization_id:'org', version:1, name:'model-candidate.txt', content}

test('historical text retains exact bytes and cannot be replaced by another same-named artifact', async () => {
  assert.equal((await comparisonArtifact('org', 'task', candidate, artifact)).content, content)
  for (const change of [{id:'a-new'}, {version:2}, {content:content.trim()}, {content:content.replace('  ',' ')}, {task_id:'other'}, {organization_id:'other'}]) {
    await assert.rejects(comparisonArtifact('org', 'task', candidate, {...artifact,...change}))
  }
})

test('historical candidates are still scoped and digest-bound', async () => {
  for (const change of [{task_id:'other'}, {organization_id:'other'}, {content_digest:'0'.repeat(64)}, {content_digest:digest+'\n'}]) {
    await assert.rejects(comparisonArtifact('org', 'task', {...candidate,...change}, artifact))
  }
})

test('feedback belongs to exact candidate, artifact, run and tenant, even when names and versions coincide', () => {
  const decision = {...candidate, id:'decision', candidate_id:candidate.id, decision:'request_changes', reason:'Keep the date'}
  assert.deepEqual(comparisonDecisions(candidate, [decision]), [decision])
  for (const change of [{candidate_id:'new'}, {artifact_id:'new'}, {artifact_version:2}, {run_id:'new'}, {content_digest:'0'.repeat(64)}, {task_id:'other'}, {organization_id:'other'}]) {
    assert.deepEqual(comparisonDecisions(candidate, [{...decision,...change}]), [])
  }
})

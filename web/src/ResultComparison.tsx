// SPDX-License-Identifier: Elastic-2.0
import {useEffect, useState} from 'react'
import type {ApiClient, Artifact, Task, TaskCandidate, TaskDecision} from './api'
import {t} from './i18n'
import {dateTime} from './format'
import {decisionLabels} from './review'
import {comparisonArtifact, comparisonDecisions} from './comparison'
import {ErrorNotice, Loading} from './ui'
import CopyText from './CopyText'
import './ResultComparison.css'

export default function ResultComparison({api, task, candidates, decisions, loadError}: {
  api: ApiClient; task: Task; candidates: TaskCandidate[]; decisions: TaskDecision[]; loadError: unknown
}) {
  const versions = candidates.filter(item => item.organization_id === task.organization_id && item.task_id === task.id)
    .sort((a, b) => a.created_at.localeCompare(b.created_at) || a.id.localeCompare(b.id))
  const [selection, setSelection] = useState<[string, string] | null>(null)
  const left = versions.find(item => item.id === (selection?.[0] ?? versions.at(-2)?.id))
  const right = versions.find(item => item.id === (selection?.[1] ?? versions.at(-1)?.id))
  useEffect(() => {
    if (!selection && left && right) setSelection([left.id, right.id])
  }, [selection, left?.id, right?.id])
  const binding = JSON.stringify([task.organization_id, task.id, left, right])
  const [loaded, setLoaded] = useState<{binding:string; artifacts: Artifact[]} | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [refresh, setRefresh] = useState(0)
  useEffect(() => {
    const controller = new AbortController()
    setLoaded(null); setError(null)
    if (!left || !right || loadError) return () => controller.abort()
    void Promise.all([left, right].map(async candidate => comparisonArtifact(task.organization_id, task.id, candidate,
      await api.artifact(task.id, candidate.artifact_id, candidate.artifact_version, controller.signal))))
      .then(artifacts => {if (!controller.signal.aborted) setLoaded({binding, artifacts})})
      .catch(cause => {if (!controller.signal.aborted) setError(cause)})
    return () => controller.abort()
    // Binding includes exact candidate records, so polling unchanged history
    // does not repeatedly fetch text or reset the reader's selection.
  }, [api, binding, Boolean(loadError), refresh])

  const artifacts = !loadError && loaded?.binding === binding ? loaded.artifacts : null
  if (versions.length < 2) return <p className="aside-note">{t('Для сравнения нужны два сохранённых результата. Они появятся после доработки или публикации правок.')}</p>
  return <section className="result-comparison" aria-label={t('Сравнение результатов')}>
    <p className="aside-note">{t('Сравните сохранённые тексты и замечания. Эта страница не проверяет факты и не запускает агента.')}</p>
    <div className="comparison-selectors">{[left, right].map((candidate, index) => <label className="field" key={index}>
      {index === 0 ? t('Результат слева') : t('Результат справа')}
      <select value={candidate?.id ?? ''} onChange={event => {
        const next: [string, string] = [left?.id ?? '', right?.id ?? '']; next[index] = event.target.value; setSelection(next)
      }}>{versions.map((item, number) => <option key={item.id} value={item.id}>
        {number + 1} · {dateTime(item.created_at)} · {item.id.slice(0, 8)}
      </option>)}</select>
    </label>)}</div>
    <ErrorNotice error={error} onRetry={() => setRefresh(value => value + 1)}/>
    {Boolean(loadError) && <p role="alert">{t('Нет актуального подтверждения доступа к задаче. Обновите данные перед действием.')}</p>}
    {!artifacts && !error && !loadError && <Loading/>}
    {artifacts && <>
      <p role="status">{artifacts[0].content === artifacts[1].content ? t('Тексты совпадают полностью.') : t('Тексты различаются. Проверьте обе версии перед решением.')}</p>
      <div className="comparison-columns">{[left!, right!].map((candidate, index) => <article key={`${index}:${candidate.id}`}>
        <h3>{index === 0 ? t('Результат слева') : t('Результат справа')}</h3>
        <p className="muted">{artifacts[index].name} · v{artifacts[index].version} · {dateTime(candidate.created_at)}</p>
        <CopyText text={artifacts[index].content}/>
        <pre className="prose-output comparison-text">{artifacts[index].content}</pre>
        <details><summary>{t('Версия и контрольная сумма')}</summary><p>{candidate.id}</p><p>{candidate.run_id}</p><code>{candidate.content_digest}</code></details>
        <h4>{t('Замечания и решения к этой версии')}</h4>
        {comparisonDecisions(candidate, decisions).length ? comparisonDecisions(candidate, decisions).map(decision => <div className="comparison-decision" key={decision.id}>
          <strong>{t(decisionLabels[decision.decision] ?? decision.decision)}</strong> · {dateTime(decision.created_at)}
          {decision.reason && <p className="prose-output">{decision.reason}</p>}
        </div>) : <p className="muted">{t('Для этой версии решений пока нет.')}</p>}
      </article>)}</div>
    </>}
  </section>
}

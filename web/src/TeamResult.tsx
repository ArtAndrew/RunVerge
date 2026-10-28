// SPDX-License-Identifier: Elastic-2.0
import {t, getIntlLocale} from './i18n'
import {useEffect, useMemo, useState} from 'react'
import type {Artifact, Task} from './api'
import {exactTextBytes, parseTeamResult, teamMemberLabels, teamPatternLabels, teamResultLimits} from './teamManifest'
import './TeamResult.css'

export function isTeamResultArtifact(artifact: Artifact): boolean {
  return artifact.name === 'team-result.json' && artifact.media_type === 'application/json' && artifact.provenance?.adapter === 'team:coordinator'
}

function downloadExact(content: string, name: string) {
  const url = URL.createObjectURL(new Blob([content], {type: 'text/plain;charset=utf-8'}))
  const link = document.createElement('a')
  link.href = url; link.download = name.replace(/[\/\\\u0000-\u001f]/g, '_') || 'material.txt'; link.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

function ExactText({content, label, name, artifact = false}: {content: string; label: string; name: string; artifact?: boolean}) {
  const [open, setOpen] = useState(false)
  useEffect(() => setOpen(false), [content])
  const bytes = exactTextBytes(content), renderable = bytes <= teamResultLimits.bytes
  return <details className="team-exact" open={open} onToggle={event => setOpen(event.currentTarget.open)}>
    <summary>{label}<span>{bytes.toLocaleString(getIntlLocale())}{t(" байт")}</span></summary>
    {open && <div className="team-exact-body"><p className="muted">{t("Точный текст без преобразования. Содержимое рассматривается как цитата; команды и ссылки из него не выполняются.")}</p>
      {renderable ? <pre className="prose-output team-exact-text" data-team-exact={artifact ? 'artifact' : 'source'}>{content}</pre> : <p role="status">{t("Материал превышает предел отображения 256 КиБ. Полный исходный текст доступен в файле.")}</p>}
      <button type="button" className="button button-small button-secondary" onClick={() => downloadExact(content, name)}>{t("Скачать точный текст")}</button>
    </div>}
  </details>
}

export default function TeamResult({artifact, children = [], onOpenTask, contentDigest}: {artifact: Artifact; children?: Task[]; onOpenTask?: (id: string) => void; contentDigest?: string}) {
  const parsed = useMemo(() => parseTeamResult(artifact.content), [artifact.content])
  const [computedDigest, setComputedDigest] = useState('')
  useEffect(() => {
    let active = true; setComputedDigest('')
    if (exactTextBytes(artifact.content) <= teamResultLimits.bytes) void crypto.subtle.digest('SHA-256', new TextEncoder().encode(artifact.content)).then(bytes => {
      if (active) setComputedDigest([...new Uint8Array(bytes)].map(v => v.toString(16).padStart(2, '0')).join(''))
    }).catch(() => { /* No integrity claim when local hashing is unavailable. */ })
    return () => {active = false}
  }, [artifact.content])
  const manifest = parsed.ok ? parsed.manifest : null
  return <section className="team-result" aria-label={t("Обзор результата команды")}>
    {manifest ? <>
      <div className="team-result-heading"><h3>{manifest.complete ? t("Материалы всей команды") : t("Частичный результат команды")}</h3><span className={`team-result-completeness ${manifest.complete ? '' : 'partial'}`}>{manifest.members.filter(v => v.result).length}{t(" из ")}{manifest.members.length}{t(" включено")}</span></div>
      <p className="team-result-explanation">{t(teamPatternLabels[manifest.pattern])} · {manifest.join_policy === 'all' ? t("Нужны материалы всех участников") : t("Допускается частичный результат")}</p>
      {!manifest.complete && <p className="notice notice-warning">{t("В общий результат включены не все участники. Причины пропусков указаны ниже; пропущенные материалы не считаются принятыми.")}</p>}
      <p className="team-result-caution">{t("Это обзор сохранённых материалов. Наличие текста или ответ проверяющего агента не подтверждают фактическую точность. Решение относится к точному исходному артефакту.")}</p>
      {manifest.all_fixture && <p className="aside-note">{t("Все участники — тестовые исполнители. Их тексты и расходы синтетические.")}</p>}
      <ol className="team-result-members">{manifest.members.map((member, index) => {
        const child = children.find(task => task.id === member.task_id && task.organization_id === artifact.organization_id && task.parent_id === artifact.task_id)
        return <li className="team-result-member" key={member.id}>
          <div className="team-member-heading"><span className="team-member-number">{index + 1}</span><div><h4>{member.id}</h4>{child && <p>{child.goal}</p>}</div><span className={`team-member-state ${member.result ? 'included' : 'omitted'}`}>{t(teamMemberLabels[member.status])}</span></div>
          {member.result ? <>
            <p className="team-source-caption">{t("Один закреплённый источник · материал v")}{member.result.artifact_version}</p>
            <ExactText content={member.result.content} label={t("Открыть точный текст участника")} name={`${member.id}-v${member.result.artifact_version}.txt`}/>
            <details className="team-source-metadata"><summary>{t("Версия и ссылки на источник")}</summary><dl className="technical-ids"><div><dt>{t("Материал")}</dt><dd>{member.result.artifact_id} · v{member.result.artifact_version}</dd></div><div><dt>{t("Черновик")}</dt><dd>{member.result.candidate_id}</dd></div><div><dt>{t("Попытка")}</dt><dd>{member.run_id}</dd></div><div><dt>{t("SHA-256 из артефакта")}</dt><dd>{member.result.content_digest}</dd></div></dl></details>
          </> : <p className="team-member-omission">{t("Материал этого участника не включён в общий результат.")}</p>}
          {onOpenTask && (child ? <button type="button" className="button button-small button-secondary" onClick={() => onOpenTask(child.id)}>{t("Открыть задачу участника")}</button> : <p className="muted team-child-unavailable">{t("Задача участника не загружена в текущем списке. Её доступность проверяет сервер.")}</p>)}
        </li>
      })}</ol>
    </> : <div className="notice notice-warning"><div><strong>{t("Не удалось построить обзор команды")}</strong><p>{parsed.ok ? '' : parsed.reason === 'size' ? t("Превышен предел размера материала.") : parsed.reason === 'depth' || parsed.reason === 'count' ? t("Структура материала превышает предел отображения.") : t("Формат не соответствует поддерживаемой версии командного результата.")}{t(" Исходный материал сохранён и доступен ниже; обзор не заменяет его.")}</p></div></div>}
    <details className="team-source-metadata"><summary>{t("Исходный артефакт и его контрольная сумма")}</summary><dl className="technical-ids"><div><dt>{t("Материал")}</dt><dd>{artifact.name} · v{artifact.version}</dd></div><div><dt>{t("Идентификатор")}</dt><dd>{artifact.id}</dd></div><div><dt>{t("SHA-256 точного текста")}</dt><dd>{computedDigest || t("Не вычислена")}</dd></div>{contentDigest && <div><dt>{t("SHA-256 выбранного черновика")}</dt><dd>{contentDigest}</dd></div>}</dl><p className="muted">{t("Совпадение контрольных сумм связывает решение с байтами материала и не проверяет достоверность его содержания.")}</p></details>
    <ExactText content={artifact.content} label={t("Открыть весь исходный артефакт")} name={artifact.name} artifact/>
  </section>
}

// SPDX-License-Identifier: Elastic-2.0
import {useMemo} from 'react'
import {checkCIReferences} from './ciReferences'
import {t} from './i18n'

export default function CIReferenceCheck({goal, draft, onRepair}: {
  goal: string
  draft: string
  onRepair?: (draft: string) => void
}) {
  const report = useMemo(() => checkCIReferences(goal, draft), [goal, draft])
  if (!report) return null
  const repair = report.status === 'unavailable' ? undefined : report.repair
  const message = {
    unavailable: 'Не удалось разобрать адрес запуска и строки исходного журнала.',
    missing: 'В черновике нет ссылок на строки исходного журнала.',
    issues: 'Некоторые ссылки неполные или не совпадают со строками исходного журнала.',
    matched: 'Ссылки совпадают со строками исходного журнала.',
  }[report.status]
  return <section className={`notice notice-${report.status === 'matched' ? 'info' : 'warning'} ci-reference-check`} aria-label={t('Ссылки на журнал CI')}>
    <div>
      <strong>{t('Ссылки на журнал CI')}</strong>
      <p>{t(message)}</p>
      {report.status !== 'unavailable' && report.references.length > 0 &&
        <details>
          <summary>{t('Ссылки и строки журнала')}</summary>
          <ul>{report.references.map(ref => <li key={ref.text}>
            <code>{ref.text}</code>
            {ref.lines.length > 0 ? <pre>{ref.lines.map(line => `L${line.number} ${line.text}`).join('\n')}</pre> : <p>{t('Совпадения нет')}</p>}
          </li>)}</ul>
        </details>}
      {repair && onRepair &&
        <details>
          <summary>{t('Предложенные исправления ссылок')}</summary>
          <ul>{repair.replacements.map(item => <li key={item.before}>
            <code>{item.before}</code> → <code>{item.after}</code>
          </li>)}</ul>
          <button type="button" className="button button-small button-secondary"
            onClick={() => onRepair(repair.draft)}>{t('Дополнить ссылки в черновике')}</button>
        </details>}
      <p>{t('Сверьте вывод с журналом. Проверка ссылок не подтверждает вывод.')}</p>
    </div>
  </section>
}

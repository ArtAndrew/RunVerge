// SPDX-License-Identifier: Elastic-2.0
import {t} from './i18n'
import type {Agent, CurrentIdentity} from './api'
import './FirstTaskGuide.css'

export default function FirstTaskGuide({agents, moreAgents, identity, onCreateAgent, onAgents, onTask}: {
  agents: Agent[]; moreAgents: boolean; identity: CurrentIdentity | null
  onCreateAgent: () => void; onAgents: () => void; onTask: () => void
}) {
  if (!identity?.permissions.includes('tasks.create')) return null
  const published = agents.some(agent => agent.published)
  const hasAgents = agents.length > 0 || moreAgents
  const needsAdministrator = !published && !identity.permissions.includes('agents.write')
  const step = published ? 3 : hasAgents ? 2 : 1
  return <section className="panel first-task-guide" aria-labelledby="first-task-title">
    <h2 id="first-task-title">{t('От подключения к первому результату')}</h2>
    <p>{t('Агент хранит инструкции и выбранную модель. Задача — конкретная работа для него.')}</p>
    <ol>
      <li aria-current={step === 1 ? 'step' : undefined}><strong>{t('Создайте агента')}</strong><span>{t('Возьмите заготовку инструкций или напишите свои. Выберите подключённую модель и лимит.')}</span></li>
      <li aria-current={step === 2 ? 'step' : undefined}><strong>{t('Опубликуйте версию')}</strong><span>{t('Проверьте настройки на карточке агента. Публикация делает его доступным для задач, но не запускает модель.')}</span></li>
      <li aria-current={step === 3 ? 'step' : undefined}><strong>{t('Дайте задачу и проверьте ответ')}</strong><span>{t('Начните с учебного примера. Сверьте ответ с исходными фактами; при необходимости запросите доработку.')}</span></li>
    </ol>
    <button className="button button-primary" type="button"
      disabled={needsAdministrator}
      onClick={published ? onTask : hasAgents ? onAgents : onCreateAgent}>
      {published ? t('Перейти к первой задаче') : hasAgents ? t('Открыть агентов для публикации') : t('Настроить первого агента')}
    </button>
    {needsAdministrator && <p>{t('Попросите администратора подготовить и опубликовать агента.')}</p>}
    <p className="muted">{t('Используйте учебные данные. Полное восстановление рабочих данных пока не поддерживается.')}</p>
  </section>
}

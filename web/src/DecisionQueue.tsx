// SPDX-License-Identifier: Elastic-2.0
import type {Task} from './api'
import {t} from './i18n'
import {decisionTab, waitingDescription, type DecisionTab} from './taskAttention'
import {dateTime} from './format'
import {BudgetBar, Icon, Status} from './ui'

const tabLabels: Record<DecisionTab, string> = {
  result: 'Результат',
  actions: 'Действия',
  events: 'Ход работы',
}

export default function DecisionQueue({tasks, onOpen}: {
  tasks: Task[]
  onOpen: (id: string, tab: DecisionTab) => void
}) {
  return <ul className="decision-queue" aria-label={t('Нужно внимание')}>
    {tasks.map(task => {
      const tab = decisionTab(task)
      if (!tab) return null
      return <li key={task.id} className="decision-card">
        <div className="decision-copy">
          <div className="decision-meta"><Status status={task.status} />
            <time dateTime={task.created_at}>{dateTime(task.created_at)}</time>
          </div>
          <h2>{task.goal}</h2>
          <p>{t(waitingDescription(task.waiting_reason))}</p>
        </div>
        <div className="decision-controls">
          <BudgetBar budget={task.budget_microusd} spent={task.spent_microusd} reserved={task.reserved_microusd} />
          <button className="button button-secondary button-small"
            aria-label={`${t(tabLabels[tab])} · ${t('Открыть задачу: {value0}', {value0: task.goal})}`}
            onClick={() => onOpen(task.id, tab)}>
            {t(tabLabels[tab])}<Icon name="arrow" />
          </button>
        </div>
      </li>
    })}
  </ul>
}

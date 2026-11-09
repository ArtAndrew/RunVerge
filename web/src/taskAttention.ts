// SPDX-License-Identifier: Elastic-2.0
import type {Task} from './api'

const waitingLabels: Record<string, string> = {
  model_candidate_pending: 'Подготовлен черновик. Проверьте его во вкладке «Результат» и сохраните решение.',
  material_candidate_pending: 'Предложен отредактированный материал. Проверьте точный текст и исходную основу перед решением.',
  team_candidate_pending: 'Команда подготовила общий черновик. Проверьте материалы участников и сохраните решение.',
  candidate_changes_requested: 'Запрошена доработка. Чтобы запустить её, создайте новую попытку с указаниями исполнителю и подтверждением расходов.',
  candidate_rejected: 'Результат отклонён. Решение сохранено; повторный запуск этой задачи недоступен.',
  model_outcome_unknown: 'Исход обращения к модели неизвестен. Резерв сохранён; повторная отправка остановлена до сверки.',
  model_accounting_pending: 'Ожидается подтверждение расходов. Резерв сохранён.',
  model_output_rejected: 'Ответ модели не прошёл проверку допустимого формата или размера. Расход требует сверки.',
  model_tools_pending: 'Модель предложила действия с инструментами. Откройте «Действия»: проверьте каждое предложение, затем отдельно продолжите модель с результатами всего пакета.',
  model_turn_limit: 'Достигнут предел шагов модели. Выполнение остановлено для проверки.',
  model_repeated_intent: 'Модель повторила одинаковое действие. Выполнение остановлено для проверки.',
  model_input_rejected: 'Задание превышает ограничения выбранной модели. Создайте новую задачу с меньшим объёмом входных данных.',
  model_credentials_unavailable: 'Не удалось получить доступ к модели. Администратору нужно проверить её подключение.',
  model_profile_unavailable: 'Закреплённая версия подключения модели недоступна. Администратору нужно проверить настройки.',
  model_attempt_requires_attention: 'Шаг модели не был отправлен и требует проверки. Новую попытку можно запустить отдельно с подтверждением расходов.',
  automation_authorization_revoked: 'Автоматическая работа остановлена из-за изменения прав. Проверьте доступ; новая попытка требует отдельного разрешения и подтверждения расходов.',
  authorization_revoked: 'Доступ к выполнению отозван. Обратитесь к администратору организации.',
  tool_approval_pending: 'Подготовлено внешнее действие. Откройте «Действия» и проверьте точные аргументы перед решением.',
  tool_dispatch_pending: 'Действие разрешено, но ещё не отправлено. Для запуска откройте «Действия» и подтвердите внешний эффект и расходы.',
  tool_execution_pending: 'Действие передано исполнителю. Его исход появится во вкладке «Действия».',
  external_action_unknown: 'Исход внешнего действия неизвестен. Откройте «Действия» для сверки. Повторная отправка остановлена.',
  budget_exceeded: 'Доступного бюджета недостаточно для продолжения.',
  attempt_limit: 'Достигнут предел попыток. Выполнение остановлено для проверки.',
}

export type DecisionTab = 'result' | 'actions' | 'events'

export function waitingDescription(reason?: string) {
  return reason && Object.hasOwn(waitingLabels, reason)
    ? waitingLabels[reason]
    : 'Выполнение приостановлено. Требуется проверить причину на сервере.'
}

const actionReasons = new Set(['model_tools_pending', 'tool_approval_pending', 'tool_dispatch_pending', 'external_action_unknown'])
const resultReasons = new Set(['model_candidate_pending', 'material_candidate_pending', 'team_candidate_pending', 'candidate_changes_requested'])
const teamDraftReasons = new Set(['model_candidate_pending', 'material_candidate_pending', 'team_candidate_pending'])

export function decisionTab(task: Pick<Task, 'status' | 'waiting_reason' | 'parent_id'>): DecisionTab | null {
  const reason = task.waiting_reason ?? ''
  if (!['waiting_input', 'waiting_approval', 'blocked'].includes(task.status) ||
      reason === 'tool_execution_pending' || reason === 'team_dependency_pending' ||
      (task.parent_id && task.status === 'waiting_input' && teamDraftReasons.has(reason))) return null
  if (actionReasons.has(reason)) return 'actions'
  if (resultReasons.has(reason)) return 'result'
  return 'events'
}

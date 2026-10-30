// SPDX-License-Identifier: Elastic-2.0
import type {TeamDefinition} from './api'
import {t} from './i18n'

// An editable draft only. Agent versions remain an explicit user choice.
export const teamReviewStarter = {
  name: 'Черновик и проверка',
  instructions: 'Два этапа: подготовить черновик по исходным материалам, затем проверить его. Сохранить оба результата для решения человека. Проверка агентом не означает принятие результата.',
  draft: 'Подготовь черновик по исходной цели задачи и предоставленным материалам. Сохрани имена, числа и сроки. Не добавляй отсутствующие факты. Перечисли вопросы, если данных недостаточно.',
  review: 'Сравни черновик предыдущего участника с исходной целью задачи и материалами. Укажи пропущенные требования, неподтверждённые утверждения и ошибки в числах или сроках. Для каждого замечания приведи фрагмент и предложи исправление. Если замечаний нет, напиши об этом. Не утверждай, что проверил внешние источники; окончательное решение принимает человек.',
} as const

export function createReviewTeam(): TeamDefinition {
  return {
    pattern: 'delegate_verify', join_policy: 'all', max_depth: 1, max_children: 2, max_parallel: 1,
    members: [
      {id: 'writer', agent_id: '', agent_version: 1, task: t(teamReviewStarter.draft)},
      {id: 'reviewer', agent_id: '', agent_version: 1, task: t(teamReviewStarter.review)},
    ],
  }
}

export function isEmptyTeamDraft(name: string, instructions: string, team: TeamDefinition): boolean {
  return name === '' && instructions === '' && team.pattern === 'sequence' && team.join_policy === 'all' &&
    team.max_depth === 1 && team.max_children === 2 && team.max_parallel === 1 && team.members.length === 2 &&
    team.members.every((member, i) => member.id === `member-${i + 1}` && member.agent_id === '' && member.task === undefined)
}

// SPDX-License-Identifier: Elastic-2.0
import {t} from './i18n.ts'
import type {AutomationToolPin, TaskAutomationState, ModelToolDefinition, ToolInfo, ProgressSignal} from '../../packages/typescript-sdk/src/types'
import {parseBudget} from './format.ts'

export interface CreateAutomation {
  expected_task_version:number;mode:'read_only'|'prepare_and_continue';tool_pins:AutomationToolPin[];
  max_model_turns:number;max_tool_executions:number;max_charge_microusd:string;expires_in_seconds:number;
  acknowledge_cost:true;reason:string;
}
export interface RevokeAutomation {grant_id:string;expected_version:number;reason:string}
const object=(v:unknown):v is Record<string,unknown>=>Boolean(v&&typeof v==='object'&&!Array.isArray(v))
const integer=(v:unknown,min:number,max=Number.MAX_SAFE_INTEGER):v is number=>typeof v==='number'&&Number.isSafeInteger(v)&&v>=min&&v<=max
const exactMoney=(v:unknown,max=9223372036854775807n,min=0n)=>typeof v==='string'&&v.length<=19&&/^(?:0|[1-9][0-9]*)(?![\s\S])/.test(v)&&BigInt(v)>=min&&BigInt(v)<=max
export const validProgressSignal=(v:unknown):v is ProgressSignal=>object(v)&&typeof v.id==='string'&&Boolean(v.id)&&typeof v.organization_id==='string'&&typeof v.task_id==='string'&&typeof v.code==='string'&&typeof v.detector_version==='string'&&typeof v.created_at==='string'&&typeof v.snapshot_digest==='string'&&/^[0-9a-f]{64}$/.test(v.snapshot_digest)&&Array.isArray(v.evidence_ids)&&v.evidence_ids.length<=512&&v.evidence_ids.every(id=>typeof id==='string')
export function validAutomationState(v:unknown):v is TaskAutomationState {
  if(!object(v)||!object(v.usage)||!integer(v.usage.model_turns,0)||!integer(v.usage.tool_executions,0)||!exactMoney(v.usage.allocated_microusd)||!Array.isArray(v.available_commands)||!v.available_commands.every(c=>typeof c==='string')||!Array.isArray(v.signals)||!v.signals.every(validProgressSignal))return false
  if(v.grant!==undefined&&(!object(v.grant)||typeof v.grant.id!=='string'||!v.grant.id||!integer(v.grant.version,1)||typeof v.grant.status!=='string'||!['read_only','prepare_and_continue'].includes(String(v.grant.mode))||!exactMoney(v.grant.max_charge_microusd,1_000_000_000_000n,1n)||!integer(v.grant.max_model_turns,1,31)||!integer(v.grant.max_tool_executions,1,32)||!Array.isArray(v.grant.tool_pins)))return false
  return true
}
export function eligibleAutomationTools(pins:readonly ModelToolDefinition[],tools:readonly ToolInfo[],mode:CreateAutomation['mode']) {
  return pins.filter(pin=>{const tool=tools.find(t=>t.id===pin.tool_id&&t.version===pin.tool_version&&t.active);return tool&&(mode==='prepare_and_continue'||tool.effect==='read'&&tool.automation_allowed===true&&!tool.require_distinct_approver)})
}
export function automationPayload(taskVersion:number,mode:CreateAutomation['mode'],selected:readonly ModelToolDefinition[],eligible:readonly ModelToolDefinition[],modelTurns:string,executions:string,chargeUsd:string,ttl:string,reason:string,ack:boolean):CreateAutomation {
  if(!ack)throw new Error('Подтвердите разрешение на новые расходы.')
  const bounded=(value:string,min:number,max:number)=>{if(!/^[1-9][0-9]*$/.test(value)||!integer(Number(value),min,max))throw new Error(t('Укажите целое число от {min} до {max}.', {min,max}));return Number(value)}
  if(!integer(taskVersion,1)||!['read_only','prepare_and_continue'].includes(mode)||selected.length<1||selected.length>16||new Set(selected.map(pin=>pin.tool_id)).size!==selected.length||selected.some(pin=>!eligible.some(e=>e.tool_id===pin.tool_id&&e.tool_version===pin.tool_version&&e.tool_profile_digest===pin.tool_profile_digest)))throw new Error('Выберите точные разрешённые инструменты закреплённого агента.')
  if(!reason.trim()||new TextEncoder().encode(reason).length>4096||reason.includes('\0'))throw new Error('Укажите причину до 4096 байт.')
  for(const c of reason){const n=c.codePointAt(0)!;if(n>=0xd800&&n<=0xdfff)throw new Error('Причина содержит некорректный символ.')}
  return {expected_task_version:taskVersion,mode,tool_pins:selected.map(p=>({tool_id:p.tool_id,tool_version:p.tool_version,tool_profile_digest:p.tool_profile_digest})),max_model_turns:bounded(modelTurns,1,31),max_tool_executions:bounded(executions,1,32),max_charge_microusd:String(parseBudget(chargeUsd)),expires_in_seconds:bounded(ttl,1,86400),acknowledge_cost:true,reason}
}
export const automationModeLabels={read_only:'Самостоятельное чтение',prepare_and_continue:'Подготовка с вашим согласованием'}
export const automationStatusLabels:Record<string,string>={active:'Действует',revoked:'Отозвано',stopped:'Остановлено',expired:'Срок истёк',completed:'Автоматические шаги завершены'}
export const progressSignalLabels:Record<string,string>={
  unknown_effect:'Исход отправленного действия неизвестен. Нужна сверка; повторная отправка остановлена.',
  repeated_effect_intent:'Предложено повторить уже выполненное изменяющее действие. Нужна проверка.',
  repeated_intent:'Несколько раз предложено одинаковое действие без подтверждённого прогресса.',
  repeated_failed_outcome:'Повторяется тот же неудачный исход. Нужна проверка дальнейшего плана.',
  alternating_intents:'Действия повторяются по кругу. Автоматическое продолжение остановлено.',
  identical_unverified_output:'Повторяется одинаковый неподтверждённый результат. Нужна проверка.',
  duplicate_batch_intent:'В одном пакете предложены одинаковые действия. Пакет остановлен до выполнения.',
  repeated_batch_intent:'В одном пакете предложены одинаковые действия. Пакет остановлен до выполнения.',
  grant_expired:'Срок разрешения истёк.',grant_revoked:'Разрешение отозвано.',
  automation_limit:'Достигнут один из конечных пределов автоматизации.',
  final_candidate_requires_review:'Автоматические шаги завершены. Итоговый материал ожидает отдельного решения пользователя.',
  automation_authorization_revoked:'Текущих прав инициатора или владельца больше недостаточно. Автоматическая работа остановлена.',
  task_requires_attention:'Задача остановлена или требует внимания. Автоматическая работа приостановлена.',
  tool_outside_automation_grant:'Модель запросила инструмент вне точного разрешённого набора. Весь пакет остановлен.',
  tool_profile_unavailable:'Закреплённая версия инструмента недоступна. Администратору нужно проверить подключение.',
  existing_action_requires_review:'Существующее действие требует отдельной проверки; автоматическое разрешение к нему не применяется.',
  operator_assertion_requires_review:'Исход записан утверждением оператора. Для продолжения нужна отдельная проверка.',
  unperformed_action_requires_review:'Одно из действий не выполнено. Автоматическая работа остановлена для проверки.',
  proposal_batch_unavailable:'Не удалось проверить полный пакет предложений. Новые действия не допускаются.',
  progress_history_unavailable:'История для проверки прогресса недоступна или неполна. Автоматическая работа остановлена.',
  progress_guard_unavailable:'Не удалось выполнить проверку прогресса. Автоматическая работа остановлена.',
  automation_admission_budget_exceeded:'Недостаточно бюджета или достигнут общий предел выделенных средств.',
  automation_admission_invalid_transition:'Продолжение не соответствует текущему состоянию или конечным пределам задачи.',
  automation_admission_forbidden:'Доступ к дальнейшей автоматической работе больше не разрешён.',
}

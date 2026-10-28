import {t} from './i18n.ts'
import type {CurrentIdentity, Artifact, Task, TaskCandidate, ReviewDecision, ReviewTask, RetryTask} from './api'

export const reviewLabels: Record<ReviewDecision,string> = {accept:'Принять результат',reject:'Отклонить и завершить',request_changes:'Запросить доработку'}
export const decisionLabels: Record<ReviewDecision,string> = {accept:'Принято',reject:'Отклонено',request_changes:'Запрошена доработка'}
export interface ReviewSnapshot {task:Task;candidate:TaskCandidate;artifact:Artifact}
export function mayReviewTask(identity:CurrentIdentity|null,task:Task):boolean {
 return Boolean(identity&&identity.principal_kind==='human'&&identity.organization_id===task.organization_id&&(identity.permissions.includes('tasks.review_any')||(identity.principal_id===task.owner_id&&identity.permissions.includes('tasks.review_own'))))
}
export function reviewDecisions(source:string):ReviewDecision[]{return source==='model_tools'?['request_changes','reject']:['accept','request_changes','reject']}

export function freshExecutionMetadata(task:Task):boolean {
  return Boolean(task.current_run_id && Number.isSafeInteger(task.attempt_count) && task.attempt_count!>=1 && Number.isSafeInteger(task.max_runs) && task.max_runs!>=1 && task.max_runs!<=10 && task.attempt_count!<=task.max_runs! && typeof task.execution_deadline==='string' && Number.isFinite(Date.parse(task.execution_deadline)))
}

export async function bindCandidate(task:Task,candidates:TaskCandidate[],artifacts:Artifact[]):Promise<ReviewSnapshot|null> {
  if (!task.candidate_id) return null
  if (!freshExecutionMetadata(task)) throw new Error('Обновите задачу: не получены актуальные сведения о попытке и лимитах.')
  const candidate=candidates.find(value=>value.id===task.candidate_id)
  if (!candidate || candidate.task_id!==task.id || candidate.organization_id!==task.organization_id || candidate.run_id!==task.current_run_id) throw new Error('Не получен черновик текущей попытки. Обновите данные перед решением.')
  const artifact=artifacts.find(value=>value.id===candidate.artifact_id && value.version===candidate.artifact_version)
  if (!artifact || artifact.task_id!==task.id || artifact.organization_id!==task.organization_id || typeof artifact.content!=='string') throw new Error('Не получена точная версия материала. Обновите данные перед решением.')
  if (!/^[0-9a-f]{64}(?![\s\S])/.test(candidate.content_digest)) throw new Error('Некорректная контрольная сумма материала.')
  const bytes=await crypto.subtle.digest('SHA-256',new TextEncoder().encode(artifact.content))
  const digest=[...new Uint8Array(bytes)].map(value=>value.toString(16).padStart(2,'0')).join('')
  if (digest!==candidate.content_digest) throw new Error('Текст материала не совпал с сохранённой контрольной суммой. Решение заблокировано; обновите данные.')
  return {task:{...task},candidate:{...candidate},artifact:{...artifact}}
}

export function reviewPayload(snapshot:ReviewSnapshot,decision:ReviewDecision,reason:string):ReviewTask {
  if (!reviewDecisions(snapshot.candidate.source).includes(decision)) throw new Error('Этот материал не допускает выбранного решения.')
  validateText(reason,4096,'Комментарий')
  if (decision!=='accept' && !reason.trim()) throw new Error('Укажите причину решения.')
  return {expected_version:snapshot.task.version,candidate_id:snapshot.candidate.id,artifact_id:snapshot.artifact.id,artifact_version:snapshot.artifact.version,content_digest:snapshot.candidate.content_digest,decision,...(reason ? {reason}: {})}
}

export function validateText(value:string,maxBytes:number,label:string) {
  if (value.includes('\0') || [...value].some(character=>{const cp=character.codePointAt(0)!;return cp>=0xd800&&cp<=0xdfff})) throw new Error(t('{label}: недопустимый символ.', {label:t(label)}))
  if (new TextEncoder().encode(value).length>maxBytes) throw new Error(t('{label}: максимум {maxBytes} байт UTF-8.', {label:t(label),maxBytes}))
}

export function retryBlockReason(task:Task,now=Date.now()):string|null {
  if (!freshExecutionMetadata(task)) return 'Обновите задачу, чтобы получить действующие лимиты.'
  if (task.parent_id) return 'Новые попытки запускаются только для корневой задачи.'
  if (task.status!=='blocked') return 'Новая попытка доступна для приостановленной задачи.'
  if (!new Set(['candidate_changes_requested','model_output_rejected','model_outcome_unknown','model_accounting_pending','model_credentials_unavailable','model_profile_unavailable','model_input_rejected','model_attempt_requires_attention','automation_authorization_revoked','fixture_input_too_large','attempt_limit','team_member_failed','team_input_too_large','team_output_too_large']).has(task.waiting_reason??'')) return 'Причина остановки не допускает новую попытку.'
  if (task.financial_hold) return 'Новые расходы заблокированы до финансовой сверки.'
  if (task.attempt_count!>=task.max_runs!) return 'Достигнут предел попыток.'
  if (Date.parse(task.execution_deadline!)<=now) return 'Истёк срок выполнения задачи.'
  return null
}

export function retryPayload(task:Task,reason:string,feedback:string,acknowledged:boolean,now=Date.now()):RetryTask {
  const blocked=retryBlockReason(task,now);if(blocked)throw new Error(blocked)
  if(!acknowledged)throw new Error('Подтвердите возможные новые расходы.')
  if(!reason.trim())throw new Error('Укажите причину новой попытки.')
  if(task.waiting_reason==='candidate_changes_requested'&&!feedback.trim())throw new Error('Опишите исполнителю необходимые изменения.')
  validateText(reason,4096,'Причина');validateText(feedback,8192,'Указания исполнителю')
  return {expected_version:task.version,reason,acknowledge_new_cost:true,...(feedback?{feedback}: {})}
}

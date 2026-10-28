// SPDX-License-Identifier: Elastic-2.0
import {t} from './i18n.ts'
import type {CreateMaterial,Material,MaterialContent,PublishMaterial,ReviseMaterial,Task} from './api'
import {freshExecutionMetadata,validateText} from './review.ts'

export const materialLimits={content:262144,name:120,reason:4096,versions:128} as const
export interface MaterialPublishSnapshot {task:Task;material:Material;content:MaterialContent}
const digestPattern=/^[0-9a-f]{64}(?![\s\S])/
function version(value:number,max=Number.MAX_SAFE_INTEGER){if(!Number.isSafeInteger(value)||value<1||value>max)throw new Error('Некорректная версия материала.')}
function nonblank(value:string,max:number,label:string){if(typeof value!=='string'||!value.trim())throw new Error(t('{label}: заполните поле.', {label:t(label)}));validateText(value,max,label);return value}
export async function textDigest(content:string):Promise<string>{
  if(typeof content!=='string')throw new Error('Не получен точный текст материала.')
  validateText(content,materialLimits.content,'Текст')
  return [...new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(content)))].map(v=>v.toString(16).padStart(2,'0')).join('')
}
export function createMaterialPayload(input:CreateMaterial):CreateMaterial {
  nonblank(input.name,materialLimits.name,'Название');validateText(input.content,materialLimits.content,'Текст')
  if(!['text/plain','text/markdown','application/json'].includes(input.media_type))throw new Error('Недопустимый формат материала.')
  if(input.base_candidate_id!==undefined)nonblank(input.base_candidate_id,128,'Исходный черновик')
  if(input.reason!==undefined)validateText(input.reason,materialLimits.reason,'Комментарий')
  return {...input}
}
export function reviseMaterialPayload(head:Material,content:string,reason:string,mergeBase?:number):ReviseMaterial {
  version(head.head_version,materialLimits.versions);validateText(content,materialLimits.content,'Текст');nonblank(reason,materialLimits.reason,'Причина изменения')
  if(mergeBase!==undefined)version(mergeBase,head.head_version)
  return {expected_version:head.head_version,content,reason,...(mergeBase===undefined?{}:{merge_base_version:mergeBase})}
}
export async function bindMaterialContent(task:Task,head:Material,body:MaterialContent):Promise<MaterialContent>{
  nonblank(head.id,128,'Материал');version(head.head_version,materialLimits.versions)
  const revision=body?.revision
  if(head.task_id!==task.id||head.organization_id!==task.organization_id||!revision||revision.task_id!==task.id||revision.organization_id!==task.organization_id||revision.material_id!==head.id||revision.version!==head.head_version||!digestPattern.test(head.head_content_digest)||revision.content_digest!==head.head_content_digest)throw new Error('Не получена точная версия материала этой задачи.')
  if(await textDigest(body.content)!==revision.content_digest)throw new Error('Контрольная сумма материала не совпала. Публикация заблокирована.')
  return {revision:{...revision},content:body.content}
}
export function publicationBlockReason(task:Task):string|null {
  if(task.waiting_reason==='model_tools_pending')return 'Сначала завершите предложения инструментов и получите итоговый ответ модели. Этот пакет действий нельзя заменить итоговым материалом.'
  if(!freshExecutionMetadata(task)||!task.candidate_id)return 'Обновите задачу: нужен текущий черновик и сведения о попытке.'
  if(task.status!=='waiting_input')return 'Предложить материал можно только пока задача ожидает решения. Сохранение черновика доступно отдельно.'
  return null
}
export async function bindMaterialPublication(task:Task,head:Material,body:MaterialContent):Promise<MaterialPublishSnapshot>{
  const blocked=publicationBlockReason(task);if(blocked)throw new Error(blocked)
  version(task.version);nonblank(task.candidate_id!,128,'Текущий черновик')
  const verified=await bindMaterialContent(task,head,body)
  if(!verified.content.trim())throw new Error('Перед публикацией добавьте непустой итоговый текст.')
  if(head.media_type==='application/json'){try{JSON.parse(verified.content)}catch{throw new Error('Для публикации JSON-материала исправьте его синтаксис. Черновик можно сохранить отдельно.')}}
  return {task:{...task},material:{...head},content:verified}
}
export function publishMaterialPayload(snapshot:MaterialPublishSnapshot):PublishMaterial {
  return {expected_task_version:snapshot.task.version,expected_material_version:snapshot.material.head_version,content_digest:snapshot.material.head_content_digest,parent_candidate_id:snapshot.task.candidate_id!}
}

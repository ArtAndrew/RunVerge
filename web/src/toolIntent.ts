// SPDX-License-Identifier: Elastic-2.0
import {t} from './i18n.ts'
import type {Task, ToolInfo, ToolExecutionView, PrepareTool, DecideTool, DispatchTool, ReconcileTool} from './api'
import {validateText, type ReviewSnapshot} from './review.ts'

export const toolStatusLabels:Record<string,string>={awaiting_approval:'Ожидает решения',approved:'Разрешено, ещё не запущено',queued:'В очереди',leased:'Получено исполнителем',sent:'Отправлено',unknown:'Исход неизвестен',succeeded:'Получен ответ об успехе',rejected:'Получен отказ',denied:'Не разрешено',expired:'Срок истёк',cancelled:'Отменено'}
export const effectLabels:Record<string,string>={read:'Чтение',write:'Изменение данных',destructive:'Удаление или необратимое изменение'}
const sha=/^[0-9a-f]{64}(?![\s\S])/
const positive=(n:number)=>Number.isSafeInteger(n)&&n>=1
function nonblank(value:string,max:number,label:string){validateText(value,max,label);if(!value.trim())throw new Error(t('{label}: заполните поле.', {label:t(label)}));return value}
function digest(value:string){if(!sha.test(value))throw new Error('Некорректная контрольная сумма.');return value}
export async function textDigest(text:string){return [...new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(text)))].map(byte=>byte.toString(16).padStart(2,'0')).join('')}
function charge(value:string){if(typeof value!=='string'||!/^(?:0|[1-9][0-9]{0,18})(?![\s\S])/.test(value)||BigInt(value)>1_000_000_000_000n)throw new Error('Не получен точный предел расходов.');return value}

// Only validate syntax. Never replace the string with JSON.stringify(JSON.parse):
// numeric lexemes beyond Number precision, whitespace and key order are binding.
export function exactArguments(value:string,max=32768){
  validateText(value,Math.min(max,32768),'Аргументы')
  const frames:{kind:string;keys:Set<string>}[]=[]
  let count=0
  for(let i=0;i<value.length;i++){
    const c=value[i]
    if(c==='"'){
      const start=i
      for(i++;i<value.length&&value[i]!=='"';i++)if(value[i]==='\\')i++
      if(i>=value.length)throw new Error('Аргументы должны быть JSON-объектом.')
      let next=i+1;while(next<value.length&&/\s/.test(value[next]))next++
      const frame=frames.at(-1)
      if(frame?.kind==='{'&&value[next]===':'){
        const key=JSON.parse(value.slice(start,i+1)) as string
        if(frame.keys.has(key))throw new Error('Повторяющиеся ключи JSON недопустимы.')
        frame.keys.add(key)
      }
    }else if(c==='{'||c==='['){frames.push({kind:c,keys:new Set()});if(frames.length>64)throw new Error('JSON слишком глубоко вложен для проверки в интерфейсе.')}else if(c==='}'||c===']'){if(frames.pop()?.kind!==(c==='}'?'{':'['))throw new Error('Некорректный JSON.')}else if(c===','&&++count>4096)throw new Error('Слишком много значений JSON для проверки в интерфейсе.')
  }
  let parsed:unknown
  try{parsed=JSON.parse(value)}catch{throw new Error('Аргументы должны быть JSON-объектом.')}
  if(!parsed||typeof parsed!=='object'||Array.isArray(parsed))throw new Error('Аргументы должны быть JSON-объектом.')
  return value
}

export async function bindTool(tool:ToolInfo,organization:string){
  if(tool.organization_id!==organization||!positive(tool.version)||!['read','write','destructive'].includes(tool.effect))throw new Error('Не получена закреплённая версия инструмента этой организации.')
  charge(tool.max_charge_microusd);validateText(tool.input_schema_json,8192,'Схема аргументов')
  if(await textDigest(tool.input_schema_json)!==digest(tool.input_schema_digest))throw new Error('Схема аргументов не совпала с контрольной суммой.')
  return {...tool}
}

export async function bindToolView(value:ToolExecutionView,taskId:string,organization:string):Promise<ToolExecutionView>{
  const {execution:e,approval:a,task:t,tool}=value
  if(!e||!a||!t||!tool||e.task_id!==taskId||t.id!==taskId||[e.organization_id,a.organization_id,t.organization_id,tool.organization_id].some(org=>org!==organization)||a.task_id!==taskId||a.id!==e.approval_id||a.action_id!==e.action_id||a.execution_id!==e.id||a.intent_hash!==e.intent_hash||tool.id!==e.tool_id||tool.version!==e.tool_version||tool.provider!==e.provider||tool.price_version!==e.price_version||tool.max_charge_microusd!==e.max_charge_microusd||![t.version,e.version,a.version,e.artifact_version].every(positive))throw new Error('Сведения о действии не совпали. Обновите точный снимок.')
  digest(e.intent_hash);digest(e.tool_profile_digest);digest(e.content_digest);charge(e.max_charge_microusd)
  if(typeof e.arguments_json!=='string')throw new Error('Не получены точные аргументы действия.')
  exactArguments(e.arguments_json)
  if(await textDigest(e.arguments_json)!==digest(e.arguments_digest))throw new Error('Аргументы не совпали с контрольной суммой. Решение заблокировано.')
  await bindTool(tool,organization)
  if(!Number.isFinite(Date.parse(e.expires_at))||!Array.isArray(value.available_commands)||!value.available_commands.every(command=>typeof command==='string'))throw new Error('Некорректный срок или список команд.')
  const r=value.receipt
  if(r&&(r.execution_id!==e.id||r.organization_id!==organization||r.intent_hash!==e.intent_hash||r.id!==e.result_receipt_id))throw new Error('Квитанция не соответствует действию.')
  return structuredClone(value)
}

export function prepareToolPayload(snapshot:ReviewSnapshot,tool:ToolInfo,args:string,seconds:number,occurrence:string):PrepareTool{
  if(snapshot.task.status!=='waiting_input'||snapshot.task.accepted_decision_id||!['model','team','material'].includes(snapshot.candidate.source))throw new Error('Действие готовится только для текущего ожидающего решения черновика.')
  if(!tool.active||tool.organization_id!==snapshot.task.organization_id)throw new Error('Инструмент недоступен.')
  if(!positive(seconds)||seconds>86400)throw new Error('Срок действия: от 1 до 86400 секунд.')
  if(!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}(?![\s\S])/.test(occurrence))throw new Error('Некорректный идентификатор намерения.')
  return {occurrence_id:occurrence,expected_task_version:snapshot.task.version,candidate_id:snapshot.candidate.id,artifact_id:snapshot.artifact.id,artifact_version:snapshot.artifact.version,content_digest:snapshot.candidate.content_digest,tool_id:tool.id,tool_version:tool.version,arguments_json:exactArguments(args,tool.max_input_bytes),expires_in_seconds:seconds}
}

export function commandAllowed(view:ToolExecutionView,command:string,now=Date.now()):boolean{
  if(!view.available_commands.includes(command))return false
  const status=view.execution.status
  if(command==='reconcile')return status==='unknown'
  if(Date.parse(view.execution.expires_at)<=now)return false
  if(command==='approve'||command==='deny')return status==='awaiting_approval'
  if(command==='dispatch')return status==='approved'
  return false
}
function commandBase(view:ToolExecutionView,command:string){if(!commandAllowed(view,command))throw new Error('Эта команда недоступна. Обновите сведения о действии.');return {expected_task_version:view.task.version,expected_execution_version:view.execution.version,intent_hash:view.execution.intent_hash}}
export function decideToolPayload(view:ToolExecutionView,decision:'approve'|'deny',reason:string):DecideTool{if(!['approve','deny'].includes(decision))throw new Error('Неизвестное решение.');return {...commandBase(view,decision),expected_approval_version:view.approval.version,decision,reason:nonblank(reason,4096,'Причина')}}
export function dispatchToolPayload(view:ToolExecutionView,ack:boolean):DispatchTool{if(!ack)throw new Error('Подтвердите возможные расходы и внешний эффект.');return {...commandBase(view,'dispatch'),expected_approval_version:view.approval.version,acknowledge_cost:true}}
export function reconcileToolPayload(view:ToolExecutionView,outcome:'succeeded'|'rejected',reference:string,result:string,evidence:string,reason:string):ReconcileTool{if(!['succeeded','rejected'].includes(outcome))throw new Error('Неизвестный исход.');validateText(result,262144,'Результат');return {...commandBase(view,'reconcile'),outcome,effect_reference:nonblank(reference,256,'Ссылка на эффект'),result,evidence_digest:digest(evidence),reason:nonblank(reason,4096,'Основание сверки')}}
export function mayPrepare(task:Task,permissions:string[],principal:string){return task.status==='waiting_input'&&Boolean(task.candidate_id)&&!task.accepted_decision_id&&(permissions.includes('actions.prepare_any')||(task.owner_id===principal&&permissions.includes('actions.prepare_own')))}

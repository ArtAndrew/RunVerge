// SPDX-License-Identifier: Elastic-2.0
import type {ContinueModel,ModelToolProposal,ModelToolResultBinding,PrepareTool,ToolExecutionView,ToolInfo} from './api'
import type {ReviewSnapshot} from './review'
import {validateText} from './review.ts'
import {exactArguments,textDigest} from './toolIntent.ts'

const digest=/^[0-9a-f]{64}(?![\s\S])/
export interface NativePreview {snapshot:ReviewSnapshot;tool:ToolInfo;payload:PrepareTool}
export function nativeSource(snapshot:ReviewSnapshot){
  if(snapshot.candidate.source!=='model_tools'||snapshot.task.status!=='waiting_input'||snapshot.task.waiting_reason!=='model_tools_pending'||snapshot.task.accepted_decision_id)throw new Error('Нужен текущий пакет предложений модели, ожидающий действий.')
  let parsed:unknown;try{parsed=JSON.parse(snapshot.artifact.content)}catch{throw new Error('Пакет предложений модели повреждён.')}
  if(!parsed||typeof parsed!=='object'||!('tool_calls' in parsed)||!Array.isArray(parsed.tool_calls)||parsed.tool_calls.length<1||parsed.tool_calls.length>8)throw new Error('Некорректный пакет вызовов модели.')
  const seen=new Set<string>();
  for(const call of parsed.tool_calls){if(!call||typeof call!=='object'||typeof call.id!=='string'||seen.has(call.id)||typeof call.name!=='string'||typeof call.arguments_json!=='string')throw new Error('Вызовы модели не имеют однозначной привязки.');seen.add(call.id)}
  return parsed.tool_calls as {id:string;name:string;arguments_json:string}[]
}
export function bindModelProposal(snapshot:ReviewSnapshot,proposal:ModelToolProposal){
  const calls=nativeSource(snapshot)
  if(proposal.organization_id!==snapshot.task.organization_id||proposal.task_id!==snapshot.task.id||proposal.run_id!==snapshot.task.current_run_id||proposal.candidate_id!==snapshot.candidate.id||proposal.operation_id!==snapshot.candidate.operation_id||proposal.artifact_id!==snapshot.artifact.id||proposal.artifact_version!==snapshot.artifact.version||proposal.content_digest!==snapshot.candidate.content_digest||!digest.test(proposal.arguments_digest)||!digest.test(proposal.tool_profile_digest))throw new Error('Предложение не соответствует текущему пакету модели.')
  const call=calls.find(call=>call.id===proposal.call_id)
  if(!call||call.name!==proposal.tool_name||call.arguments_json!==proposal.arguments_json)throw new Error('Точные аргументы предложения не совпали с исходным материалом.')
  return proposal
}
export async function prepareModelProposal(snapshot:ReviewSnapshot,proposal:ModelToolProposal,tool:ToolInfo):Promise<NativePreview>{
  bindModelProposal(snapshot,proposal)
  if(!tool.active||tool.organization_id!==snapshot.task.organization_id||tool.id!==proposal.tool_id||tool.version!==proposal.tool_version||proposal.execution_id)throw new Error('Эта версия инструмента недоступна или действие уже подготовлено.')
  const args=exactArguments(proposal.arguments_json!,tool.max_input_bytes)
  if(await textDigest(args)!==proposal.arguments_digest)throw new Error('Контрольная сумма аргументов не совпала.')
  return {snapshot,tool,payload:{model_proposal_id:proposal.id,occurrence_id:`model:${proposal.id}`,expected_task_version:snapshot.task.version,candidate_id:snapshot.candidate.id,artifact_id:snapshot.artifact.id,artifact_version:snapshot.artifact.version,content_digest:snapshot.candidate.content_digest,tool_id:tool.id,tool_version:tool.version,arguments_json:args,expires_in_seconds:900}}
}
export function continueModelPayload(snapshot:ReviewSnapshot,proposals:ModelToolProposal[],views:ToolExecutionView[],reason:string,ack:boolean):ContinueModel{
  const calls=nativeSource(snapshot)
  if(!ack)throw new Error('Подтвердите возможные расходы следующего обращения к модели.')
  if(!reason.trim())throw new Error('Укажите причину продолжения.');validateText(reason,4096,'Причина')
  if(snapshot.task.financial_hold||Date.parse(snapshot.task.execution_deadline??'')<=Date.now())throw new Error('Бюджет или срок задачи не допускает новый шаг.')
  if(proposals.length!==calls.length||views.length!==proposals.length)throw new Error('Нужны завершённые исходы всех предложений этого пакета.')
  const seen=new Set<string>(),results:ModelToolResultBinding[]=[]
  for(const proposal of proposals){
    bindModelProposal(snapshot,proposal)
    if(seen.has(proposal.call_id))throw new Error('Предложение повторяется.');seen.add(proposal.call_id)
    const view=views.find(view=>view.execution.id===proposal.execution_id)
    if(!view||view.execution.organization_id!==snapshot.task.organization_id||view.execution.task_id!==snapshot.task.id||view.execution.candidate_id!==snapshot.candidate.id||view.execution.model_proposal_id!==proposal.id||view.execution.occurrence_id!==`model:${proposal.id}`||view.execution.run_id!==proposal.run_id||view.execution.operation_id!==proposal.operation_id||view.execution.artifact_id!==proposal.artifact_id||view.execution.artifact_version!==proposal.artifact_version||view.execution.content_digest!==proposal.content_digest||view.execution.tool_id!==proposal.tool_id||view.execution.tool_version!==proposal.tool_version||view.execution.arguments_digest!==proposal.arguments_digest||view.execution.tool_profile_digest!==proposal.tool_profile_digest)throw new Error('Исход действия не соответствует предложению модели.')
    const status=view.execution.status
    const result:ModelToolResultBinding={proposal_id:proposal.id,execution_id:view.execution.id,execution_version:view.execution.version,status:status as ModelToolResultBinding['status']}
    if(status==='succeeded'||status==='rejected'){
      if(!view.receipt||view.receipt.id!==view.execution.result_receipt_id||view.receipt.execution_id!==view.execution.id||view.receipt.outcome!==status||!digest.test(view.receipt.content_digest))throw new Error('Не получена точная квитанция исхода действия.')
      result.receipt_id=view.receipt.id;result.content_digest=view.receipt.content_digest
    }else if(!['denied','expired','cancelled'].includes(status))throw new Error('Сначала завершите или сверьте все действия. Неизвестный исход блокирует продолжение.')
    results.push(result)
  }
  return {expected_task_version:snapshot.task.version,candidate_id:snapshot.candidate.id,artifact_id:snapshot.artifact.id,artifact_version:snapshot.artifact.version,content_digest:snapshot.candidate.content_digest,results,acknowledge_cost:true,reason}
}

// SPDX-License-Identifier: Elastic-2.0
import {t} from './i18n'
import {useEffect,useRef,useState,type FormEvent} from 'react'
import {ApiError,type ApiClient,type Artifact,type ModelToolProposal,type Task,type ToolExecutionView,type ToolInfo} from './api'
import {sameIdentity,type DraftScope} from './draftVault'
import {bindCandidate,type ReviewSnapshot} from './review'
import {bindTool,bindToolView,textDigest,toolStatusLabels} from './toolIntent'
import {continueModelPayload,nativeSource,prepareModelProposal,type NativePreview} from './modelIntent'
import {money} from './format'
import {ErrorNotice,Modal} from './ui'

async function pages<T>(read:(cursor?:string)=>Promise<{data:T[];next_cursor?:string}>){const items:T[]=[];const seen=new Set<string>();let cursor:string|undefined;do{const page=await read(cursor);items.push(...page.data);cursor=page.next_cursor;if(cursor){if(seen.has(cursor)||seen.size>=128)throw new Error('Список предложений не завершён.');seen.add(cursor)}}while(cursor);return items}
type Continuation={snapshot:ReviewSnapshot;proposals:ModelToolProposal[];views:ToolExecutionView[];artifacts:Artifact[]}

export default function ModelTools({api,task,scope,tools,permissions,onPreview,onChanged}:{api:ApiClient;task:Task;scope:DraftScope;tools:ToolInfo[];permissions:string[];onPreview:(preview:NativePreview)=>void;onChanged:(task:Task)=>void}){
  const [proposals,setProposals]=useState<ModelToolProposal[]>([]),[error,setError]=useState<unknown>(null),[modalError,setModalError]=useState<unknown>(null)
  const [busy,setBusy]=useState(false),[continuation,setContinuation]=useState<Continuation|null>(null),[reason,setReason]=useState(''),[ack,setAck]=useState(false),[submitted,setSubmitted]=useState(false),[refresh,setRefresh]=useState(0)
  const epoch=useRef(0)
  async function authorize(){const identity=await api.currentIdentity();if(!sameIdentity(scope,identity))throw new ApiError(t("Учётная запись изменилась. Подключите пространство заново."),'unauthenticated');return identity}
  function failure(cause:unknown){setError(cause);if(cause instanceof ApiError&&['unauthenticated','forbidden','not_found'].includes(cause.code)){setProposals([]);setContinuation(null)}}
  useEffect(()=>{epoch.current++;setProposals([]);setContinuation(null);setError(null);setBusy(false);return()=>{epoch.current++}},[api,task.id])
  useEffect(()=>{let active=true;if(task.waiting_reason!=='model_tools_pending')return;void(async()=>{try{await authorize();const all=await pages(cursor=>api.modelProposals(task.id,cursor));await authorize();if(active){setProposals(all.filter(proposal=>proposal.candidate_id===task.candidate_id&&proposal.run_id===task.current_run_id));setError(null)}}catch(cause){if(active)failure(cause)}})();return()=>{active=false}},[api,task.id,task.version,task.candidate_id,refresh])
  async function source(){
    await authorize();const fresh=await api.task(task.id)
    const candidates=await pages(cursor=>api.candidates(task.id,cursor)),candidate=candidates.find(item=>item.id===fresh.candidate_id)
    if(!candidate)throw new Error('Обновите текущий пакет предложений модели.')
    const artifact=await api.artifact(task.id,candidate.artifact_id,candidate.artifact_version)
    const snapshot=await bindCandidate(fresh,[candidate],[artifact]);if(!snapshot)throw new Error('Текущий пакет модели недоступен.')
    nativeSource(snapshot);return snapshot
  }
  async function preview(id:string){const current=epoch.current;setBusy(true);setError(null);try{
    const snapshot=await source(),proposal=await api.modelProposal(task.id,id),choice=tools.find(tool=>tool.id===proposal.tool_id&&tool.version===proposal.tool_version)
    if(!choice)throw new Error('Закреплённая версия инструмента сейчас недоступна. Обратитесь к администратору.')
    const prepared=await prepareModelProposal(snapshot,proposal,await bindTool(choice,scope.organization))
    await authorize();if(current!==epoch.current)return;onChanged(snapshot.task);onPreview(prepared)
  }catch(cause){if(current===epoch.current)failure(cause)}finally{if(current===epoch.current)setBusy(false)}}
  async function prepareContinuation(){const current=epoch.current;setBusy(true);setError(null);try{
    const snapshot=await source(),metadata=(await pages(cursor=>api.modelProposals(task.id,cursor))).filter(item=>item.candidate_id===snapshot.candidate.id)
    const exact=await Promise.all(metadata.map(item=>api.modelProposal(task.id,item.id)))
    const views=await Promise.all(exact.map(async proposal=>{if(!proposal.execution_id)throw new Error('Сначала подготовьте и завершите каждое предложение модели.');return bindToolView(await api.toolExecution(task.id,proposal.execution_id),task.id,scope.organization)}))
    continueModelPayload(snapshot,exact,views,'Проверка готовности пакета',true)
    const artifacts=await Promise.all(views.filter(view=>view.receipt).map(async view=>{const artifact=await api.artifact(task.id,view.receipt!.artifact_id,1);if(artifact.organization_id!==scope.organization||artifact.task_id!==task.id||await textDigest(artifact.content)!==view.receipt!.content_digest)throw new Error('Текст результата не совпал с квитанцией.');return artifact}))
    const fresh=await api.task(task.id);if(fresh.version!==snapshot.task.version)throw new Error('Состояние изменилось во время проверки. Обновите пакет и проверьте его заново.')
    await authorize();if(current!==epoch.current)return
    setContinuation({snapshot,proposals:exact,views,artifacts});setReason('');setAck(false);setSubmitted(false);setModalError(null);onChanged(fresh)
  }catch(cause){if(current===epoch.current)failure(cause)}finally{if(current===epoch.current)setBusy(false)}}
  async function submit(event:FormEvent){event.preventDefault();if(!continuation||submitted)return;const current=epoch.current;setBusy(true);setModalError(null);try{
    const payload=continueModelPayload(continuation.snapshot,continuation.proposals,continuation.views,reason,ack)
    await authorize();if(current!==epoch.current)return;setSubmitted(true)
    const next=await api.continueModel(task.id,payload);await authorize();if(current!==epoch.current)return
    setContinuation(null);onChanged(next)
  }catch(cause){if(current===epoch.current){setModalError(cause);if(cause instanceof ApiError&&['unauthenticated','forbidden','not_found'].includes(cause.code))failure(cause)}}finally{if(current===epoch.current)setBusy(false)}}
  if(task.waiting_reason!=='model_tools_pending')return null
  const changed=Boolean(continuation&&(task.version!==continuation.snapshot.task.version||task.candidate_id!==continuation.snapshot.candidate.id))
  const allTerminal=proposals.length>0&&proposals.every(proposal=>['succeeded','rejected','denied','expired','cancelled'].includes(proposal.execution_status??''))
  const mayPrepare=permissions.includes('actions.prepare_any')||task.owner_id===scope.principal&&permissions.includes('actions.prepare_own')
  const mayContinue=permissions.includes('tasks.continue_any')||task.owner_id===scope.principal&&permissions.includes('tasks.continue_own')
  return <section className="native-tools" aria-label={t("Предложения модели")}>
    <div className="artifact-top"><h2>{t("Модель предлагает действия")}</h2><button className="button button-small button-secondary" disabled={busy} onClick={()=>setRefresh(n=>n+1)}>{t("Обновить предложения")}</button></div>
    <p>{t("Проверьте каждое предложение. Подготовка резервирует бюджет; разрешение и запуск выполняются отдельно. После завершения всего пакета можно передать его результаты модели.")}</p>
    <ErrorNotice error={error}/>
    <div className="artifact-list">{proposals.map(proposal=><article className="artifact" key={proposal.id}><div className="artifact-top"><strong>{tools.find(tool=>tool.id===proposal.tool_id)?.name??t("Инструмент")} · v{proposal.tool_version}</strong><span>{proposal.execution_status?t(toolStatusLabels[proposal.execution_status]??proposal.execution_status):t("Ещё не подготовлено")}</span></div><details><summary>{t("Привязка предложения")}</summary><p className="digest">{proposal.id} · {proposal.call_id}</p><p className="digest">{t("SHA-256 аргументов: ")}{proposal.arguments_digest}</p></details>{!proposal.execution_id&&<button className="button button-secondary" disabled={busy||!mayPrepare||task.status!=='waiting_input'||Boolean(error)} onClick={()=>void preview(proposal.id)}>{t("Проверить предложение модели")}</button>}{proposal.execution_id&&<p className="aside-note">{t("Для решения, запуска или сверки откройте точное действие ниже.")}</p>}{proposal.execution_status==='unknown'&&<p className="notice notice-warning">{t("Исход неизвестен. Продолжение модели и повторная отправка остановлены до сверки.")}</p>}</article>)}</div>
    <button className="button button-primary" disabled={busy||!mayContinue||!allTerminal||task.status!=='waiting_input'||Boolean(error)||task.financial_hold} onClick={()=>void prepareContinuation()}>{t("Проверить пакет и продолжить модель")}</button>
    {!allTerminal&&<p className="aside-note">{t("Для продолжения нужны завершённые исходы всех предложений, включая явный отказ в неподходящих действиях.")}</p>}
    {continuation&&<Modal title={t("Следующий шаг модели")} description={t("Модель получит точные исходы всего пакета в той же попытке. Это отдельное обращение с возможными новыми расходами.")} onClose={()=>{if(!busy)setContinuation(null)}} busy={busy}><form className="modal-form" onSubmit={event=>void submit(event)}>
      <ErrorNotice error={modalError}/>{changed&&<p role="alert" className="notice notice-warning">{t("Пакет изменился. Закройте форму и проверьте текущие результаты заново.")}</p>}
      {submitted&&Boolean(modalError)&&<p className="notice notice-warning">{t("Команда уже отправлена. Проверьте состояние задачи; эта форма не отправляет её повторно.")}</p>}
      <p>{t("Сохранённый резерв: ")}{money(continuation.snapshot.task.reserved_microusd)}{t(". Он не обнуляется при продолжении.")}</p>
      {continuation.views.map(view=><article className="artifact" key={view.execution.id}><h3>{view.tool.name}</h3><p>{t(toolStatusLabels[view.execution.status]??view.execution.status)}{t(" · версия ")}{view.execution.version}</p><h4>{t("Точные аргументы предложения")}</h4><pre className="prose-output artifact-preview">{view.execution.arguments_json}</pre>{view.execution.reason&&<><h4>{t("Сохранённая причина")}</h4><pre className="prose-output">{view.execution.reason}</pre></>}<details><summary>{t("Привязка предложения и исхода")}</summary><p className="digest">{t("Предложение: ")}{view.execution.model_proposal_id}{t(" · вызов: ")}{continuation.proposals.find(proposal=>proposal.id===view.execution.model_proposal_id)?.call_id}</p><p className="digest">{t("Действие: ")}{view.execution.id}{t(" · квитанция: ")}{view.receipt?.id??t("нет внешнего эффекта")}</p></details>{view.receipt&&<><p>{view.receipt.method==='operator_assertion'?t("Утверждение оператора"):t("Ответ зарегистрированного инструмента")} · {view.receipt.effect_reference}</p><pre className="prose-output artifact-preview">{continuation.artifacts.find(artifact=>artifact.id===view.receipt!.artifact_id)?.content}</pre></>}</article>)}
      <p className="aside-note">{t("Результаты инструментов рассматриваются как данные. Они не подтверждают окончательный счёт или готовность итогового ответа.")}</p>
      <label className="field">{t("Причина продолжения")}<textarea aria-label={t("Причина продолжения")} value={reason} onChange={event=>setReason(event.target.value)} disabled={busy||changed} required rows={3}/></label>
      <label className="acknowledgment"><input type="checkbox" checked={ack} onChange={event=>setAck(event.target.checked)} disabled={busy||changed}/>{t("Понимаю, что следующий шаг модели может увеличить расходы. Передать именно эти результаты.")}</label>
      <div className="modal-footer"><button type="button" className="button button-secondary" disabled={busy} onClick={()=>setContinuation(null)}>{t("Закрыть")}</button><button className="button button-primary" disabled={busy||changed||!ack||submitted||!mayContinue}>{t("Продолжить с новыми расходами")}</button></div>
    </form></Modal>}
  </section>
}

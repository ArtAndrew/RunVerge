import {t} from './i18n'
import {useEffect,useRef,useState,type FormEvent} from 'react'
import {ApiError,type CurrentIdentity,type ApiClient,type Artifact,type Task,type TaskCandidate,type TaskDecision,type ReviewDecision} from './api'
import {mayReviewTask,bindCandidate,decisionLabels,freshExecutionMetadata,reviewLabels,reviewDecisions,reviewPayload,retryBlockReason,retryPayload,type ReviewSnapshot} from './review'
import {dateTime,money} from './format'
import TeamResult from './TeamResult'
import CopyText from './CopyText'
import {ErrorNotice,Icon,Modal,Status} from './ui'

export default function ResultReview({identity,api,task,candidates,artifacts,decisions,children,loadError,onRefresh,onChanged,onOpenTask,onEditMaterial}:{identity:CurrentIdentity|null;api:ApiClient;task:Task;candidates:TaskCandidate[];artifacts:Artifact[];decisions:TaskDecision[];children:Task[];loadError:unknown;onRefresh:()=>void;onChanged:(task:Task)=>void;onOpenTask:(id:string)=>void;onEditMaterial:(snapshot:ReviewSnapshot)=>void}) {
  const [snapshot,setSnapshot]=useState<ReviewSnapshot|null>(null)
  const [basis,setBasis]=useState<ReviewSnapshot|null>(null)
  const [bindingError,setBindingError]=useState<unknown>(null)
  const [bindingBusy,setBindingBusy]=useState(false)
  const [review,setReview]=useState<{snapshot:ReviewSnapshot;decision:ReviewDecision;basis:ReviewSnapshot|null}|null>(null)
  const [retry,setRetry]=useState<Task|null>(null)
  const [reason,setReason]=useState('')
  const [feedback,setFeedback]=useState('')
  const [acknowledged,setAcknowledged]=useState(false)
  const [busy,setBusy]=useState(false)
  const [error,setError]=useState<unknown>(null)
  const epoch=useRef(0)
  useEffect(()=>{epoch.current++;setBusy(false);setReview(null);setRetry(null);setSnapshot(null);setBasis(null);setError(null);setReason('');setFeedback('');setAcknowledged(false);return()=>{epoch.current++}},[api,task.id])
  useEffect(()=>{
    let active=true;setBindingBusy(true)
    void bindCandidate(task,candidates,artifacts).then(async value=>{let original:ReviewSnapshot|null=null;if(value?.candidate.source==='material'){if(!value.candidate.review_basis_candidate_id)throw new Error('Не получена исходная основа опубликованного материала.');original=await bindCandidate({...task,candidate_id:value.candidate.review_basis_candidate_id},candidates,artifacts)}if(active){setSnapshot(value);setBasis(original);setBindingError(null)}}).catch(cause=>{if(active){setSnapshot(null);setBasis(null);setBindingError(cause)}}).finally(()=>{if(active)setBindingBusy(false)})
    return()=>{active=false}
  },[task,candidates,artifacts])
  const current=snapshot && snapshot.task.id===task.id && snapshot.task.version===task.version && snapshot.candidate.id===task.candidate_id && snapshot.candidate.run_id===task.current_run_id
  const native=snapshot?.candidate.source==='model_tools'
  const reviewAllowed=mayReviewTask(identity,task)
  useEffect(()=>{if(!reviewAllowed)setReview(null)},[reviewAllowed])
  const mayReview=Boolean(reviewAllowed && current && task.status==='waiting_input' && !bindingBusy && !loadError)
  const retryBlocked=retryBlockReason(task)
  const modalTask=review?.snapshot.task??retry
  const changed=Boolean(modalTask && (modalTask.id!==task.id || modalTask.version!==task.version || modalTask.current_run_id!==task.current_run_id || modalTask.candidate_id!==task.candidate_id))
  function openReview(decision:ReviewDecision){if(!mayReview||!snapshot||!reviewDecisions(snapshot.candidate.source).includes(decision))return;setReview({snapshot,decision,basis});setReason('');setError(null)}
  function openRetry(){setRetry({...task});setReason('');setFeedback('');setAcknowledged(false);setError(null)}
  function close(){if(!busy){setReview(null);setRetry(null);setError(null)}}
  async function submit(event:FormEvent){
    event.preventDefault();if(changed||loadError||(!review&&!retry))return
    setError(null)
    const submittedEpoch=epoch.current
    try{
      const payload=review?reviewPayload(review.snapshot,review.decision,reason):retryPayload(retry!,reason,feedback,acknowledged)
      setBusy(true)
      if(review){const me=await api.currentIdentity();if(submittedEpoch!==epoch.current)return;if(!identity||me.organization_id!==identity.organization_id||me.principal_id!==identity.principal_id||!mayReviewTask(me,task))throw new ApiError(t("Решение доступно только уполномоченному человеку в этой организации."),'forbidden')}
      const result=review?await api.reviewTask(task.id,payload as ReturnType<typeof reviewPayload>):await api.retryTask(task.id,payload as ReturnType<typeof retryPayload>)
      if(submittedEpoch!==epoch.current)return
      setReview(null);setRetry(null);onChanged(result)
    }catch(cause){if(submittedEpoch!==epoch.current)return;setError(cause);if(cause instanceof ApiError && ['conflict','forbidden','unauthenticated','not_found'].includes(cause.code))onRefresh()}finally{if(submittedEpoch===epoch.current)setBusy(false)}
  }
  return <div className="review-workspace">
    {task.candidate_id && <section className="review-candidate" aria-label={t("Текущий черновик")}>
      <div className="section-kicker"><Icon name="file" />{task.accepted_decision_id?t("Принятый материал"):task.status==='succeeded'?t("Материал завершённой задачи"):task.status==='waiting_input'?t("Черновик · ожидает решения"):t("Материал предыдущей попытки")}</div>
      <ErrorNotice error={bindingError}/>
      {bindingBusy&&!snapshot&&<p role="status">{t("Проверяем версию материала…")}</p>}
      {current&&snapshot&&<><p className="muted">{snapshot.artifact.name} · v{snapshot.artifact.version}{t(" · попытка ")}{task.attempt_count}</p><CopyText key={`${snapshot.artifact.id}:${snapshot.artifact.version}`} text={snapshot.artifact.content}/>{snapshot.candidate.source==='model_tools'?<div className="notice notice-info"><p>{t("Модель подготовила предложения инструментов. Откройте «Действия», чтобы проверить их и продолжить работу. Итоговый ответ появится после следующего шага модели.")}</p><details><summary>{t("Исходный пакет предложений")}</summary><pre className="prose-output">{snapshot.artifact.content}</pre></details></div>:snapshot.candidate.source==='team'?<TeamResult artifact={snapshot.artifact} children={children} onOpenTask={onOpenTask} contentDigest={snapshot.candidate.content_digest}/>:<pre className="prose-output">{snapshot.artifact.content}</pre>}{snapshot.candidate.source==='material'&&<><p className="aside-note">{t("Редактируемый итог · материал v")}{snapshot.candidate.material_version}{t(" · автор публикации ")}{snapshot.candidate.published_by}{t(". Текст предложен пользователем и требует отдельного решения.")}</p><BasisView basis={basis}/></>}{!native&&<button className="button button-small button-secondary" onClick={()=>onEditMaterial(snapshot)}>{snapshot.candidate.source==='team'?t("Подготовить итоговый текст"):t("Создать редактируемую копию")}</button>}<details><summary>{t("Версия и контрольная сумма")}</summary><dl className="technical-ids"><div><dt>{t("Черновик")}</dt><dd>{snapshot.candidate.id}</dd></div><div><dt>{t("Попытка")}</dt><dd>{snapshot.candidate.run_id}</dd></div><div><dt>SHA-256</dt><dd>{snapshot.candidate.content_digest}</dd></div></dl></details></>}
      {snapshot?.candidate.source==='fixture'&&<p className="aside-note">{t("Тестовое исполнение fixture:echo: завершение подтверждает точное повторение входа, а не качество ответа модели или решение пользователя.")}</p>}{task.status==='waiting_input'&&reviewAllowed&&<><p className="aside-note">{t("Решение относится к этой версии материала. Принятие не подтверждает фактическую точность текста или окончательный расход.")}</p><div className="review-actions">{reviewDecisions(snapshot?.candidate.source??'').map(decision=><button key={decision} className={`button ${decision==='accept'?'button-primary':'button-secondary'}`} disabled={!mayReview||busy} onClick={()=>openReview(decision)}>{t(reviewLabels[decision])}</button>)}</div></>}
    </section>}
    {!freshExecutionMetadata(task)&&<div className="notice notice-info"><p>{t("Для решения или новой попытки нужны свежие сведения о выполнении.")}</p><button className="button button-secondary" onClick={onRefresh}>{t("Обновить задачу")}</button></div>}
    {task.status==='blocked'&&<section className="retry-panel"><h3>{t("Новая попытка")}</h3><p>{t("Прежние расходы и резервы сохраняются. Новая попытка использует тот же бюджет задачи и может привести к дополнительным расходам.")}</p>{retryBlocked&&<p className="muted">{t(retryBlocked)}</p>}<button className="button button-secondary" disabled={Boolean(retryBlocked)||Boolean(loadError)||busy} onClick={openRetry}>{t("Подготовить новую попытку")}</button></section>}
    {task.parent_id&&<p><button className="button button-small button-secondary" onClick={()=>onOpenTask(task.parent_id!)}>{t("Открыть родительскую задачу")}</button></p>}
    {children.length>0&&<section className="child-tasks"><h3>{t("Подзадачи команды")}</h3><p className="muted">{t("Результаты участников сохраняются отдельно. Оценка проверяющего агента не является доказательством истинности или присутствия человека.")}</p>{children.map(child=><article className="artifact" key={child.id}><div className="artifact-top"><strong>{child.goal}</strong><Status status={child.status}/></div><p className="muted">{t("Попытки ")}{child.attempt_count??'—'} / {child.max_runs??'—'}{t(" · в резерве ")}{money(child.reserved_microusd)}</p><button className="button button-small button-secondary" onClick={()=>onOpenTask(child.id)}>{t("Открыть подзадачу")}</button></article>)}</section>}
    {decisions.length>0&&<section className="decision-history"><h3>{t("История решений")}</h3><ol className="timeline">{decisions.map(decision=><li key={decision.id}><span className="timeline-marker"/><div className="timeline-top"><strong>{t(decisionLabels[decision.decision]??decision.decision)}</strong><time dateTime={decision.created_at}>{dateTime(decision.created_at)}</time></div><p>{t("От имени ")}{decision.principal_id}{t(" · материал v")}{decision.artifact_version}</p>{decision.reason&&<p className="prose-output">{decision.reason}</p>}<details><summary>{t("Запись решения")}</summary><dl className="technical-ids"><div><dt>{t("Способ")}</dt><dd>{decision.method}</dd></div><div><dt>{t("Попытка")}</dt><dd>{decision.run_id}</dd></div><div><dt>{t("Материал")}</dt><dd>{decision.artifact_id}</dd></div><div><dt>SHA-256</dt><dd>{decision.content_digest}</dd></div><div><dt>{t("Версия задачи")}</dt><dd>{decision.task_version_before} → {decision.task_version_after}</dd></div></dl></details></li>)}</ol></section>}
    {(review||retry)&&<Modal title={review?t(reviewLabels[review.decision]):t("Новая попытка")} description={review?t("Решение будет сохранено для выбранной версии материала."):t("Это новый запуск исполнителя. Существующие резервы и расходы не обнуляются.")} onClose={close} busy={busy}><form className="modal-form" onSubmit={event=>void submit(event)}>
      <ErrorNotice error={error}/>
      {changed&&<div className="notice notice-warning" role="alert">{t("Задача изменилась. Закройте форму, обновите результат и примите решение заново.")}</div>}
      {Boolean(loadError)&&<p role="alert">{t("Нет актуального подтверждения доступа к задаче. Обновите данные перед действием.")}</p>}
      {review&&<><p>{review.snapshot.artifact.name} · v{review.snapshot.artifact.version}{t(" · попытка ")}{review.snapshot.task.attempt_count}</p>{review.snapshot.candidate.source==='team'?<TeamResult artifact={review.snapshot.artifact} contentDigest={review.snapshot.candidate.content_digest}/>:<pre className="prose-output artifact-preview">{review.snapshot.artifact.content}</pre>}{review.snapshot.candidate.source==='material'&&<BasisView basis={review.basis}/>} {review.decision==='request_changes'&&<p>{t("Этот запрос сохранит замечание и приостановит задачу. Запуск доработки оформляется отдельно с подтверждением расходов.")}</p>}{review.decision==='reject'&&<p>{t("Задача завершится без принятого результата. Новая попытка этой задачи после отклонения недоступна.")}</p>}</>}
      {retry&&<><dl className="cost-breakdown"><div><dt>{t("Бюджет задачи и подзадач")}</dt><dd>{money(retry.budget_microusd)}</dd></div><div><dt>{t("Подтверждено")}</dt><dd>{money(retry.spent_microusd)}</dd></div><div><dt>{t("Сохранённый резерв")}</dt><dd>{money(retry.reserved_microusd)}</dd></div><div><dt>{t("Попытки")}</dt><dd>{retry.attempt_count} / {retry.max_runs}</dd></div><div><dt>{t("Срок")}</dt><dd>{dateTime(retry.execution_deadline!)}</dd></div></dl><p>{t("Сервер заново проверит права, доступный бюджет, срок и лимит попыток. Размер нового списания пока неизвестен.")}</p></>}
      <label className="field">{review?t("Причина решения"):t("Причина новой попытки")}{review?.decision==='accept'&&<span className="optional">{t("необязательно")}</span>}<textarea aria-label={review?t("Причина решения"):t("Причина новой попытки")} rows={3} value={reason} onChange={event=>setReason(event.target.value)} disabled={busy||changed} required={!review||review.decision!=='accept'}/><small>{t("До 4096 байт UTF-8. Запись останется в истории.")}</small></label>
      {retry&&<><label className="field">{t("Указания исполнителю")}<textarea aria-label={t("Указания исполнителю")} rows={4} value={feedback} onChange={event=>setFeedback(event.target.value)} disabled={busy||changed} required={retry.waiting_reason==='candidate_changes_requested'}/><small>{t("Эти указания войдут в неизменяемый вход новой попытки. До 8192 байт UTF-8.")}</small></label><label className="acknowledgment"><input type="checkbox" checked={acknowledged} onChange={event=>setAcknowledged(event.target.checked)} disabled={busy||changed}/>{t("Я понимаю, что новая попытка может увеличить расходы, а прежние резервы сохранятся.")}</label></>}
      <div className="modal-footer"><button className="button button-secondary" type="button" onClick={close} disabled={busy}>{t("Закрыть")}</button><button className="button button-primary" disabled={busy||changed||Boolean(loadError)||Boolean(retry&&!acknowledged)} type="submit">{busy?t("Сохраняем…"):review?t(reviewLabels[review.decision]):t("Запустить новую попытку")}</button></div>
    </form></Modal>}
  </div>
}

function BasisView({basis}:{basis:ReviewSnapshot|null}){
  if(!basis)return null
  return <details className="material-basis"><summary>{t("Исходная основа результата · сохранённые материалы")}</summary><p className="muted">{t("Эта версия не изменялась при редактировании итогового текста. Она определяет прежние ссылки на участников, а не достоверность нового текста.")}</p>{basis.candidate.source==='team'?<TeamResult artifact={basis.artifact} contentDigest={basis.candidate.content_digest}/>:<pre className="prose-output material-exact">{basis.artifact.content}</pre>}<p className="material-digest">{t("SHA-256 основы: ")}{basis.candidate.content_digest}</p></details>
}

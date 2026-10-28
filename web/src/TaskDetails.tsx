import {t} from './i18n'
import { useEffect, useRef, useState } from 'react'
import { ApiError, type Agent, type CurrentIdentity,type ApiClient, type Artifact, type CostReport, type Task, type TaskEvent, type TaskCandidate, type TaskDecision } from './api'
import {unknownOperationCount} from './modelAccounting'
import { dateTime, money, terminalStatuses } from './format'
import ResultReview from './ResultReview'
import CopyText from './CopyText'
import MaterialWorkspace from './MaterialWorkspace'
import ToolActions from './ToolActions'
import AutomationControls from './AutomationControls'
import {sameIdentity,type DraftVault,type DraftScope} from './draftVault'
import type {ReviewSnapshot} from './review'
import {EventJournal,eventReconnectDelay,type EventConnectionState} from './eventJournal'
import {EventReadinessContext} from './eventReadiness'
import TeamResult, {isTeamResultArtifact} from './TeamResult'
import { BudgetBar, ErrorNotice, Icon, Loading, Modal, Status, EmptyState } from './ui'

const eventLabels: Record<string, string> = {
  'task.created': 'Задача создана', 'task.queued': 'Задача в очереди', 'task.started': 'Работа началась',
  'task.running': 'Агент выполняет задачу', 'task.succeeded': 'Работа завершена', 'task.failed': 'Ошибка выполнения',
  'task.cancelled': 'Задача отменена', 'task.blocked': 'Работа приостановлена',
  'budget.reserved': 'Средства зарезервированы', 'budget.settled': 'Расход подтверждён',
  'cost.settled': 'Расход подтверждён', 'operation.claimed': 'Исполнитель начал операцию',
  'cost.fixture_reconciled': 'Тестовый расход восстановлен',
  'cost.reserved': 'Средства зарезервированы', 'cost.unknown': 'Расход ожидает сверки',
  'cost.quarantined': 'Счёт сохранён для финансовой сверки',
  'model.candidate_created': 'Подготовлен непроверенный материал',
  'task.result_decided': 'Сохранено решение по результату',
  'task.new_attempt': 'Начата новая попытка',
  'team.candidate_created': 'Собран черновик команды',
  'artifact.created': 'Добавлен результат', 'operation.completed': 'Операция выполнена',
}

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

function downloadArtifact(artifact: Artifact) {
  const blob = new Blob([artifact.content], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = artifact.name.replace(/[\/\\\u0000-\u001f]/g, '_') || 'artifact.txt'
  link.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

export default function TaskDetails({ identity,vault,draftScope,api, taskId, agents, onBack, onChanged, onOpenTask }: { identity:CurrentIdentity|null;vault:DraftVault;draftScope:DraftScope;api: ApiClient; taskId: string; agents: Agent[]; onBack: () => void; onChanged: () => void; onOpenTask: (id: string) => void }) {
  const [task, setTask] = useState<Task | null>(null)
  const [events, setEvents] = useState<TaskEvent[]>([])
  const [artifacts, setArtifacts] = useState<Artifact[]>([])
  const [candidates, setCandidates] = useState<TaskCandidate[]>([])
  const [materialSeed,setMaterialSeed]=useState<ReviewSnapshot|null>(null)
  const [artifactCursor,setArtifactCursor]=useState<string|undefined>(undefined)
  const [artifactBusy,setArtifactBusy]=useState(false)
  const artifactPaging=useRef(false)
  const artifactEpoch=useRef(0)
  const [decisions, setDecisions] = useState<TaskDecision[]>([])
  const [children, setChildren] = useState<Task[]>([])
  const [costs,setCosts]=useState<CostReport|null>(null)
  const [costError,setCostError]=useState<unknown>(null)
  const [error, setError] = useState<unknown>(null)
  const [loading, setLoading] = useState(true)
  const [refresh, setRefresh] = useState(0)
  const [tab, setTab] = useState<'result' | 'events' | 'artifacts' | 'actions' | 'automation'>('result')
  const [confirmCancel, setConfirmCancel] = useState(false)
  const [cancelBusy, setCancelBusy] = useState(false)
  const [cancelError, setCancelError] = useState<unknown>(null)
  const journal=useRef(new EventJournal())
  const needsSnapshot=useRef(true)
  const [streamStart,setStreamStart]=useState<{cursor:string}|null>(null)
  const [streamStatus,setStreamStatus]=useState<EventConnectionState>('connecting')
  const [streamError,setStreamError]=useState<unknown>(null)
  const [historyReset,setHistoryReset]=useState(false)
  const [historyGap,setHistoryGap]=useState(false)
  const [eventTruncated,setEventTruncated]=useState(false)

  useEffect(() => {
    setCosts(null);setCostError(null)
    setTask(null); setEvents([]); setArtifacts([]); setCandidates([]); setDecisions([]); setChildren([]); setLoading(true); journal.current=new EventJournal();needsSnapshot.current=true;setStreamStart(null);setStreamStatus('connecting');setStreamError(null);setHistoryReset(false);setHistoryGap(false);setEventTruncated(false);setMaterialSeed(null);setArtifactCursor(undefined);artifactPaging.current=false;artifactEpoch.current++;setArtifactBusy(false)
  }, [api, taskId])

  useEffect(() => {
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    let stopped = false
    async function poll() {
      try {
        if(!sameIdentity(draftScope,await api.currentIdentity(controller.signal)))throw new ApiError(t("Учётная запись изменилась. Подключите пространство заново."),'unauthenticated')
        async function allPages<T>(read: (cursor?: string) => Promise<{data:T[];next_cursor?:string}>): Promise<T[]> {
          const values:T[]=[];const seen=new Set<string>();let next:string|undefined
          do {const page=await read(next);values.push(...page.data);next=page.next_cursor;if(next){if(seen.has(next)||seen.size>=128)throw new Error('Некорректная последовательность страниц.');seen.add(next)}}while(next)
          return values
        }
        const nextSnapshot=needsSnapshot.current?await api.eventSnapshot(taskId,controller.signal):null
        const [nextTask, nextArtifacts, nextCandidates, nextDecisions, nextChildren, nextCosts] = await Promise.all([
          nextSnapshot?Promise.resolve(nextSnapshot.task):api.task(taskId, controller.signal),
          api.artifacts(taskId, controller.signal),
          allPages(cursor=>api.candidates(taskId,cursor,controller.signal)),
          allPages(cursor=>api.decisions(taskId,cursor,controller.signal)),
          allPages(cursor=>api.children(taskId,cursor,controller.signal)),
          Promise.resolve().then(async()=>{const report=await api.costs(taskId,controller.signal);unknownOperationCount(report.unknown_reservation_count);return{report,error:null}}).catch(error=>({report:null,error})),
        ])
        const current=nextCandidates.find(candidate=>candidate.id===nextTask.candidate_id)
        const basis=current?.review_basis_candidate_id?nextCandidates.find(candidate=>candidate.id===current.review_basis_candidate_id):undefined
        const exactCandidates=[current,basis].filter((candidate):candidate is TaskCandidate=>Boolean(candidate))
        const exact=await Promise.all(exactCandidates.filter(candidate=>!nextArtifacts.data.some(artifact=>artifact.id===candidate.artifact_id&&artifact.version===candidate.artifact_version)).map(candidate=>api.artifact(taskId,candidate.artifact_id,candidate.artifact_version,controller.signal)))
        if (stopped) return
        setTask(nextTask)
        setCosts(nextCosts.report);setCostError(nextCosts.error)
        if(nextSnapshot){
          setEvents(journal.current.snapshot(nextSnapshot));needsSnapshot.current=false;setEventTruncated(journal.current.truncated)
          setHistoryReset(false);setStreamError(null);setStreamStatus('connecting');setStreamStart({cursor:nextSnapshot.cursor})
        }
        setArtifacts(previous=>[...new Map([...previous,...nextArtifacts.data,...exact].map(artifact=>[`${artifact.id}:${artifact.version}`,artifact])).values()]);
        if(!artifactPaging.current){setArtifactCursor(nextArtifacts.next_cursor);artifactPaging.current=true}
        setCandidates(nextCandidates); setDecisions(nextDecisions); setChildren(nextChildren)
        setError(null)
        // Parent events do not cover subtree accounting, so retain bounded fresh reads.
        timer = setTimeout(poll, terminalStatuses.has(nextTask.status) ? 10000 : 7500)
      } catch (cause) {
        if (stopped || controller.signal.aborted) return
        setError(cause)
        if (cause instanceof ApiError && ['unauthenticated', 'forbidden', 'not_found','incompatible_client'].includes(cause.code)) {
          setTask(null); setEvents([]); setArtifacts([]); setCandidates([]); setDecisions([]); setChildren([]); setStreamStart(null);setStreamStatus('stopped')
        }
        timer = setTimeout(poll, 10000)
      } finally {
        if (!stopped) setLoading(false)
      }
    }
    void poll()
    return () => { stopped = true; controller.abort(); clearTimeout(timer) }
  }, [api, taskId, refresh])

  useEffect(()=>{
    if(!streamStart)return
    const controller=new AbortController();let stopped=false;let timer:ReturnType<typeof setTimeout>|undefined;let refreshTimer:ReturnType<typeof setTimeout>|undefined
    let attempt=0
    const wait=(ms:number)=>new Promise<void>(resolve=>{const done=()=>{clearTimeout(timer);controller.signal.removeEventListener('abort',done);resolve()};timer=setTimeout(done,ms);controller.signal.addEventListener('abort',done,{once:true});if(controller.signal.aborted)done()})
    async function listen(){
      while(!stopped&&!controller.signal.aborted){
        try{
          setStreamStatus(attempt?'reconnecting':'connecting')
          if(!sameIdentity(draftScope,await api.currentIdentity(controller.signal)))throw new ApiError(t("Учётная запись изменилась. Подключите пространство заново."),'unauthenticated')
          for await(const frame of api.streamTaskEvents(taskId,journal.current.cursor,controller.signal)){
            if(stopped)return
            if(frame.kind==='error')throw new ApiError(frame.error.message,frame.error.code,undefined,frame.error.retryable)
            if(frame.kind==='reset_required'){setHistoryReset(true);setHistoryGap(true);setStreamStatus('reset_required');return}
            // Apply the whole validated batch before adopting its opaque cursor.
            try{setEvents(journal.current.batch(frame.batch))}catch{throw new ApiError(t("Получены противоречивые события. Обновите состояние."),'invalid_event_stream')}
            setEventTruncated(journal.current.truncated);setStreamStatus('live');setStreamError(null);attempt=0
            if(frame.batch.events.length&&!refreshTimer)refreshTimer=setTimeout(()=>{refreshTimer=undefined;if(!stopped)setRefresh(value=>value+1)},250)
          }
        }catch(cause){
          if(stopped||controller.signal.aborted)return
          setStreamError(cause)
          if(cause instanceof ApiError){
            if(['unauthenticated','forbidden','not_found','incompatible_client'].includes(cause.code)){setTask(null);setEvents([]);setArtifacts([]);setCandidates([]);setDecisions([]);setChildren([]);setStreamStatus('stopped');return}
            if(cause.code==='invalid_event_stream'||!cause.retryable){setStreamStatus('stopped');return}
          }
        }
        if(stopped)return
        setStreamStatus('reconnecting');await wait(eventReconnectDelay(attempt++))
      }
    }
    void listen()
    return()=>{stopped=true;controller.abort();clearTimeout(timer);clearTimeout(refreshTimer)}
  },[api,taskId,streamStart,draftScope])

  function resetEventHistory(){setStreamStart(null);needsSnapshot.current=true;setHistoryReset(true);setStreamError(null);setHistoryGap(true);setRefresh(value=>value+1)}
  const commandsPaused=historyReset||streamStatus==='stopped'

  async function cancel() {
    setCancelBusy(true); setCancelError(null)
    try {
      setTask(await api.cancelTask(taskId))
      setConfirmCancel(false)
      setRefresh(value => value + 1)
      onChanged()
    } catch (cause) { setCancelError(cause) } finally { setCancelBusy(false) }
  }

  async function moreArtifacts(){if(!artifactCursor||artifactBusy)return;setArtifactBusy(true);const run=artifactEpoch.current;try{const page=await api.artifacts(taskId,undefined,artifactCursor);if(run!==artifactEpoch.current)return;setArtifacts(previous=>[...new Map([...previous,...page.data].map(artifact=>[`${artifact.id}:${artifact.version}`,artifact])).values()]);setArtifactCursor(page.next_cursor)}catch(cause){if(run===artifactEpoch.current)setError(cause)}finally{if(run===artifactEpoch.current)setArtifactBusy(false)}}
  const agent = agents.find(item => item.id === task?.agent_id && item.version === task?.agent_version)
  const isFixture = Boolean(agent?.model.startsWith('fixture:') || artifacts.some(item => String(item.provenance?.adapter ?? '').startsWith('fixture:')))
  const syntheticCost = isFixture || events.some(item => item.payload_version===1&&'synthetic_cost' in item.data&&item.data.synthetic_cost === true)
  const resultText = typeof task?.result === 'string' ? task.result : JSON.stringify(task?.result, null, 2) ?? ''
  const currentCandidate=candidates.find(item=>item.id===task?.candidate_id)
  const candidateArtifact=artifacts.find(item=>item.id===currentCandidate?.artifact_id&&item.version===currentCandidate?.artifact_version)
  const localUnmeteredOutput=candidateArtifact?.provenance?.model_accounting_mode==='operator_unmetered_local'
  let available = '0'
  if (task) {
    const remaining = BigInt(task.budget_microusd) - BigInt(task.spent_microusd) - BigInt(task.reserved_microusd)
    available = (!task.financial_hold && remaining > 0n ? remaining : 0n).toString()
  }
  return <EventReadinessContext.Provider value={{paused:commandsPaused,refresh:resetEventHistory}}><>
    <button className="back-button" onClick={onBack}><Icon name="arrow" />{t("Все задачи")}</button>
    <ErrorNotice error={streamError} />
    <ErrorNotice error={error} onRetry={() => setRefresh(value => value + 1)} />
    {error && task && <p className="muted">{t("Показаны последние полученные данные. Они могут быть неактуальны.")}</p>}
    {loading && !task && <Loading />}
    {task && <>
      <div className="detail-heading"><div><div className="eyebrow">{t("Задача · ")}{task.id.slice(0, 12)}</div><h1>{task.goal}</h1><div className="detail-meta"><Status status={task.status} /><span><Icon name="agents" />{agent?.name ?? t("Агент")} · v{task.agent_version}</span><span><Icon name="clock" />{dateTime(task.created_at)}</span></div></div><button className="button button-secondary" onClick={() => setRefresh(value => value + 1)}><Icon name="refresh" />{t("Обновить")}</button></div>
      <div className="event-connection" role="status">{({connecting:t("Подключаем журнал…"),live:t("События поступают в реальном времени"),reconnecting:t("Восстанавливаем подключение к журналу…"),reset_required:t("Доступная история изменилась"),stopped:t("Журнал приостановлен")})[streamStatus]}</div>
      {(historyReset||streamStatus==='stopped')&&<div className="notice notice-warning"><div><strong>{t("Нужно обновить состояние")}</strong><p>{historyReset?t("Часть истории больше недоступна для продолжения. Новый снимок покажет текущее состояние; сохранённые черновики останутся."):t("Подключение не прошло проверку. Команды приостановлены до обновления состояния.")}</p><button className="button button-secondary" onClick={resetEventHistory}>{t("Обновить состояние")}</button></div></div>}
      {historyGap&&!historyReset&&<p className="muted">{t("История была восстановлена по новому снимку. Журнал может содержать не все прежние события.")}</p>}
      {isFixture && <div className="notice notice-info"><Icon name="agents" /><p>{t("Тестовый запуск: ")}<code>{agent?.model ?? 'fixture'}</code>{t(". Его результат не является ответом языковой модели. Расходы синтетические: деньги не списываются.")}</p></div>}
      {localUnmeteredOutput&&<div className="notice notice-info"><Icon name="agents"/><p>{t("Для этой версии модельного результата администратор задал локальный тариф API 0. Стоимость оборудования и энергии не измерена. Платные инструменты и подзадачи по-прежнему ограничены общим бюджетом.")}</p></div>}
      {task.waiting_reason && <div className="notice notice-warning"><Icon name="clock" /><div><strong>{t("Нужно внимание")}</strong><p>{t(waitingLabels[task.waiting_reason] ?? t("Выполнение приостановлено. Требуется проверить причину на сервере."))}</p><details><summary>{t("Код причины")}</summary><code>{task.waiting_reason}</code></details></div></div>}
      {task.financial_hold && <div className="notice notice-warning"><Icon name="shield" /><div><strong>{t("Расходы приостановлены до сверки")}</strong><p>{t("Финансовые итоги требуют проверки. Учтённые суммы могут ещё не включать все полученные счета. Новые расходы заблокированы.")}</p></div></div>}
      <div className="detail-grid">
        <section className="panel detail-main">
          <div className="tabs" role="tablist" aria-label={t("Данные задачи")}>
            <button id="result-tab" role="tab" aria-selected={tab === 'result'} aria-controls="task-panel" onClick={() => setTab('result')}>{t("Результат")}</button>
            <button id="actions-tab" role="tab" aria-selected={tab === 'actions'} aria-controls="task-panel" onClick={() => setTab('actions')}>{t("Действия")}</button>
            <button id="automation-tab" role="tab" aria-selected={tab === 'automation'} aria-controls="task-panel" onClick={() => setTab('automation')}>{t("Автоматизация")}</button>
            <button id="events-tab" role="tab" aria-selected={tab === 'events'} aria-controls="task-panel" onClick={() => setTab('events')}>{t("Ход работы ")}<span>{events.length}</span></button>
            <button id="artifacts-tab" role="tab" aria-selected={tab === 'artifacts'} aria-controls="task-panel" onClick={() => setTab('artifacts')}>{t("Материалы ")}<span>{artifacts.length}</span></button>
          </div>
          <div id="task-panel" role="tabpanel" aria-labelledby={`${tab}-tab`} className="tab-content">
            {tab === 'actions' && <div inert={commandsPaused}><ToolActions api={api} task={task} scope={draftScope} onChanged={next=>{setTask(next);setRefresh(value=>value+1);onChanged()}}/></div>}
            {tab === 'automation' && <div inert={commandsPaused}><AutomationControls api={api} task={task} agent={agent} scope={draftScope} onChanged={()=>{setRefresh(value=>value+1);onChanged()}}/></div>}
            {tab === 'result' && <div inert={commandsPaused}><ResultReview identity={identity} api={api} task={task} candidates={candidates} artifacts={artifacts} decisions={decisions} children={children} loadError={error} onRefresh={() => setRefresh(value => value + 1)} onChanged={next => {setTask(next);setRefresh(value=>value+1);onChanged()}} onOpenTask={onOpenTask} onEditMaterial={snapshot=>{setMaterialSeed(snapshot);setTab('artifacts')}}/>{!task.candidate_id && (task.result ? <div className="result-content"><div className="section-kicker"><Icon name="file" />{t("Ответ исполнителя")}</div><CopyText text={resultText}/><pre className="prose-output">{resultText}</pre></div> : <EmptyState icon={task.status === 'succeeded' ? 'check' : 'activity'} title={task.status === 'succeeded' ? t("Текстовый ответ не добавлен") : t("Результат появится здесь")} description={task.status === 'succeeded' ? t("Проверьте материалы и события задачи.") : t("Статус и ход выполнения обновляются автоматически. Материалы сохраняются в отдельной вкладке.")} />)}</div>}
            {tab === 'events' && <>{eventTruncated&&<p className="muted">{t("Показана ограниченная часть журнала — не более 300 последних полученных событий.")}</p>}{events.length ? <ol className="timeline">{events.map(event => <li key={event.id}><span className="timeline-marker" /><div className="timeline-top"><strong>{t(eventLabels[event.type] ?? event.type)}</strong><time dateTime={event.created_at}>{dateTime(event.created_at)}</time></div>{event.payload_version===0&&<p className="muted">{t("Историческое событие: исходные данные сохранены без преобразования.")}</p>}<details><summary>{t("Данные события · #")}{event.sequence}</summary><pre className="json-output">{event.payload_version===0?event.legacy_data_json:JSON.stringify(event.data, null, 2)}</pre></details></li>)}</ol> : <EmptyState icon="activity" title={t("Событий пока нет")} description={t("Здесь будет последовательность действий, полученная от сервера.")} />}</>}
            {tab === 'artifacts' && <div inert={commandsPaused}><MaterialWorkspace vault={vault} draftScope={draftScope} api={api} task={task} seed={materialSeed} onSeedUsed={()=>setMaterialSeed(null)} onChanged={next=>{setTask(next);setRefresh(value=>value+1);onChanged()}} onPublished={next=>{setTask(next);setRefresh(value=>value+1);onChanged();setTab('result')}}/></div>}
            {tab === 'artifacts' && (artifacts.length ? <div className="artifact-list">{artifacts.map(artifact => <article className="artifact" key={artifact.id}><div className="artifact-top"><div><Icon name="file" /><strong>{artifact.name}</strong><span>v{artifact.version}</span></div><button className="button button-small button-secondary" onClick={() => downloadArtifact(artifact)}>{t("Скачать")}</button></div><CopyText key={`${artifact.id}:${artifact.version}`} text={artifact.content}/><p className="muted">{artifact.media_type} · {dateTime(artifact.created_at)}</p>{artifact.provenance?.verified === false && <p className="notice notice-warning">{t("Непроверенный материал. Его наличие не означает, что задача выполнена.")}</p>}{isTeamResultArtifact(artifact)?<TeamResult artifact={artifact} children={children} onOpenTask={onOpenTask}/>:<pre className="prose-output artifact-preview">{artifact.content}</pre>}<details><summary>{t("Происхождение материала")}</summary><pre className="json-output">{JSON.stringify(artifact.provenance, null, 2)}</pre></details></article>)}</div> : <EmptyState icon="file" title={t("Материалов пока нет")} description={t("Документы и другие сохранённые результаты появятся здесь.")} />)}
            {tab==='artifacts'&&artifactCursor&&<button className="button button-secondary" disabled={artifactBusy} onClick={()=>void moreArtifacts()}>{artifactBusy?t("Загружаем…"):t("Загрузить ещё исходные материалы")}</button>}
          </div>
        </section>
        <aside className="detail-aside"><section className="panel budget-panel"><div className="panel-title"><h2>{t("Бюджет задачи и подзадач")}</h2><Icon name="shield" /></div><BudgetBar budget={task.budget_microusd} spent={task.spent_microusd} reserved={task.reserved_microusd} /><dl className="cost-breakdown"><div><dt><i className="legend-used" />{t("Потрачено")}</dt><dd>{money(task.spent_microusd)}</dd></div><div><dt><i className="legend-held" />{t("В резерве")}</dt><dd>{money(task.reserved_microusd)}</dd></div><div><dt><i className="legend-free" />{t("Доступно")}</dt><dd>{money(available)}</dd></div></dl><p className="aside-note">{syntheticCost ? t("Тестовый учёт расходов. Это синтетические суммы, не реальные списания.") : t("Резерв учитывает операции, расход по которым ещё не подтверждён.")}</p></section>
          <section className="panel info-panel"><h2>{t("Сверка операций")}</h2><ErrorNotice error={costError} />{costs?<><p>{t("Ожидают окончательной сверки: ")}<strong>{unknownOperationCount(costs.unknown_reservation_count)}</strong></p><p className="aside-note">{t("По последнему отчёту для задачи и подзадач, включая операции с нулевым тарифом и за пределами текущей страницы. Нулевая сумма сама по себе не означает, что все операции завершены. Аппаратные расходы локальной модели не измерены.")}</p></>:<p className="aside-note">{t("Сведения о сверке пока не получены.")}</p>}</section>
          <section className="panel info-panel"><h2>{t("О выполнении")}</h2><dl><div><dt>{t("Попытки")}</dt><dd>{task.attempt_count ?? '—'} / {task.max_runs ?? '—'}</dd></div><div><dt>{t("Срок выполнения")}</dt><dd>{task.execution_deadline ? dateTime(task.execution_deadline) : '—'}</dd></div><div><dt>{t("Версия состояния")}</dt><dd>{task.version}</dd></div><div><dt>{t("Последнее изменение")}</dt><dd>{dateTime(task.updated_at)}</dd></div></dl><details><summary>{t("Идентификаторы")}</summary><dl className="technical-ids"><div><dt>{t("Задача")}</dt><dd>{task.id}</dd></div><div><dt>{t("Исполнитель")}</dt><dd>{task.agent_id}</dd></div><div><dt>{t("Владелец")}</dt><dd>{task.owner_id}</dd></div></dl></details></section>
          {!terminalStatuses.has(task.status) && <button disabled={commandsPaused} className="button button-danger cancel-task" onClick={() => { setCancelError(null); setConfirmCancel(true) }}><Icon name="stop" />{t("Отменить задачу")}</button>}
        </aside>
      </div>
    </>}
    {confirmCancel && !commandsPaused && <Modal title={t("Отменить задачу?")} description={t("Запрос остановит дальнейшее выполнение. Уже совершённые внешние действия автоматически не отменяются.")} onClose={() => setConfirmCancel(false)} busy={cancelBusy}><div className="modal-form"><ErrorNotice error={cancelError} /><div className="modal-footer"><button className="button button-secondary" disabled={cancelBusy} onClick={() => setConfirmCancel(false)}>{t("Продолжить работу")}</button><button className="button button-danger" disabled={cancelBusy} onClick={() => void cancel()}>{cancelBusy ? t("Отменяем…") : t("Отменить задачу")}</button></div></div></Modal>}
  </></EventReadinessContext.Provider>
}

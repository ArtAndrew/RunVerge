import {t} from './i18n'
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { type Agent, type AvailableModel, type ApiClient, type Connection, type TeamDefinition, type ToolInfo, normalizeBaseUrl } from './api'
import { byteLength, money, parseBudget } from './format'
import { ErrorNotice, Icon, Modal } from './ui'
import TeamFields, {validateTeamForm} from './TeamFields'
import {modelTariffLabel,modelResourceLabel,validateModelProfile,chooseModelSelection} from './modelAccounting'
import {taskExamples} from './taskExamples'
import {sourceTextStarter} from './agentStarter'

export function ConnectionForm({ initial, busy, error, onConnect }: { initial: Connection; busy: boolean; error: unknown; onConnect: (connection: Connection) => void }) {
  const [url, setUrl] = useState(initial.baseUrl)
  const [org, setOrg] = useState(initial.organizationId)
  const [token, setToken] = useState(initial.token)
  const [localError, setLocalError] = useState<unknown>(null)
  function submit(event: FormEvent) {
    event.preventDefault()
    try {
      const baseUrl = normalizeBaseUrl(url)
      if (!org.trim() || !token.trim()) throw new Error('Укажите организацию и токен доступа.')
      setLocalError(null)
      onConnect({ baseUrl, organizationId: org.trim(), token: token.trim() })
    } catch (cause) { setLocalError(cause) }
  }
  return <form className="connection-form" onSubmit={submit}>
    <ErrorNotice error={localError || error} />
    <label className="field">{t("Адрес API")}<input aria-label={t("Адрес API")} value={url} onChange={event => setUrl(event.target.value)} placeholder="/api/v1" spellCheck={false} autoCapitalize="none" required disabled={busy} aria-describedby="api-hint" /><small id="api-hint">{t("Для локального запуска оставьте /api/v1. Токен будет отправлен на указанный сервер.")}</small></label>
    <label className="field">{t("Организация")}<input value={org} onChange={event => setOrg(event.target.value)} placeholder={t("Идентификатор организации")} spellCheck={false} autoCapitalize="none" required disabled={busy} /></label>
    <label className="field">{t("Токен доступа")}<input aria-label={t("Токен доступа")} type="password" value={token} onChange={event => setToken(event.target.value)} placeholder={t("Вставьте токен")} autoComplete="off" spellCheck={false} required disabled={busy} aria-describedby="token-hint" /><small id="token-hint">{t("Хранится только в памяти этой страницы. После перезагрузки нужно войти снова.")}</small></label>
    <button className="button button-primary" disabled={busy} type="submit"><Icon name="link" />{busy ? t("Проверяем подключение…") : t("Подключить пространство")}</button>
  </form>
}

export function TaskForm({ api, agents, onClose, onCreated }: { api: ApiClient; agents: Agent[]; onClose: () => void; onCreated: (id: string) => void }) {
  const published = agents.filter(agent => agent.published)
  const [agentId, setAgentId] = useState(published[0]?.id ?? '')
  const [goal, setGoal] = useState('')
  const goalInput = useRef<HTMLTextAreaElement>(null)
  const [budget, setBudget] = useState('1.00')
  const [maxRuns,setMaxRuns]=useState('3')
  const [hours,setHours]=useState('24')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const agent = published.find(item => item.id === agentId)
  async function submit(event: FormEvent) {
    event.preventDefault()
    try {
      if (!goal.trim()) throw new Error('Опишите результат, который нужно получить.')
      if (byteLength(goal.trim()) > 16 * 1024) throw new Error('Описание задачи слишком большое: максимум 16 КиБ.')
      if (!agent) throw new Error('Выберите опубликованного агента.')
      const budgetMicrousd = parseBudget(budget)
      if (agent.budget_microusd > 0 && budgetMicrousd > agent.budget_microusd) throw new Error(t('Бюджет задачи превышает лимит агента: {limit}.', {limit:money(agent.budget_microusd)}))
      const attempts=Number(maxRuns),duration=Number(hours)
      if(!Number.isInteger(attempts)||attempts<1||attempts>10)throw new Error('Допустимо от 1 до 10 попыток.')
      if(!Number.isInteger(duration)||duration<1||duration>720)throw new Error('Срок должен быть от 1 до 720 часов.')
      setBusy(true); setError(null)
      const task = await api.createTask({ goal: goal.trim(), agent_id: agentId, budget_microusd: budgetMicrousd, max_runs:attempts, execution_timeout_seconds:duration*3600 })
      onCreated(task.id)
    } catch (cause) { setError(cause) } finally { setBusy(false) }
  }
  return <Modal title={t("Новая задача")} description={t("Задайте ожидаемый результат, исполнителя и предел расходов.")} onClose={onClose} busy={busy}>
    <form onSubmit={submit} className="modal-form">
      <ErrorNotice error={error} />
      {goal === '' && <fieldset className="task-examples" disabled={busy}>
        <legend>{t('Начать с примера')}</legend>
        <div className="example-choices">{taskExamples.map(example => <button key={example.id} type="button" className="button button-secondary" onClick={() => {
          const prompt = t(example.prompt)
          setGoal(current => current === '' ? prompt : current)
          goalInput.current?.focus()
        }}>{t(example.label)}</button>)}</div>
        <small>{t('Учебные данные. Измените текст под свою задачу и проверьте результат. Выбор примера не запускает агента.')}</small>
      </fieldset>}
      <label className="field">{t("Что нужно сделать?")}<textarea ref={goalInput} value={goal} onChange={event => setGoal(event.target.value)} rows={5} placeholder={t("Опишите задачу и критерии готового результата…")} required disabled={busy} autoFocus /></label>
      <label className="field">{t("Исполнитель")}<select aria-label={t("Исполнитель")} value={agentId} onChange={event => setAgentId(event.target.value)} required disabled={busy}><option value="" disabled>{t("Выберите агента")}</option>{published.map(item => <option key={item.id} value={item.id}>{item.name} · v{item.version}{item.model.startsWith('fixture:') ? t(" · тестовый") : ''}</option>)}</select><small>{t("Доступны опубликованные версии. Права и лимиты проверяются сервером.")}</small></label>
      {agent?.model.startsWith('fixture:') && <div className="notice notice-info"><Icon name="agents" /><p>{t("Этот агент использует тестовую модель")} <code>{agent.model}</code>{t(". Она проверяет работу платформы и не выполняет интеллектуальную задачу.")}</p></div>}
      <label className="field">{t("Бюджет задачи, USD")}<div className="currency-input"><span>$</span><input aria-label={t("Бюджет задачи, USD")} inputMode="decimal" value={budget} onChange={event => setBudget(event.target.value)} required disabled={busy} aria-describedby="budget-hint" /></div><small id="budget-hint">{t("Лимит агента:")} {agent ? money(agent.budget_microusd) : '—'}{t(". Общие ограничения организации также действуют.")}</small></label>
      <div className="form-columns"><label className="field">{t("Предел попыток")}<input aria-label={t("Предел попыток")} type="number" min={1} max={10} value={maxRuns} onChange={event=>setMaxRuns(event.target.value)} disabled={busy}/></label><label className="field">{t("Срок выполнения, часы")}<input aria-label={t("Срок выполнения, часы")} type="number" min={1} max={720} value={hours} onChange={event=>setHours(event.target.value)} disabled={busy}/></label></div>
      <p className="aside-note">{t("Повторные попытки используют общий бюджет и срок. Новая попытка потребует отдельного подтверждения.")}</p>
      {agent?.team&&<div className="notice notice-info"><p>{t("Задачу выполнит команда из")} {agent.team.members.length} {t("участников. Бюджет покрывает всю задачу и её подзадачи.")}</p></div>}
      <div className="modal-footer"><button className="button button-secondary" type="button" onClick={onClose} disabled={busy}>{t("Закрыть")}</button><button className="button button-primary" type="submit" disabled={busy || !published.length}>{busy ? t("Создаём задачу…") : t("Создать задачу")}<Icon name="arrow" /></button></div>
    </form>
  </Modal>
}

export function AgentForm({ api, onClose, onCreated }: { api: ApiClient; onClose: () => void; onCreated: () => void }) {
  const [name, setName] = useState('')
  const [mode,setMode]=useState<'single'|'team'>('single')
  const [team,setTeam]=useState<TeamDefinition>({pattern:'sequence',members:[{id:'member-1',agent_id:'',agent_version:1},{id:'member-2',agent_id:'',agent_version:1}],join_policy:'all',max_depth:1,max_children:2,max_parallel:1})
  const [instructions, setInstructions] = useState('')
  const [model, setModel] = useState('')
  const modelExplicitlySelected = useRef(false)
  const [models, setModels] = useState<AvailableModel[]>([])
  const [modelCursor, setModelCursor] = useState<string | undefined>()
  const [modelError, setModelError] = useState<unknown>(null)
  const [modelLoading, setModelLoading] = useState(false)
  useEffect(() => {
    const controller = new AbortController()
    setModelLoading(true)
    void api.models(undefined, controller.signal).then(page => {
      if (controller.signal.aborted) return
      page.data.forEach(validateModelProfile)
      setModels(page.data); setModelCursor(page.next_cursor); setModelError(null)
      setModel(current => chooseModelSelection(current, modelExplicitlySelected.current, page.data))
    }).catch(cause => { if (!controller.signal.aborted) setModelError(cause) }).finally(() => { if (!controller.signal.aborted) setModelLoading(false) })
    return () => controller.abort()
  }, [api])
  async function loadModels() {
    setModelLoading(true)
    try { const page = await api.models(modelCursor);page.data.forEach(validateModelProfile); setModels(current => [...new Map([...current, ...page.data].map(item => [item.id, item])).values()]); setModelCursor(page.next_cursor); setModelError(null); setModel(current => chooseModelSelection(current, modelExplicitlySelected.current, page.data)) }
    catch (cause) { setModelError(cause) } finally { setModelLoading(false) }
  }
  const selectedModel = models.find(item => item.model === model)
  const [tools, setTools] = useState<string[]>([])
  const [catalog,setCatalog]=useState<ToolInfo[]>([]),[toolCursor,setToolCursor]=useState<string|undefined>(),[toolError,setToolError]=useState<unknown>(null),[toolLoading,setToolLoading]=useState(false)
  useEffect(()=>{const controller=new AbortController();setToolLoading(true);void api.tools(undefined,controller.signal).then(page=>{setCatalog(page.data.filter(tool=>tool.active));setToolCursor(page.next_cursor);setToolError(null)}).catch(cause=>{if(!controller.signal.aborted)setToolError(cause)}).finally(()=>setToolLoading(false));return()=>controller.abort()},[api])
  async function loadTools(){setToolLoading(true);try{const page=await api.tools(toolCursor);setCatalog(current=>[...new Map([...current,...page.data.filter(tool=>tool.active)].map(item=>[item.id,item])).values()]);setToolCursor(page.next_cursor);setToolError(null)}catch(cause){setToolError(cause)}finally{setToolLoading(false)}}
  const [budget, setBudget] = useState('1.00')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  async function submit(event: FormEvent) {
    event.preventDefault()
    try {
      if (!name.trim() || !instructions.trim() || (mode === 'single' && !model.trim())) throw new Error('Заполните название, инструкции и модель.')
      if (byteLength(name.trim()) > 120) throw new Error('Название слишком длинное: максимум 120 байт.')
      if (byteLength(instructions.trim()) > 32 * 1024) throw new Error('Инструкции слишком большие: максимум 32 КиБ.')
      const amount = parseBudget(budget)
      if(mode==='team')validateTeamForm(team)
      setBusy(true); setError(null)
      await api.createAgent({ name: name.trim(), instructions: instructions.trim(), model: mode==='team'?'team:coordinator':model.trim(), tools: mode==='team'||model.startsWith('fixture:')?[]:tools, budget_microusd: amount, ...(mode==='team'?{team}: {}) })
      onCreated()
    } catch (cause) { setError(cause) } finally { setBusy(false) }
  }
  return <Modal title={t("Создать агента")} description={t("Сначала сохраните черновик, затем опубликуйте версию для запуска задач.")} onClose={onClose} busy={busy}>
    <form onSubmit={submit} className="modal-form">
      <ErrorNotice error={error} />
      {mode === 'single' && name === '' && instructions === '' && <div className="agent-starter">
        <p>{t('Для резюме, инструкций и разбора заявок можно начать с готовых инструкций. Их можно изменить перед сохранением.')}</p>
        <button type="button" className="button button-secondary" disabled={busy} onClick={() => {setName(t(sourceTextStarter.name));setInstructions(t(sourceTextStarter.instructions))}}>{t('Использовать заготовку агента')}</button>
      </div>}
      <label className="field">{t("Тип исполнителя")}<select aria-label={t("Тип исполнителя")} value={mode} onChange={event=>setMode(event.target.value as 'single'|'team')} disabled={busy}><option value="single">{t("Один агент")}</option><option value="team">{t("Команда агентов")}</option></select></label>
      <label className="field">{t("Название")}<input value={name} onChange={event => setName(event.target.value)} placeholder={t("Например, редактор отчётов")} required disabled={busy} autoFocus /></label>
      <label className="field">{t("Инструкции")}<textarea value={instructions} onChange={event => setInstructions(event.target.value)} rows={5} placeholder={t("Опишите роль, порядок работы и требования к результату…")} required disabled={busy} /></label>
      <div className="form-columns">{mode==='single'?<label className="field">{t("Модель")}<select aria-label={t("Модель")} value={model} onChange={event => { modelExplicitlySelected.current = true; setModel(event.target.value) }} required disabled={busy}><option value="" disabled>{t("Выберите модель")}</option><option value="fixture:echo">{t("Тестовый исполнитель · fixture:echo")}</option>{models.map(item => <option key={item.id} value={item.model}>{item.model} · v{item.version}</option>)}</select><small>{t("Доступные подключения задаёт администратор.")}</small></label>:<div className="field"><span>{t("Исполнение")}</span><strong>{t("Координатор команды")}</strong><small>{t("Модели вызывают выбранные участники.")}</small></div>}<label className="field">{t("Лимит задачи, USD")}<div className="currency-input"><span>$</span><input aria-label={t("Лимит задачи, USD")} inputMode="decimal" value={budget} onChange={event => setBudget(event.target.value)} required disabled={busy} /></div></label></div>
      <ErrorNotice error={modelError} onRetry={() => void loadModels()} />
      {mode === 'single' && !modelLoading && !modelError && models.length === 0 && !modelCursor && <div className="notice notice-info"><div>
        <strong>{t('Для полезного ответа подключите модель')}</strong>
        <p>{t('В каталоге пока нет подключённых моделей. Тестовый исполнитель только проверяет платформу. Администратор может подключить локальную модель или OpenRouter по инструкции.')}</p>
        <a href="https://github.com/ArtAndrew/RunVerge/blob/main/docs/LOCAL-MODELS.md" target="_blank" rel="noopener noreferrer">{t('Локальная модель')}</a>{' · '}<a href="https://github.com/ArtAndrew/RunVerge/blob/main/docs/OPENROUTER.md" target="_blank" rel="noopener noreferrer">OpenRouter</a>
      </div></div>}
      {modelCursor && <button type="button" className="button button-secondary" disabled={modelLoading} onClick={() => void loadModels()}>{t("Загрузить ещё модели")}</button>}
      {mode==='single' && selectedModel && <div className="muted"><p>{modelTariffLabel(selectedModel)}</p><details><summary>{t("Ограничения модели")}</summary><p>{modelResourceLabel(selectedModel)}</p><p>{selectedModel.bound_assumptions}</p></details><p>{t("Денежный бюджет также ограничивает платные инструменты и подзадачи. Число попыток и срок всей работы задаются отдельно при создании задачи.")}</p></div>}
      {mode==='single' && model.startsWith('fixture:') && <div className="notice notice-info"><p><strong>{t("Тестовый исполнитель.")}</strong> {t("Модели fixture проверяют платформу без обращения к LLM. Доступность других моделей зависит от настроек сервера.")}</p></div>}
      {mode==='team'&&<TeamFields api={api} value={team} onChange={setTeam} disabled={busy}/>}
      {mode==='single'&&<fieldset className="field"><legend>{t("Инструменты")} <span className="optional">{t("необязательно · до 16")}</span></legend><ErrorNotice error={toolError} onRetry={()=>void loadTools()}/>{model.startsWith('fixture:')?<p className="muted">{t("Тестовый исполнитель не вызывает инструменты. Выберите подключённую модель для работы с ними.")}</p>:catalog.length?catalog.map(tool=><label className="acknowledgment" key={tool.id}><input type="checkbox" checked={tools.includes(tool.id)} onChange={event=>setTools(current=>event.target.checked?[...current,tool.id]:current.filter(id=>id!==tool.id))} disabled={busy||(!tools.includes(tool.id)&&tools.length>=16)}/><span><strong>{tool.name} · v{tool.version}</strong><small>{tool.description} {t("· до")} {money(tool.max_charge_microusd)} {t("за действие")}</small></span></label>):<p className="muted">{t("Зарегистрированных инструментов пока нет. Администратор может добавить подключение.")}</p>}{toolCursor&&<button type="button" className="button button-secondary" disabled={toolLoading||busy} onClick={()=>void loadTools()}>{t("Загрузить ещё инструменты")}</button>}<small>{t("Сервер закрепит точные версии и схемы. Модель предлагает действия; текущие права, согласование и бюджет проверяются отдельно.")}</small></fieldset>}
      <div className="modal-footer"><button className="button button-secondary" type="button" onClick={onClose} disabled={busy}>{t("Закрыть")}</button><button className="button button-primary" type="submit" disabled={busy}>{busy ? t("Сохраняем…") : t("Сохранить черновик")}<Icon name="check" /></button></div>
    </form>
  </Modal>
}

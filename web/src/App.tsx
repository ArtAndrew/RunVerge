import {t, useLocale, setLocale, supportedLocales, useLocaleLoading} from './i18n'
import {API_VERSION} from '../../packages/typescript-sdk/src/events.ts'
import { useEffect, useRef, useState, lazy, Suspense } from 'react'
import { ApiClient, ApiError, browserAuth, normalizeBaseUrl, type BrowserSession, type Organization, type Agent, type Connection, type Task, type CurrentIdentity } from './api'
import AuthPanel from './AuthPanel'
import FirstTaskGuide from './FirstTaskGuide'
import OrganizationMembers from './OrganizationMembers'
import OrganizationSecurity from './OrganizationSecurity'
const OrganizationConfiguration=lazy(()=>import('./OrganizationConfiguration'))
const ProductCapabilities=lazy(()=>import('./ProductCapabilities'))
import {canAdminSecurity} from './security'
import {canAdminMembers} from './members'
import {DraftVault,sameIdentity,type DraftScope} from './draftVault'
import { AgentForm, ConnectionForm, TaskForm } from './forms'
import { dateTime, money, number, terminalStatuses } from './format'
import TaskDetails from './TaskDetails'
import { BudgetBar, EmptyState, ErrorNotice, Icon, Loading, Status } from './ui'

type View = 'tasks' | 'agents' | 'connection' | 'members' | 'security' | 'configuration' | 'capabilities'
const organizationStorageKey = 'agent-platform.organization'
function savedOrganization() { try { return localStorage.getItem(organizationStorageKey) ?? '' } catch { return '' } }
function uniqueById<T extends { id: string }>(items: T[]) { return [...new Map(items.map(item => [item.id, item])).values()] }

export default function App() {
  const locale = useLocale()
  const {busy: languageBusy, error: languageError} = useLocaleLoading()
  const [view, setView] = useState<View>('tasks')
  const [mobileNav, setMobileNav] = useState(false)
  const [oidcEnabled, setOidcEnabled] = useState<boolean | null>(null)
  const [session, setSession] = useState<BrowserSession | null>(null)
  const [organizations, setOrganizations] = useState<Organization[]>([])
  const [organizationCursor, setOrganizationCursor] = useState<string | undefined>()
  const [authError, setAuthError] = useState<unknown>(null)
  const [authRefresh, setAuthRefresh] = useState(0)
  const [authBusy, setAuthBusy] = useState(false)
  const [connection, setConnection] = useState<Connection | null>(null)
  const vault=useRef(new DraftVault()).current
  const [draftScope,setDraftScope]=useState<DraftScope|null>(null)
  const [api, setApi] = useState<ApiClient | null>(null)
  const [identity,setIdentity]=useState<CurrentIdentity|null>(null)
  const [connecting, setConnecting] = useState(false)
  const [connectError, setConnectError] = useState<unknown>(null)
  const [agents, setAgents] = useState<Agent[]>([])
  const [tasks, setTasks] = useState<Task[]>([])
  const [taskCursor, setTaskCursor] = useState<string | undefined>()
  const [agentCursor, setAgentCursor] = useState<string | undefined>()
  const [loadingMore, setLoadingMore] = useState(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [refresh, setRefresh] = useState(0)
  const [selectedTask, setSelectedTask] = useState<string | null>(null)
  const [modal, setModal] = useState<'task' | 'agent' | null>(null)
  const [filter, setFilter] = useState('all')
  const [search, setSearch] = useState('')
  const [announcement, setAnnouncement] = useState('')
  const activeClient = useRef<ApiClient | null>(null)
  const connectAttempt = useRef(0)
  const taskPages = useRef(1)
  const agentPages = useRef(1)

  useEffect(()=>{const handler=(event:BeforeUnloadEvent)=>{if(vault.size){event.preventDefault();event.returnValue=''}};window.addEventListener('beforeunload',handler);return()=>window.removeEventListener('beforeunload',handler)},[vault])

  useEffect(() => {
    let stopped = false
    async function initialize() {
      setAuthBusy(true); setAuthError(null)
      try {
        const config = await browserAuth.config()
        if (stopped) return
        setOidcEnabled(config.oidc_enabled)
        if (!config.oidc_enabled) return
        const current = await browserAuth.session()
        const page = await browserAuth.organizations()
        if (stopped) return
        setSession(current); setOrganizations(page.data); setOrganizationCursor(page.next_cursor)
        const choice = page.data.find(org => org.id === savedOrganization()) ?? (page.data.length === 1 && !page.next_cursor ? page.data[0] : undefined)
        if (choice) await connect({ baseUrl: '/api/v1', organizationId: choice.id, token: '', mode: 'session', csrfToken: current.csrf_token })
        else setView('connection')
      } catch (cause) {
        if (stopped) return
        if (cause instanceof ApiError && cause.code === 'unauthenticated') { disconnect(); setSession(null); setOrganizations([]); setOrganizationCursor(undefined); setView('connection') }
        else setAuthError(cause)
      } finally { if (!stopped) setAuthBusy(false) }
    }
    void initialize()
    return () => { stopped = true }
  }, [authRefresh])

  useEffect(() => {
    if (!api) return
    const controller = new AbortController()
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    async function load() {
      try {
        const identity=await api!.currentIdentity(controller.signal)
        if(!draftScope||!sameIdentity(draftScope,identity))throw new ApiError(t('Учётная запись изменилась. Подключите пространство заново.'),'unauthenticated')
        const readTasks = async () => {
          let page = await api!.tasks(undefined, controller.signal)
          const items = [...page.data]
          for (let index = 1; index < taskPages.current && page.next_cursor; index++) {
            page = await api!.tasks(page.next_cursor, controller.signal)
            items.push(...page.data)
          }
          return { data: uniqueById(items), next_cursor: page.next_cursor }
        }
        const readAgents = async () => {
          let page = await api!.agents(undefined, controller.signal)
          const items = [...page.data]
          for (let index = 1; index < agentPages.current && page.next_cursor; index++) {
            page = await api!.agents(page.next_cursor, controller.signal)
            items.push(...page.data)
          }
          return { data: uniqueById(items), next_cursor: page.next_cursor }
        }
        const [taskPage, agentPage] = await Promise.all([readTasks(), readAgents()])
        if (stopped) return
        setIdentity(identity);setView(current=>(current==='members'&&!canAdminMembers(identity))||(current==='security'&&!canAdminSecurity(identity))?'tasks':current)
        // Re-read every loaded page, including its current authorization decisions.
        setTasks(taskPage.data); setTaskCursor(taskPage.next_cursor)
        setAgents(agentPage.data); setAgentCursor(agentPage.next_cursor)
        setError(null)
      } catch (cause) {
        if (stopped || controller.signal.aborted) return
        setError(cause)
        if (cause instanceof ApiError && ['unauthenticated', 'forbidden'].includes(cause.code)) {
          setTasks([]); setAgents([]); setTaskCursor(undefined); setAgentCursor(undefined); disconnect();setView('connection')
        }
      } finally {
        if (!stopped) { setLoading(false); timer = setTimeout(load, 15000) }
      }
    }
    void load()
    return () => { stopped = true; controller.abort(); clearTimeout(timer) }
  }, [api, refresh])

  async function connect(next: Connection) {
    const attempt = ++connectAttempt.current
    setConnecting(true); setConnectError(null); setApi(null); setConnection(null); setIdentity(null)
    activeClient.current = null; taskPages.current = 1; agentPages.current = 1
    setTasks([]); setAgents([]); setSelectedTask(null); setError(null)
    try {
      const client = new ApiClient(next)
      const identity=await client.currentIdentity()
      const scope={server:new URL(normalizeBaseUrl(next.baseUrl),window.location.origin).toString().replace(/\/+$/,''),principal:identity.principal_id,organization:identity.organization_id}
      const [taskPage, agentPage] = await Promise.all([client.tasks(), client.agents()])
      if (attempt !== connectAttempt.current) return
      setTasks(taskPage.data); setTaskCursor(taskPage.next_cursor)
      setAgents(agentPage.data); setAgentCursor(agentPage.next_cursor)
      activeClient.current = client; setConnection(next); setDraftScope(scope); setApi(client); setIdentity(identity)
      try { localStorage.setItem(organizationStorageKey, next.organizationId) } catch { /* Storage is optional. */ }
      setView('tasks'); setAnnouncement('Рабочее пространство подключено.')
    } catch (cause) { if (attempt === connectAttempt.current) setConnectError(cause) } finally { if (attempt === connectAttempt.current) setConnecting(false) }
  }

  function disconnect(explicit=false) {
    if(explicit&&vault.size&&!window.confirm(t("В памяти есть несохранённые правки. Удалить их и отключиться?")))return
    if(explicit)vault.clear()
    setDraftScope(null);setIdentity(null)
    connectAttempt.current += 1
    activeClient.current = null; taskPages.current = 1; agentPages.current = 1
    setApi(null); setConnection(null); setTasks([]); setAgents([]); setSelectedTask(null)
    setTaskCursor(undefined); setAgentCursor(undefined); setError(null); setConnectError(null)
    setView('tasks'); setAnnouncement('Подключение закрыто. Токен удалён из состояния приложения.')
  }

  async function endSession() {
    if (!session) { disconnect(true); return }
    if(vault.size&&!window.confirm(t("В памяти есть несохранённые правки. Удалить их и выйти?")))return
    setAuthBusy(true); setAuthError(null)
    try { await browserAuth.logout(session.csrf_token); vault.clear(); disconnect(); setSession(null); setOrganizations([]); setOrganizationCursor(undefined); setView('connection') }
    catch (cause) { setAuthError(cause) } finally { setAuthBusy(false) }
  }
  async function rotateSession() {
    if (!session) return
    setAuthBusy(true); setAuthError(null)
    try {
      const next = await browserAuth.rotate(session.csrf_token)
      setSession(next)
      if (connection?.mode === 'session') await connect({ ...connection, csrfToken: next.csrf_token })
    } catch (cause) { setAuthError(cause) } finally { setAuthBusy(false) }
  }
  async function moreOrganizations() {
    if (!organizationCursor) return
    setAuthBusy(true)
    try { const page = await browserAuth.organizations(organizationCursor); setOrganizations(current => uniqueById([...current, ...page.data])); setOrganizationCursor(page.next_cursor); setAuthError(null) }
    catch (cause) { setAuthError(cause) } finally { setAuthBusy(false) }
  }

  function navigate(next: View) { setView(next); setSelectedTask(null); setMobileNav(false) }
  function reload() { setLoading(true); setRefresh(value => value + 1) }

  async function loadMore(kind: 'tasks' | 'agents') {
    if (!api) return
    const activeApi = api
    setLoadingMore(true)
    try {
      if (kind === 'tasks') {
        const page = await activeApi.tasks(taskCursor)
        if (activeClient.current !== activeApi) return
        taskPages.current += 1
        setTasks(previous => uniqueById([...previous, ...page.data])); setTaskCursor(page.next_cursor)
      } else {
        const page = await activeApi.agents(agentCursor)
        if (activeClient.current !== activeApi) return
        agentPages.current += 1
        setAgents(previous => uniqueById([...previous, ...page.data])); setAgentCursor(page.next_cursor)
      }
      setError(null)
    } catch (cause) { if (activeClient.current === activeApi) setError(cause) } finally { setLoadingMore(false) }
  }

  const attention = tasks.filter(task => ['waiting_input', 'waiting_approval', 'blocked'].includes(task.status)).length
  const active = tasks.filter(task => ['queued', 'running'].includes(task.status)).length
  const completed = tasks.filter(task => task.status === 'succeeded').length
  const showFirstTaskGuide = !loading && !error && tasks.length === 0 && !taskCursor && Boolean(identity?.permissions.includes('tasks.create'))
  const publishedAgents = agents.filter(agent => agent.published)
  const filteredTasks = tasks.filter(task => {
    const matchesFilter = filter === 'all' || (filter === 'active' && !terminalStatuses.has(task.status)) || (filter === 'attention' && ['waiting_input', 'waiting_approval', 'blocked'].includes(task.status)) || (filter === 'completed' && terminalStatuses.has(task.status))
    return matchesFilter && `${task.goal} ${task.id}`.toLocaleLowerCase(locale).includes(search.toLocaleLowerCase(locale))
  })
  const title = view === 'tasks' ? t("Задачи") : view === 'agents' ? t("Агенты") : view === 'members' ? t("Участники") : view === 'security' ? t("Безопасность") : view === 'configuration' ? t("Настройки подключений") : view === 'capabilities' ? t("Возможности") : t("Подключение")

  return <div className="app-shell">
    <a href="#main" className="skip-link">{t("К содержимому")}</a>
    {mobileNav && <button className="nav-scrim" aria-label={t("Закрыть навигацию")} onClick={() => setMobileNav(false)} />}
    <aside className={`sidebar ${mobileNav ? 'is-open' : ''}`} aria-label={t("Основная навигация")}>
      <a className="brand" href="#" onClick={event => { event.preventDefault(); navigate('tasks') }}><span className="brand-mark"><span /><span /><span /></span><div>RunVerge<small>Workspace</small></div></a>
      <div className="workspace-switch"><span className="workspace-avatar">W</span><div><strong>{t("Рабочее пространство")}</strong><small>{organizations.find(org => org.id === connection?.organizationId)?.name ?? (connection ? t("Организация подключена") : t("Выберите организацию"))}</small></div></div>
      <div className="nav-caption">{t("РАБОТА")}</div>
      <nav><button className={`nav-item ${view === 'tasks' ? 'active' : ''}`} onClick={() => navigate('tasks')} aria-current={view === 'tasks' ? 'page' : undefined}><Icon name="tasks" />{t("Задачи")}{attention > 0 && <span className="nav-count">{attention}</span>}</button><button className={`nav-item ${view === 'agents' ? 'active' : ''}`} onClick={() => navigate('agents')} aria-current={view === 'agents' ? 'page' : undefined}><Icon name="agents" />{t("Агенты")}</button></nav>
      <div className="sidebar-bottom">{api&&identity&&<button className={`nav-item ${view==='capabilities'?'active':''}`} onClick={()=>navigate('capabilities')} aria-current={view==='capabilities'?'page':undefined}><Icon name="file"/>{t("Возможности")}</button>}{api&&identity&&<button className={`nav-item ${view==='configuration'?'active':''}`} onClick={()=>navigate('configuration')} aria-current={view==='configuration'?'page':undefined}><Icon name="settings"/>{t("Настройки подключений")}</button>}{canAdminSecurity(identity)&&<button className={`nav-item ${view === 'security' ? 'active' : ''}`} onClick={()=>navigate('security')} aria-current={view === 'security' ? 'page' : undefined}><Icon name="shield"/>{t("Безопасность")}</button>}{canAdminMembers(identity)&&<button className={`nav-item ${view === 'members' ? 'active' : ''}`} onClick={() => navigate('members')} aria-current={view === 'members' ? 'page' : undefined}><Icon name="shield"/>{t("Участники")}</button>}<div className="sidebar-note"><Icon name="shield" /><p>{t("Права, расходы и ход работы — под контролем сервера.")}</p></div><button className={`nav-item ${view === 'connection' ? 'active' : ''}`} onClick={() => navigate('connection')} aria-current={view === 'connection' ? 'page' : undefined}><Icon name="settings" />{t("Подключение")}</button><div className="org-footer"><span className={`connection-dot ${api ? 'connected' : ''}`} /><div><strong>{api ? t("Подключено") : t("Нет подключения")}</strong><small title={connection?.organizationId}>{connection?.organizationId ?? t("Добавьте организацию")}</small></div></div></div>
    </aside>
    <div className="main-shell">
      <header className="topbar"><div className="breadcrumb"><button className="icon-button mobile-menu" onClick={() => setMobileNav(true)} aria-label={t("Открыть навигацию")}><Icon name="menu" /></button><span>Workspace</span><span className="breadcrumb-divider">/</span><strong>{title}</strong></div><div className="topbar-right"><label><span className="sr-only">{t("Язык интерфейса")}</span><select aria-label="Interface language" value={locale} disabled={languageBusy} aria-busy={languageBusy} onChange={event => void setLocale(event.target.value)} style={{maxWidth:110, padding:"5px", borderRadius:6, border:"1px solid #dbe3df", background:"#fff", color:"#344b40"}}>{supportedLocales.map(item => <option key={item.code} value={item.code}>{item.name}</option>)}</select></label><span className={`connection-indicator ${api ? 'connected' : ''}`}><span />{api ? t("API подключён") : authBusy ? t("Проверяем вход") : t("Не подключено")}</span><span className="version-pill">{t("Версия ·")} {API_VERSION}</span></div></header>
      <main id="main" tabIndex={-1}>
        <div className="sr-only" role="status">{t(announcement)}</div>
        {languageError && <ErrorNotice error={new Error(t("Не удалось загрузить язык. Проверьте подключение и выберите язык ещё раз."))} />}
        {view !== 'connection' && <ErrorNotice error={authError} onRetry={() => setAuthRefresh(value => value + 1)} />}
        {view === 'connection' ? <>
          <div className="page-heading"><div><div className="eyebrow">{t("РАБОЧЕЕ ПРОСТРАНСТВО")}</div><h1>{t("Подключение")}</h1><p>{t("Подключитесь к серверу своей организации.")}</p></div>{api && !oidcEnabled && <button className="button button-secondary" onClick={()=>disconnect(true)}>{t("Отключиться")}</button>}</div>
          {oidcEnabled === null ? <section className="panel connection-panel"><p>{t("Проверяем способ входа…")}</p><ErrorNotice error={authError} onRetry={() => setAuthRefresh(value => value + 1)} /></section> : oidcEnabled ? <AuthPanel session={session} organizations={organizations} selected={connection?.organizationId} busy={authBusy || connecting} error={authError || connectError} more={Boolean(organizationCursor)} onRefresh={() => setAuthRefresh(value => value + 1)} onSelect={id => { if (session) void connect({ baseUrl: '/api/v1', organizationId: id, token: '', mode: 'session', csrfToken: session.csrf_token }) }} onMore={() => void moreOrganizations()} onRotate={() => void rotateSession()} onLogout={() => void endSession()} /> : <>
          <div className="settings-grid"><section className="panel connection-panel"><div className="panel-title"><h2>{t("Доступ к платформе")}</h2><Icon name="link" /></div><ConnectionForm initial={connection ?? { baseUrl: '/api/v1', organizationId: savedOrganization(), token: '' }} busy={connecting} error={connectError} onConnect={next => void connect(next)} /></section><aside className="connection-guide"><span className="guide-icon"><Icon name="shield" /></span><h2>{t("Ваше рабочее пространство")}</h2><p>{t("Используйте идентификатор организации и токен, выданные при настройке сервера.")}</p><ul><li>{t("Доступ к задачам определяет сервер.")}</li><li>{t("Токен не записывается в локальное хранилище.")}</li><li>{t("Организация запоминается на этом устройстве.")}</li></ul><p className="muted">{t("В этой версии вход выполняется по токену. Экран не предоставляет дополнительных полномочий.")}</p></aside></div></>}

        </> : view === 'capabilities' && api && identity ? <Suspense fallback={<Loading/>}><ProductCapabilities key={`${identity.organization_id}:${identity.principal_id}`} api={api} identity={identity} onIdentity={next=>{if(activeClient.current===api)setIdentity(next)}} onUnauthenticated={()=>{if(activeClient.current!==api)return;disconnect();setView('connection')}}/></Suspense> : view === 'configuration' && api && identity ? <Suspense fallback={<Loading/>}><OrganizationConfiguration key={`${identity.organization_id}:${identity.principal_id}`} api={api} identity={identity} onIdentity={next=>{if(activeClient.current===api)setIdentity(next)}} onUnauthenticated={()=>{if(activeClient.current!==api)return;disconnect();setView('connection')}} onOpenTask={id=>{setSelectedTask(id);setView('tasks')}}/></Suspense> : view === 'security' && api && identity ? <OrganizationSecurity key={`${identity.organization_id}:${identity.principal_id}`} api={api} identity={identity} onIdentity={next=>{if(activeClient.current!==api)return;setIdentity(next);if(!canAdminSecurity(next))setView('tasks')}} onUnauthenticated={()=>{if(activeClient.current!==api)return;disconnect();setView('connection')}}/> : view === 'members' && api && identity ? <OrganizationMembers key={`${identity.organization_id}:${identity.principal_id}`} api={api} identity={identity} onIdentity={next=>{if(activeClient.current!==api)return;setIdentity(next);if(!canAdminMembers(next))setView('tasks')}} onUnauthenticated={()=>{if(activeClient.current!==api)return;disconnect();setView('connection')}}/> : selectedTask && api && draftScope && view === 'tasks' ? <TaskDetails identity={identity} vault={vault} draftScope={draftScope} onOpenTask={setSelectedTask} key={`${connection?.organizationId}:${selectedTask}`} api={api} taskId={selectedTask} agents={agents} onBack={() => setSelectedTask(null)} onChanged={reload} /> : <>
          <div className="page-heading"><div><div className="eyebrow">{t("РАБОЧЕЕ ПРОСТРАНСТВО")}</div><h1>{title}</h1><p>{view === 'tasks' ? t("Поручайте работу и следите за проверяемым результатом.") : t("Определяйте исполнителей и публикуйте готовые версии.")}</p></div><div className="heading-actions">{api && <button className="icon-button bordered" onClick={reload} disabled={loading} aria-label={t("Обновить список")}><Icon name="refresh" className={loading ? 'spinning' : ''} /></button>}<button className="button button-primary" disabled={!api || (view === 'tasks' && !publishedAgents.length)} onClick={() => setModal(view === 'tasks' ? 'task' : 'agent')}><Icon name="plus" />{view === 'tasks' ? t("Новая задача") : t("Создать агента")}</button></div></div>
          <ErrorNotice error={error} onRetry={reload} />
          {!api ? <section className="panel welcome-panel"><div className="welcome-illustration"><div className="orbit orbit-one" /><div className="orbit orbit-two" /><span className="welcome-node node-one"><Icon name="agents" /></span><span className="welcome-node node-two"><Icon name="file" /></span><span className="welcome-node node-three"><Icon name="check" /></span><span className="welcome-center"><Icon name="tasks" /></span></div><EmptyState icon="link" title={t("Начните с подключения")} description={t("Задачи, агенты и расходы появятся после подключения к API вашей организации.")} action={<button className="button button-primary" onClick={() => navigate('connection')}>{t("Подключить пространство")}<Icon name="arrow" /></button>} /></section> : view === 'tasks' ? <>
            {showFirstTaskGuide && <FirstTaskGuide agents={agents} moreAgents={Boolean(agentCursor)} identity={identity} onCreateAgent={() => setModal('agent')} onAgents={() => navigate('agents')} onTask={() => setModal('task')} />}
            <div className="stats-grid"><div className="stat-card"><span className="stat-icon"><Icon name="activity" /></span><div><span>{t("В работе и очереди")}</span><strong>{number(active)}</strong></div></div><div className="stat-card"><span className="stat-icon warm"><Icon name="clock" /></span><div><span>{t("Нужно внимание")}</span><strong>{number(attention)}</strong></div></div><div className="stat-card"><span className="stat-icon green"><Icon name="check" /></span><div><span>{t("Завершено")}</span><strong>{number(completed)}</strong></div></div></div><div className="stats-note">{t("По загруженным задачам · обновление каждые 15 секунд")}</div>
            {!publishedAgents.length && !showFirstTaskGuide && <div className="notice notice-info"><Icon name="agents" /><div><strong>{t("Нужен опубликованный агент")}</strong><p>{t("Создайте исполнителя и опубликуйте его версию, чтобы начать работу.")}</p></div><button className="button button-small button-secondary" onClick={() => navigate('agents')}>{t("К агентам")}<Icon name="arrow" /></button></div>}
            <section className="panel task-list"><div className="list-toolbar"><div className="filter-buttons" aria-label={t("Фильтр задач")}>{[['all', t("Все")], ['active', t("Активные")], ['attention', t("Нужно внимание")], ['completed', t("Завершённые")]].map(([value, label]) => <button key={value} className={filter === value ? 'selected' : ''} aria-pressed={filter === value} onClick={() => setFilter(value)}>{label}</button>)}</div><input className="search-input" aria-label={t("Поиск по загруженным задачам")} placeholder={t("Найти задачу…")} value={search} onChange={event => setSearch(event.target.value)} /></div>
              {filteredTasks.length ? <div className="task-table-wrap"><table className="task-table"><thead><tr><th scope="col">{t("Задача")}</th><th scope="col">{t("Статус")}</th><th scope="col">{t("Расходы")}</th><th scope="col">{t("Создана")}</th><th scope="col"><span className="sr-only">{t("Открыть")}</span></th></tr></thead><tbody>{filteredTasks.map(task => <tr key={task.id}><td><button className="task-name" onClick={() => setSelectedTask(task.id)}>{task.goal}</button><div className="task-agent"><Icon name="agents" />{agents.find(agent => agent.id === task.agent_id)?.name ?? task.agent_id.slice(0, 12)}<span>v{task.agent_version}</span></div></td><td><Status status={task.status} /></td><td><BudgetBar budget={task.budget_microusd} spent={task.spent_microusd} reserved={task.reserved_microusd} /></td><td><time dateTime={task.created_at}>{dateTime(task.created_at)}</time></td><td><button className="icon-button" onClick={() => setSelectedTask(task.id)} aria-label={t("Открыть задачу: {value0}", {value0: task.goal})}><Icon name="arrow" /></button></td></tr>)}</tbody></table></div> : loading ? <Loading /> : <EmptyState title={tasks.length ? t("Ничего не найдено") : t("Первая задача — впереди")} description={tasks.length ? t("Измените поиск или фильтр. Поиск выполняется по загруженным задачам.") : t("Опишите ожидаемый результат и выберите опубликованного агента.")} />}
              <div className="list-footer"><span>{t("Загружено задач:")} {number(tasks.length)}</span>{taskCursor && <button className="button button-small button-secondary" onClick={() => void loadMore('tasks')} disabled={loadingMore}>{loadingMore ? t("Загружаем…") : t("Загрузить ещё")}</button>}</div></section>
          </> : <><AgentCatalog agents={agents} api={api} onUpdated={() => { reload(); setAnnouncement('Версия агента опубликована.') }} />{agentCursor && <div className="load-more"><button className="button button-secondary" onClick={() => void loadMore('agents')} disabled={loadingMore}>{loadingMore ? t("Загружаем…") : t("Загрузить ещё агентов")}</button></div>}</>}
        </>}
        <footer className="page-footer"><span>RunVerge Workspace</span><span>{t("Данные из API · без демонстрационных результатов")}</span></footer>
      </main>
    </div>
    {modal === 'task' && api && <TaskForm api={api} agents={agents} onClose={() => setModal(null)} onCreated={id => { setModal(null); setSelectedTask(id); reload(); setAnnouncement('Задача создана.') }} />}
    {modal === 'agent' && api && <AgentForm api={api} onClose={() => setModal(null)} onCreated={() => { setModal(null); navigate('agents'); reload(); setAnnouncement('Черновик агента сохранён.') }} />}
  </div>
}

function AgentCatalog({ agents, api, onUpdated }: { agents: Agent[]; api: ApiClient; onUpdated: () => void }) {
  const [publishing, setPublishing] = useState<string | null>(null)
  const [error, setError] = useState<unknown>(null)
  async function publish(agent: Agent) {
    setPublishing(agent.id); setError(null)
    try { await api.publishAgent(agent); onUpdated() } catch (cause) { setError(cause) } finally { setPublishing(null) }
  }
  return <><ErrorNotice error={error} />{agents.length ? <div className="agent-grid">{agents.map(agent => <article className="panel agent-card" key={agent.id}><div className="agent-card-top"><span className="agent-avatar"><Icon name="agents" /></span><span className={`status ${agent.published ? 'status-succeeded' : 'status-queued'}`}><span className="status-dot" />{agent.published ? t("Опубликован") : t("Черновик")}</span></div><h2>{agent.name}</h2><div className="agent-model">{agent.model.startsWith('fixture:') && <span className="fixture-badge">{t("Тестовый")}</span>}<code>{agent.model}</code></div><p className="agent-instructions">{agent.instructions}</p><details className="agent-more"><summary>{t("Инструкции и инструменты")}</summary><pre className="prose-output">{agent.instructions}</pre><div className="tool-chips">{agent.tools.length ? agent.tools.map(tool => <span key={tool}>{tool}</span>) : <span>{t("Без инструментов")}</span>}</div></details><div className="agent-card-footer"><div><span>{t("Лимит задачи")}</span><strong>{money(agent.budget_microusd)}</strong></div><span className="agent-version">v{agent.version}</span>{!agent.published && <button className="button button-small button-primary" disabled={publishing !== null} onClick={() => void publish(agent)}>{publishing === agent.id ? t("Публикуем…") : t("Опубликовать")}</button>}</div></article>)}</div> : <section className="panel"><EmptyState icon="agents" title={t("Создайте первого агента")} description={t("Задайте инструкции, модель и лимит расходов. Опубликованные агенты станут доступны для новых задач.")} /></section>}</>
}

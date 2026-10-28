import {configurationFacts,configurationInput} from '../../packages/typescript-sdk/src/configuration.ts'
import type * as Config from '../../packages/typescript-sdk/src/configurationTypes'
import {validateCapabilities,sameCapabilityActor,type ProductCapabilities} from './capabilities.ts'
export type {ProductCapabilities} from './capabilities.ts'
export interface Connection {
  baseUrl: string
  organizationId: string
  token: string
  mode?: 'bearer' | 'session'
  csrfToken?: string
}
import {API_VERSION,API_VERSION_HEADER,eventSequence,checkAPIVersion,validateEvent,validateEventSnapshot,validateEventBatch,eventCursor,readEventFrames} from '../../packages/typescript-sdk/src/events.ts'
import {ProtocolError} from '../../packages/typescript-sdk/src/errors.ts'
import type {TaskEventSnapshot,TaskEventBatch,TaskEventStreamFrame} from '../../packages/typescript-sdk/src/types'
export type {TaskEventSnapshot,TaskEventBatch,TaskEventStreamFrame}

// The Workspace uses the same public wire types as the TypeScript SDK.
import type {
  TaskAutomationState, ProgressSignal, CostReport,
  ModelToolProposal,ModelToolResultBinding,
  ToolInfo, ToolExecution, ToolExecutionView,
  CurrentIdentity, Material, MaterialRevision, MaterialContent, Agent, Task, Event as TaskEvent, Artifact, Page, AvailableModel, TaskCandidate, TaskDecision, ReviewDecision, TeamDefinition,
} from '../../packages/typescript-sdk/src/types'
// Forms currently submit the wire shape; the SDK's ergonomic mutation inputs
// use camelCase. Derive these field types from the shared response definitions.
type CreateTask = Pick<Task, 'goal' | 'agent_id' | 'budget_microusd'> & { max_runs?: number; execution_timeout_seconds?: number }
type CreateAgent = Pick<Agent, 'name' | 'instructions' | 'model' | 'tools' | 'budget_microusd'> & { team?: TeamDefinition }
export type { CostReport, CurrentIdentity, Material, MaterialRevision, MaterialContent, Agent, Task, TaskEvent, Artifact, Page, CreateTask, CreateAgent, AvailableModel, TaskCandidate, TaskDecision, ReviewDecision, TeamDefinition }
export interface ReviewTask { expected_version: number; candidate_id: string; artifact_id: string; artifact_version: number; content_digest: string; decision: ReviewDecision; reason?: string }
export interface CreateMaterial { name: string; media_type: Material['media_type']; content: string; base_candidate_id?: string; reason?: string }
export interface ReviseMaterial { expected_version: number; content: string; reason: string; merge_base_version?: number }
export interface PublishMaterial { expected_task_version: number; expected_material_version: number; content_digest: string; parent_candidate_id: string }
export interface RetryTask { expected_version: number; reason: string; feedback?: string; acknowledge_new_cost: true }
export type {ToolInfo, ToolExecution, ToolExecutionView}
export type {ModelToolProposal,ModelToolResultBinding}
export type {TaskAutomationState,ProgressSignal}
import {validMember,validMembershipCapabilities,validMembershipAudit,type OrganizationMember,type MembershipCapabilities,type MembershipAuditEntry,type CreateMember,type UpdateMember,type RevokeMemberCredentials} from './members.ts'
export type {OrganizationMember,MembershipCapabilities,MembershipAuditEntry,CreateMember,UpdateMember,RevokeMemberCredentials}
import {credentialFree,validLoginPolicy,validServiceAccount,validServiceKey,validSecurityAudit,type OrganizationLoginPolicy,type ServiceAccount,type ServiceKey,type OrganizationSecurityAuditEntry,type UpdateLoginPolicy,type CreateServiceAccount,type UpdateServiceAccount,type RevokeServiceKey} from './security.ts'
import {validAutomationState,type CreateAutomation,type RevokeAutomation} from './automation.ts'
export type {CreateAutomation,RevokeAutomation}
export interface PrepareTool {model_proposal_id?:string;occurrence_id:string;expected_task_version:number;candidate_id:string;artifact_id:string;artifact_version:number;content_digest:string;tool_id:string;tool_version:number;arguments_json:string;expires_in_seconds:number}
export interface ContinueModel {expected_task_version:number;candidate_id:string;artifact_id:string;artifact_version:number;content_digest:string;results:ModelToolResultBinding[];acknowledge_cost:true;reason:string}
export interface DecideTool {expected_task_version:number;expected_execution_version:number;expected_approval_version:number;intent_hash:string;decision:'approve'|'deny';reason:string}
export interface DispatchTool {expected_task_version:number;expected_execution_version:number;expected_approval_version:number;intent_hash:string;acknowledge_cost:true}
export interface ReconcileTool {expected_task_version:number;expected_execution_version:number;intent_hash:string;outcome:'succeeded'|'rejected';effect_reference:string;result:string;evidence_digest:string;reason:string}

export class ApiError extends Error {
  readonly code: string
  readonly requestId?: string
  readonly retryable: boolean

  constructor(message: string, code = 'connection_error', requestId?: string, retryable = false) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.requestId = requestId
    this.retryable = retryable
  }
}

export function normalizeBaseUrl(input: string): string {
  const value = input.trim().replace(/\/+$/, '') || '/api/v1'
  if (value.startsWith('/') && !value.startsWith('//')) {
    if (value.includes('?') || value.includes('#') || value.includes('\\')) throw new Error('Укажите путь API без параметров.')
    return value
  }
  let url: URL
  try { url = new URL(value) } catch { throw new Error('Укажите полный адрес API или путь /api/v1.') }
  const local = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname)
  if (url.protocol !== 'https:' && !(url.protocol === 'http:' && local)) {
    throw new Error('Для внешнего сервера нужен HTTPS. HTTP разрешён только для локального сервера.')
  }
  if (url.username || url.password || url.search || url.hash) throw new Error('В адресе API не должно быть пароля, параметров или фрагмента.')
  return url.toString().replace(/\/+$/, '')
}

export class ApiClient {
  private readonly connection: Connection
  private readonly pendingKeys = new Map<string, string>()
  private capabilityIdentity?: CurrentIdentity

  constructor(connection: Connection) {
    this.connection = { ...connection, baseUrl: normalizeBaseUrl(connection.baseUrl) }
    if (connection.mode === 'session' && (this.connection.baseUrl !== '/api/v1' || connection.token)) {
      throw new ApiError('Вход организации работает только на этом сервере.', 'invalid_connection')
    }
  }

  private async request<T>(path: string, init: RequestInit = {}, mutation?: unknown, resultShape: 'identified'|'tool-view'|'automation'|'member'|'policy'|'service'|'service-key' = 'identified', validateResult?: (value:unknown)=>void): Promise<{ data: T; next_cursor?: string }> {
    const headers: Record<string, string> = {
      Accept: 'application/json',
      'X-Organization-ID': this.connection.organizationId,
      [API_VERSION_HEADER]:API_VERSION,
    }
    if (this.connection.mode !== 'session') headers.Authorization = `Bearer ${this.connection.token}`
    let signature: string | undefined
    if (mutation !== undefined) {
      if (this.connection.mode === 'session') {
        if (!this.connection.csrfToken) throw new ApiError('Обновите сессию перед изменением.', 'unauthenticated')
        headers['X-CSRF-Token'] = this.connection.csrfToken
      }
      headers['Content-Type'] = 'application/json'
      signature = `${path}:${JSON.stringify(mutation)}`
      const key = this.pendingKeys.get(signature) ?? crypto.randomUUID()
      this.pendingKeys.set(signature, key)
      headers['Idempotency-Key'] = key
    }
    let response: Response
    const timeout = AbortSignal.timeout(20_000)
    const signal = init.signal ? AbortSignal.any([init.signal, timeout]) : timeout
    try {
      response = await fetch(`${this.connection.baseUrl}${path}`, {
        ...init,
        signal,
        headers,
        credentials: this.connection.mode === 'session' ? 'same-origin' : 'omit',
        redirect: 'error',
        cache: 'no-store',
        ...(mutation !== undefined ? { method: 'POST', body: JSON.stringify(mutation) } : {}),
      })
    } catch (error) {
      if (init.signal?.aborted && error instanceof DOMException && error.name === 'AbortError') throw error
      throw new ApiError(
        mutation === undefined
          ? 'Не удалось связаться с API. Проверьте адрес сервера и подключение.'
          : 'Подтверждение от сервера не получено. Обновите список; повтор этой операции в текущем подключении использует тот же ключ запроса.',
        'connection_error', undefined, true,
      )
    }
    try{checkAPIVersion(response.headers)}catch{throw new ApiError('Версия сервера несовместима с этим интерфейсом. Обновите приложение; изменения автоматически не повторяются.','incompatible_client')}
    let body: { data?: T; next_cursor?: string; error?: { message?: string; code?: string; request_id?: string; retryable?: boolean } }
    try { body = await response.json() } catch {
      throw new ApiError(`Сервер вернул ответ без JSON (HTTP ${response.status}). Проверьте адрес API.`, 'invalid_response')
    }
    if (!body || typeof body !== 'object' || Array.isArray(body)) throw new ApiError('Ответ API не соответствует контракту.', 'invalid_response')
    if (!response.ok || body.error) {
      throw new ApiError(body.error?.message ?? `Запрос отклонён (HTTP ${response.status}).`, body.error?.code ?? 'request_failed', body.error?.request_id, body.error?.retryable ?? false)
    }
    if (!('data' in body)) throw new ApiError('Ответ API не соответствует контракту: отсутствует data.', 'invalid_response')
    const identified = (value:unknown):boolean => Boolean(value && typeof value==='object' && 'id' in value && typeof value.id==='string' && value.id)
    const toolView = (value:unknown):boolean => Boolean(value && typeof value==='object' && 'execution' in value && identified(value.execution) && 'task' in value && identified(value.task) && 'approval' in value && identified(value.approval) && 'tool' in value && identified(value.tool) && 'available_commands' in value && Array.isArray(value.available_commands) && value.available_commands.every(command=>typeof command==='string'))
    if (mutation !== undefined && !(resultShape==='policy'?validLoginPolicy(body.data,this.connection.organizationId):resultShape==='service'?validServiceAccount(body.data,this.connection.organizationId,path==='/service-accounts'?undefined:decodeURIComponent(path.split('/')[2])):resultShape==='service-key'?validServiceKey(body.data,this.connection.organizationId,decodeURIComponent(path.split('/')[2]),decodeURIComponent(path.split('/')[4])):resultShape==='member'?(validMember(body.data,this.connection.organizationId)&&(path==='/members'||body.data.principal_id===decodeURIComponent(path.split('/')[2]))):resultShape==='automation'?validAutomationState(body.data):resultShape==='tool-view'?toolView(body.data):identified(body.data))) {
      throw new ApiError('Сервер не подтвердил идентификатор результата. Обновите список перед повтором операции.', 'invalid_response')
    }
    if(validateResult){try{validateResult(body.data)}catch{throw new ApiError('Ответ конфигурации не прошёл проверку. Обновите состояние перед новым действием.','invalid_response')}}
    if (signature) this.pendingKeys.delete(signature)
    return body as { data: T; next_cursor?: string }
  }

  private async list<T>(path: string, signal?: AbortSignal): Promise<Page<T>> {
    const response = await this.request<T[]>(path, { signal })
    if (!Array.isArray(response.data)) throw new ApiError('API вернул некорректный список.', 'invalid_response')
    return response
  }

  async capabilities(signal?:AbortSignal):Promise<ProductCapabilities>{
    const known=this.capabilityIdentity
    const current=await this.currentIdentity(signal)
    if(known&&!sameCapabilityActor(known,current))throw new ApiError('Учётная запись изменилась. Подключите пространство заново.','unauthenticated')
    const value=(await this.request<unknown>('/capabilities',{signal})).data
    if(signal?.aborted)throw new DOMException('Чтение отменено.','AbortError')
    try{return validateCapabilities(value,this.connection.organizationId,current)}catch{throw new ApiError('Каталог возможностей не прошёл проверку версии, организации или текущей учётной записи. Обновите подключение и прочитайте каталог ещё раз.','invalid_response')}
  }

  async configurationCatalog(signal?:AbortSignal){return configurationFacts('catalog',(await this.request<unknown>('/configuration/catalog',{signal})).data,this.connection.organizationId)}
  async configurationDestinations(cursor?:string,signal?:AbortSignal){const page=await this.list<unknown>(`/configuration/destinations?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);return {...page,data:page.data.map(v=>configurationFacts('destination',v,this.connection.organizationId))}}
  async configurationResources(cursor?:string,signal?:AbortSignal){const page=await this.list<unknown>(`/configuration/resources?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);return {...page,data:page.data.map(v=>configurationFacts('resource',v,this.connection.organizationId))}}
  async configurationGrants(cursor?:string,signal?:AbortSignal){const page=await this.list<unknown>(`/configuration/grants?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);return {...page,data:page.data.map(v=>configurationFacts('grant',v,this.connection.organizationId))}}
  async configurationPlans(cursor?:string,signal?:AbortSignal){const page=await this.list<unknown>(`/configuration/plans?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);return {...page,data:page.data.map(v=>configurationFacts('plan',v,this.connection.organizationId))}}
  async configurationVerifications(cursor?:string,signal?:AbortSignal,planId?:string){const page=await this.list<unknown>(`/configuration/verifications?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}${planId?`&plan_id=${encodeURIComponent(planId)}`:''}`,signal);return {...page,data:page.data.map(v=>configurationFacts('verification',v,this.connection.organizationId,undefined,planId))}}
  async configurationAudit(cursor?:string,signal?:AbortSignal){const page=await this.list<unknown>(`/configuration/audit?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);return {...page,data:page.data.map(v=>configurationFacts('audit',v,this.connection.organizationId))}}
  async configurationGrant(grantId:string,signal?:AbortSignal){return configurationFacts('grant',(await this.request<unknown>(`/configuration/grants/${encodeURIComponent(grantId)}`,{signal})).data,this.connection.organizationId,grantId)}
  async configurationPlan(planId:string,signal?:AbortSignal){return configurationFacts('plan',(await this.request<unknown>(`/configuration/plans/${encodeURIComponent(planId)}`,{signal})).data,this.connection.organizationId,planId)}
  async configurationVerification(verificationId:string,signal?:AbortSignal){return configurationFacts('verification',(await this.request<unknown>(`/configuration/verifications/${encodeURIComponent(verificationId)}`,{signal})).data,this.connection.organizationId,verificationId)}
  async createConfigurationGrant(input:Config.CreateConfigurationGrant):Promise<Config.CapabilityEnvelope>{const payload=configurationInput('createGrant',input);return(await this.request<Config.CapabilityEnvelope>('/configuration/grants',{},payload,'identified',v=>{configurationFacts('grant',v,this.connection.organizationId)})).data}
  async revokeConfigurationGrant(grantId:string,input:Config.RevokeConfigurationGrant):Promise<Config.CapabilityEnvelope>{const payload=configurationInput('revokeGrant',input);return(await this.request<Config.CapabilityEnvelope>(`/configuration/grants/${encodeURIComponent(grantId)}/revoke`,{},payload,'identified',v=>{configurationFacts('grant',v,this.connection.organizationId,grantId)})).data}
  async createConfigurationPlan(input:Config.CreateConfigurationPlan):Promise<Config.ConfigurationPlan>{const payload=configurationInput('createPlan',input);return(await this.request<Config.ConfigurationPlan>('/configuration/plans',{},payload,'identified',v=>{configurationFacts('plan',v,this.connection.organizationId)})).data}
  async validateConfigurationPlan(planId:string,input:Config.ValidateConfigurationPlan):Promise<Config.ConfigurationValidation>{const payload=configurationInput('validatePlan',input);return(await this.request<Config.ConfigurationValidation>(`/configuration/plans/${encodeURIComponent(planId)}/validate`,{},payload,'identified',v=>{configurationFacts('validation',v,this.connection.organizationId,undefined,planId)})).data}
  async approveConfigurationPlan(planId:string,input:Config.ApproveConfigurationPlan):Promise<Config.ConfigurationApproval>{const payload=configurationInput('approvePlan',input);return(await this.request<Config.ConfigurationApproval>(`/configuration/plans/${encodeURIComponent(planId)}/approve`,{},payload,'identified',v=>{configurationFacts('approval',v,this.connection.organizationId,undefined,planId)})).data}
  async applyConfigurationPlan(planId:string,input:Config.ApplyConfigurationPlan):Promise<Config.ConfigurationPlan>{const payload=configurationInput('applyPlan',input);return(await this.request<Config.ConfigurationPlan>(`/configuration/plans/${encodeURIComponent(planId)}/apply`,{},payload,'identified',v=>{configurationFacts('plan',v,this.connection.organizationId,planId)})).data}
  async startConfigurationVerification(planId:string,input:Config.StartConfigurationVerification):Promise<Config.ConfigurationVerification>{const payload=configurationInput('verify',input);return(await this.request<Config.ConfigurationVerification>(`/configuration/plans/${encodeURIComponent(planId)}/verify`,{},payload,'identified',v=>{configurationFacts('verification',v,this.connection.organizationId,undefined,planId)})).data}

  async membershipCapabilities(signal?:AbortSignal){const value=(await this.request<MembershipCapabilities>('/members/capabilities',{signal})).data;if(!validMembershipCapabilities(value,this.connection.organizationId))throw new ApiError('Не удалось проверить доступ к управлению участниками.','invalid_response');return value}
  async members(cursor?:string,signal?:AbortSignal){const page=await this.list<OrganizationMember>(`/members?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);if(!page.data.every(v=>validMember(v,this.connection.organizationId)))throw new ApiError('Список участников не соответствует организации.','invalid_response');return page}
  async member(principal:string,signal?:AbortSignal){const value=(await this.request<OrganizationMember>(`/members/${encodeURIComponent(principal)}`,{signal})).data;if(!validMember(value,this.connection.organizationId)||value.principal_id!==principal)throw new ApiError('Не удалось проверить карточку участника.','invalid_response');return value}
  async createMember(payload:CreateMember){return(await this.request<OrganizationMember>('/members',{},payload,'member')).data}
  async updateMember(principal:string,payload:UpdateMember){return(await this.request<OrganizationMember>(`/members/${encodeURIComponent(principal)}/update`,{},payload,'member')).data}
  async revokeMemberCredentials(principal:string,payload:RevokeMemberCredentials){return(await this.request<OrganizationMember>(`/members/${encodeURIComponent(principal)}/revoke-credentials`,{},payload,'member')).data}
  async membershipAudit(principal?:string,cursor?:string,signal?:AbortSignal){const page=await this.list<MembershipAuditEntry>(`/membership-audit?limit=20${principal?`&principal_id=${encodeURIComponent(principal)}`:''}${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);if(!page.data.every(v=>validMembershipAudit(v,this.connection.organizationId)))throw new ApiError('Журнал доступа не соответствует организации.','invalid_response');return page}

  async loginPolicy(signal?:AbortSignal){const value=(await this.request<OrganizationLoginPolicy>('/login-policy',{signal})).data;if(!validLoginPolicy(value,this.connection.organizationId))throw new ApiError('Не удалось проверить политику входа.','invalid_response');return value}
  async updateLoginPolicy(payload:UpdateLoginPolicy){return(await this.request<OrganizationLoginPolicy>('/login-policy/update',{},payload,'policy')).data}
  async serviceAccounts(cursor?:string,signal?:AbortSignal){const page=await this.list<ServiceAccount>(`/service-accounts?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);if(!page.data.every(v=>validServiceAccount(v,this.connection.organizationId)))throw new ApiError('Список служебных аккаунтов не соответствует организации.','invalid_response');return page}
  async serviceAccount(principal:string,signal?:AbortSignal){const value=(await this.request<ServiceAccount>(`/service-accounts/${encodeURIComponent(principal)}`,{signal})).data;if(!validServiceAccount(value,this.connection.organizationId,principal))throw new ApiError('Не удалось проверить служебный аккаунт.','invalid_response');return value}
  async createServiceAccount(payload:CreateServiceAccount){return(await this.request<ServiceAccount>('/service-accounts',{},payload,'service')).data}
  async updateServiceAccount(principal:string,payload:UpdateServiceAccount){return(await this.request<ServiceAccount>(`/service-accounts/${encodeURIComponent(principal)}/update`,{},payload,'service')).data}
  async serviceKeys(principal:string,cursor?:string,signal?:AbortSignal){const page=await this.list<ServiceKey>(`/service-accounts/${encodeURIComponent(principal)}/keys?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);if(!page.data.every(v=>validServiceKey(v,this.connection.organizationId,principal)))throw new ApiError('Не удалось проверить метаданные ключей.','invalid_response');return page}
  async serviceKey(principal:string,key:string,signal?:AbortSignal){const value=(await this.request<ServiceKey>(`/service-accounts/${encodeURIComponent(principal)}/keys/${encodeURIComponent(key)}`,{signal})).data;if(!validServiceKey(value,this.connection.organizationId,principal,key))throw new ApiError('Не удалось проверить метаданные ключа.','invalid_response');return value}
  async revokeServiceKey(principal:string,key:string,payload:RevokeServiceKey){return(await this.request<ServiceKey>(`/service-accounts/${encodeURIComponent(principal)}/keys/${encodeURIComponent(key)}/revoke`,{},payload,'service-key')).data}
  async securityAudit(principal?:string,cursor?:string,signal?:AbortSignal){const page=await this.list<OrganizationSecurityAuditEntry>(`/security-audit?limit=20${principal?`&principal_id=${encodeURIComponent(principal)}`:''}${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal);if(!page.data.every(v=>validSecurityAudit(v,this.connection.organizationId,principal)))throw new ApiError('Журнал безопасности не соответствует запросу.','invalid_response');return page}

  async currentIdentity(signal?: AbortSignal): Promise<CurrentIdentity> {
    const value=(await this.request<CurrentIdentity>('/me',{signal})).data
    if(!credentialFree(value)||!value||typeof value.principal_id!=='string'||!value.principal_id||value.organization_id!==this.connection.organizationId||typeof value.role!=='string'||(value.principal_kind!==undefined&&!['human','service'].includes(value.principal_kind))||!Array.isArray(value.permissions)||!value.permissions.every(item=>typeof item==='string'))throw new ApiError('Не удалось подтвердить текущую учётную запись.','invalid_response')
    this.capabilityIdentity??={...value,permissions:[...value.permissions]}
    return value
  }
  tools(cursor?:string,signal?:AbortSignal){return this.list<ToolInfo>(`/tools?limit=50${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal)}
  toolExecutions(id:string,cursor?:string,signal?:AbortSignal){return this.list<ToolExecution>(`/tasks/${encodeURIComponent(id)}/tool-executions?limit=20${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal)}
  async toolExecution(id:string,execution:string,signal?:AbortSignal){return (await this.request<ToolExecutionView>(`/tasks/${encodeURIComponent(id)}/tool-executions/${encodeURIComponent(execution)}`,{signal})).data}
  async prepareTool(id:string,payload:PrepareTool){return (await this.request<ToolExecutionView>(`/tasks/${encodeURIComponent(id)}/tool-executions`,{},payload,'tool-view')).data}
  async decideTool(id:string,execution:string,payload:DecideTool){return (await this.request<ToolExecutionView>(`/tasks/${encodeURIComponent(id)}/tool-executions/${encodeURIComponent(execution)}/decide`,{},payload,'tool-view')).data}
  async dispatchTool(id:string,execution:string,payload:DispatchTool){if(payload.acknowledge_cost!==true)throw new ApiError('Подтвердите возможные расходы.','invalid_request');return (await this.request<ToolExecutionView>(`/tasks/${encodeURIComponent(id)}/tool-executions/${encodeURIComponent(execution)}/dispatch`,{},payload,'tool-view')).data}
  async reconcileTool(id:string,execution:string,payload:ReconcileTool){return (await this.request<ToolExecutionView>(`/tasks/${encodeURIComponent(id)}/tool-executions/${encodeURIComponent(execution)}/reconcile`,{},payload,'tool-view')).data}
  modelProposals(id:string,cursor?:string,signal?:AbortSignal){return this.list<ModelToolProposal>(`/tasks/${encodeURIComponent(id)}/model-tool-proposals?limit=20${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal)}
  async modelProposal(id:string,proposal:string,signal?:AbortSignal){return (await this.request<ModelToolProposal>(`/tasks/${encodeURIComponent(id)}/model-tool-proposals/${encodeURIComponent(proposal)}`,{signal})).data}
  async continueModel(id:string,payload:ContinueModel){if(payload.acknowledge_cost!==true)throw new ApiError('Подтвердите возможные расходы следующего шага.','invalid_request');return(await this.request<Task>(`/tasks/${encodeURIComponent(id)}/continue-model`,{},payload)).data}
  async automation(id:string,signal?:AbortSignal){const value=(await this.request<TaskAutomationState>(`/tasks/${encodeURIComponent(id)}/automation`,{signal})).data;if(!validAutomationState(value))throw new ApiError('Не удалось проверить состояние автоматизации.','invalid_response');return value}
  async createAutomation(id:string,payload:CreateAutomation){if(payload.acknowledge_cost!==true)throw new ApiError('Подтвердите разрешение на новые расходы.','invalid_request');return(await this.request<TaskAutomationState>(`/tasks/${encodeURIComponent(id)}/automation`,{},payload,'automation')).data}
  async revokeAutomation(id:string,payload:RevokeAutomation){return(await this.request<TaskAutomationState>(`/tasks/${encodeURIComponent(id)}/automation/revoke`,{},payload,'automation')).data}
  progressSignals(id:string,cursor?:string,signal?:AbortSignal){return this.list<ProgressSignal>(`/tasks/${encodeURIComponent(id)}/progress-signals?limit=20${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`,signal)}
  agents(cursor?: string, signal?: AbortSignal) {
    return this.list<Agent>(`/agents?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, signal)
  }
  async costs(id:string,signal?:AbortSignal){
    return (await this.request<CostReport>(`/tasks/${encodeURIComponent(id)}/costs?limit=1`,{signal})).data
  }
  models(cursor?: string, signal?: AbortSignal) {
    return this.list<AvailableModel>(`/models?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, signal)
  }
  tasks(cursor?: string, signal?: AbortSignal) {
    return this.list<Task>(`/tasks?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, signal)
  }
  async task(id: string, signal?: AbortSignal) { return (await this.request<Task>(`/tasks/${encodeURIComponent(id)}`, { signal })).data }
  async events(id: string, after = '0', signal?: AbortSignal) {
    const page=await this.list<TaskEvent>(`/tasks/${encodeURIComponent(id)}/events?after=${eventSequence(after,true)}&limit=100`, signal)
    page.data.forEach(value=>validateEvent(value,this.connection.organizationId,id));return page
  }
  async eventSnapshot(id:string,signal?:AbortSignal):Promise<TaskEventSnapshot>{return validateEventSnapshot((await this.request<TaskEventSnapshot>(`/tasks/${encodeURIComponent(id)}/event-snapshot`,{signal})).data,this.connection.organizationId,id)}
  async eventFeed(id:string,cursor='',signal?:AbortSignal):Promise<TaskEventBatch>{return validateEventBatch((await this.request<TaskEventBatch>(`/tasks/${encodeURIComponent(id)}/event-feed?cursor=${eventCursor(cursor,true)}&limit=100`,{signal})).data,this.connection.organizationId,id)}
  async *streamTaskEvents(id:string,cursor='',signal?:AbortSignal):AsyncGenerator<TaskEventStreamFrame>{
    const headers:Record<string,string>={Accept:'text/event-stream','X-Organization-ID':this.connection.organizationId,[API_VERSION_HEADER]:API_VERSION}
    if(this.connection.mode!=='session')headers.Authorization=`Bearer ${this.connection.token}`
    let response:Response|undefined
    const controller=new AbortController(),onAbort=()=>controller.abort(signal?.reason)
    signal?.addEventListener('abort',onAbort,{once:true});if(signal?.aborted)onAbort()
    const timer=setTimeout(()=>controller.abort(),20_000)
    try{
      response=await fetch(`${this.connection.baseUrl}/tasks/${encodeURIComponent(id)}/event-stream?cursor=${eventCursor(cursor,true)}`,{headers,signal:controller.signal,credentials:this.connection.mode==='session'?'same-origin':'omit',redirect:'error',cache:'no-store'})
      try{checkAPIVersion(response.headers)}catch{throw new ApiError('Версия потока несовместима с интерфейсом. Обновите приложение.','incompatible_client')}
      if(!response.ok){let body:{error?:{code?:string;message?:string;retryable?:boolean}}={};try{body=await response.json()}catch{/* Do not display a proxy response. */}throw new ApiError(body.error?.message??'Подключение к журналу отклонено.',body.error?.code??'request_failed',undefined,body.error?.retryable===true)}
      clearTimeout(timer)
      yield*readEventFrames(response,this.connection.organizationId,id,{signal:controller.signal,idleTimeoutMs:30_000})
    }catch(error){if(error instanceof ProtocolError)throw new ApiError('Поток событий не прошёл проверку. Обновите состояние перед продолжением.','invalid_event_stream');throw error}
    finally{clearTimeout(timer);signal?.removeEventListener('abort',onAbort);controller.abort();if(response?.body&&!response.body.locked)await response.body.cancel().catch(()=>undefined)}
  }
  artifacts(id: string, signal?: AbortSignal, cursor?: string) { return this.list<Artifact>(`/tasks/${encodeURIComponent(id)}/artifacts?limit=20${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, signal) }
  async artifact(id: string, artifactId: string, version: number, signal?: AbortSignal) { return (await this.request<Artifact>(`/tasks/${encodeURIComponent(id)}/artifacts/${encodeURIComponent(artifactId)}?version=${version}`, {signal})).data }
  materials(id: string, cursor?: string, signal?: AbortSignal) { return this.list<Material>(`/tasks/${encodeURIComponent(id)}/materials?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,signal) }
  async material(id: string, materialId: string, signal?: AbortSignal) { return (await this.request<Material>(`/tasks/${encodeURIComponent(id)}/materials/${encodeURIComponent(materialId)}`,{signal})).data }
  materialRevisions(id: string, materialId: string, cursor?: string, signal?: AbortSignal) { return this.list<MaterialRevision>(`/tasks/${encodeURIComponent(id)}/materials/${encodeURIComponent(materialId)}/revisions?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,signal) }
  async materialVersion(id: string, materialId: string, version: number, signal?: AbortSignal) { return (await this.request<MaterialContent>(`/tasks/${encodeURIComponent(id)}/materials/${encodeURIComponent(materialId)}/revisions/${version}`,{signal})).data }
  async createMaterial(id: string, payload: CreateMaterial) { return (await this.request<Material>(`/tasks/${encodeURIComponent(id)}/materials`,{},payload)).data }
  async reviseMaterial(id: string, materialId: string, payload: ReviseMaterial) { return (await this.request<Material>(`/tasks/${encodeURIComponent(id)}/materials/${encodeURIComponent(materialId)}/revisions`,{},payload)).data }
  async publishMaterial(id: string, materialId: string, payload: PublishMaterial) { return (await this.request<Task>(`/tasks/${encodeURIComponent(id)}/materials/${encodeURIComponent(materialId)}/publish`,{},payload)).data }
  candidates(id: string, cursor?: string, signal?: AbortSignal) { return this.list<TaskCandidate>(`/tasks/${encodeURIComponent(id)}/candidates?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, signal) }
  decisions(id: string, cursor?: string, signal?: AbortSignal) { return this.list<TaskDecision>(`/tasks/${encodeURIComponent(id)}/decisions?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, signal) }
  children(id: string, cursor?: string, signal?: AbortSignal) { return this.list<Task>(`/tasks/${encodeURIComponent(id)}/children?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, signal) }
  async reviewTask(id: string, payload: ReviewTask) { return (await this.request<Task>(`/tasks/${encodeURIComponent(id)}/review`, {}, payload)).data }
  async retryTask(id: string, payload: RetryTask) {
    if (payload.acknowledge_new_cost !== true) throw new ApiError('Подтвердите возможные новые расходы.', 'invalid_request')
    return (await this.request<Task>(`/tasks/${encodeURIComponent(id)}/retry`, {}, payload)).data
  }
  async createTask(payload: CreateTask) { return (await this.request<Task>('/tasks', {}, payload)).data }
  async cancelTask(id: string) { return (await this.request<Task>(`/tasks/${encodeURIComponent(id)}/cancel`, {}, {})).data }
  async createAgent(payload: CreateAgent) { return (await this.request<Agent>('/agents', {}, payload)).data }
  async publishAgent(agent: Agent) {
    return (await this.request<Agent>(`/agents/${encodeURIComponent(agent.id)}/publish`, {}, { expected_version: agent.version })).data
  }
}

export interface BrowserSession { session_id: string; principal_id: string; expires_at: string; csrf_token: string }
export interface Organization { id: string; name: string; role: string }

async function authRequest<T>(path: string, csrf?: string): Promise<{ data: T; next_cursor?: string }> {
  let response: Response
  try {
    response = await fetch(`/auth/${path}`, { method: csrf ? 'POST' : 'GET', credentials: 'same-origin', redirect: 'error', cache: 'no-store', signal: AbortSignal.timeout(20_000), headers: { Accept: 'application/json', ...(csrf ? { 'X-CSRF-Token': csrf } : {}) } })
  } catch { throw new ApiError('Сервис входа недоступен. Повторите проверку подключения.', 'connection_error') }
  if (response.status === 204) return { data: undefined as T }
  let body: { data?: T; next_cursor?: string; error?: { code: string; message: string; request_id?: string } }
  try { body = await response.json() } catch { throw new ApiError('Сервис входа вернул некорректный ответ.', 'invalid_response') }
  if (!body || typeof body !== 'object' || Array.isArray(body)) throw new ApiError('Некорректный ответ входа.', 'invalid_response')
  if (!response.ok || body.error) throw new ApiError(body.error?.message ?? 'Запрос входа отклонён.', body.error?.code ?? 'request_failed', body.error?.request_id)
  if (!('data' in body)) throw new ApiError('В ответе входа отсутствуют данные.', 'invalid_response')
  return body as { data: T; next_cursor?: string }
}

export const browserAuth = {
  async config(): Promise<{ oidc_enabled: boolean }> {
    const { data } = await authRequest<{ oidc_enabled: boolean }>('config')
    if (typeof data?.oidc_enabled !== 'boolean') throw new ApiError('Некорректная настройка входа.', 'invalid_response')
    return data
  },
  async session(): Promise<BrowserSession> {
    const { data } = await authRequest<BrowserSession>('session')
    if (!data || typeof data.session_id !== 'string' || typeof data.principal_id !== 'string' || typeof data.csrf_token !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(data.csrf_token) || !Number.isFinite(Date.parse(data.expires_at))) throw new ApiError('Некорректная сессия.', 'invalid_response')
    return data
  },
  async organizations(cursor?: string): Promise<Page<Organization>> {
    const page = await authRequest<Organization[]>(`organizations?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`)
    if (!Array.isArray(page.data)) throw new ApiError('Некорректный список организаций.', 'invalid_response')
    return page
  },
  async rotate(csrf: string): Promise<BrowserSession> { return (await authRequest<BrowserSession>('session/rotate', csrf)).data },
  async logout(csrf: string): Promise<void> { await authRequest<void>('logout', csrf) },
}

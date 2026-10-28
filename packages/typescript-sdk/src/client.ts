// SPDX-License-Identifier: Apache-2.0
import { AgentPlatformError, APIError, ProtocolError, TransportError } from "./errors.js";
import { parseMicroUSD } from "./money.js";
import { API_VERSION, API_VERSION_HEADER, checkAPIVersion, eventSequence, eventCursor, validateEvent, validateEventSnapshot, validateEventBatch, readEventFrames } from "./events.js";
import {credentialFree,loginPolicyFacts,serviceAccountFacts,serviceKeyFacts,securityAuditFacts,policyInput,serviceInput,keyRevokeInput} from "./security.js";
import {configurationFacts,configurationInput} from './configuration.js';
import { capabilityFacts } from './capabilities.js';
import type { ProductCapabilities } from './capabilityTypes.js';
import type * as Config from './configurationTypes.js';
import type {
  PrincipalKind, ServiceRole, LoginPolicyMode, OrganizationLoginPolicy, UpdateLoginPolicyInput, ServiceAccount, CreateServiceAccountInput, UpdateServiceAccountInput, ServiceKey, RevokeServiceKeyInput, OrganizationSecurityAuditState, OrganizationSecurityAuditEntry, SecurityAuditOptions,
  MembershipRole, OrganizationMember, MembershipCapabilities, MembershipAuditEntry, CreateMemberInput, UpdateMemberInput, RevokeMemberCredentialsInput, MembershipAuditOptions,
  AutomationToolPin, TaskAutomationState, CreateTaskAutomationInput, RevokeTaskAutomationInput, ProgressSignal,
  ModelToolProposal, ModelToolResultBinding, ContinueModelInput,
  ToolInfo, ToolExecution, ToolExecutionView, PrepareToolExecutionInput, DecideToolExecutionInput, DispatchToolExecutionInput, ReconcileToolExecutionInput,
  CurrentIdentity, Action, Agent, Approval, Artifact, AvailableModel, ClientOptions, CostOptions, CostReport,
  CreateAgentInput, CreateTaskInput, Event, EventOptions, ListOptions, TaskEventSnapshot, TaskEventBatch, TaskEventStreamFrame, EventFeedOptions, EventStreamOptions,
  MutationOptions, Operation, Page, PublishOptions, RequestOptions, Run, Task,
  Material, MaterialRevision, MaterialContent, CreateMaterialInput, ReviseMaterialInput, PublishMaterialInput, ArtifactOptions,
  ReviewTaskInput, RetryTaskInput, TaskCandidate, TaskDecision, TeamDefinition,
} from "./types.js";

const MAX_RESPONSE_BYTES = 16 * 1024 * 1024;

function nonempty(value: string, name: string): string {
  if (typeof value !== "string" || value.trim().length === 0) {
    throw new TypeError(`${name} must be a nonempty string`);
  }
  return value;
}

function header(value: string, name: string): string {
  nonempty(value, name);
  if (!/^[\x20-\x7E]+$/.test(value)) {
    throw new TypeError(`${name} must contain only printable ASCII characters`);
  }
  return value;
}

function identifier(value: string, name: string): string {
  nonempty(value, name);
  if (value === "." || value === ".." || /[/\\]/.test(value)) {
    throw new TypeError(`${name} must be a single identifier`);
  }
  return encodeURIComponent(value);
}

function integer(value: number, name: string, minimum = 0, maximum = Number.MAX_SAFE_INTEGER): number {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw new RangeError(`${name} must be a safe integer between ${minimum} and ${maximum}`);
  }
  return value;
}

function text(value: string, name: string, maximum: number): string {
  if (typeof value !== "string" || value.includes("\0")) throw new TypeError(`${name} must be text without NUL`);
  for (const character of value) {
    const cp = character.codePointAt(0)!;
    if (cp >= 0xd800 && cp <= 0xdfff) throw new TypeError(`${name} must be valid Unicode`);
  }
  if (new TextEncoder().encode(value).length > maximum) throw new RangeError(`${name} exceeds its UTF-8 byte limit`);
  return value;
}

function digest(value: string): string {
  if (typeof value !== "string" || !/^[0-9a-f]{64}(?![\s\S])/.test(value)) throw new TypeError("contentDigest must be a lowercase SHA-256 digest");
  return value;
}

function toolArguments(value: string): string {
  text(value, "argumentsJson", 32768);
  const frames:{kind:string;keys:Set<string>}[]=[];
  for(let i=0;i<value.length;i++){
    const c=value[i];
    if(c==='"'){
      const start=i;for(i++;i<value.length&&value[i]!=='"';i++)if(value[i]==='\\')i++;
      let next=i+1;while(next<value.length&&/\s/.test(value[next]!))next++;
      const frame=frames.at(-1);
      if(frame?.kind==='{'&&value[next]===':'){
        let key:unknown;try{key=JSON.parse(value.slice(start,i+1));}catch{throw new TypeError("argumentsJson must be a JSON object");}
        if(typeof key!=="string"||frame.keys.has(key))throw new TypeError("argumentsJson must not contain duplicate keys");frame.keys.add(key);
      }
    }else if(c==='{'||c==='[')frames.push({kind:c,keys:new Set()});else if(c==='}'||c===']')frames.pop();
  }
  let parsed: unknown;
  try { parsed = JSON.parse(value); } catch { throw new TypeError("argumentsJson must be a JSON object"); }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new TypeError("argumentsJson must be a JSON object");
  // Never reserialize parsed JSON: large numeric lexemes remain exact in value.
  return value;
}
function occurrence(value: string): string {
  if(typeof value!=="string" || !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}(?![\s\S])/.test(value)) throw new TypeError("occurrenceId must be a stable 1..128 character identifier");
  return value;
}
function modelResults(values: ModelToolResultBinding[]): ModelToolResultBinding[] {
  if(!Array.isArray(values)||values.length<1||values.length>8)throw new TypeError("results must bind 1..8 terminal proposals");
  const seen=new Set<string>();
  return values.map(value=>{
    if(!value||typeof value!=="object")throw new TypeError("result must be an exact terminal binding");
    const proposal=text(nonempty(value.proposal_id,"proposal_id"),"proposal_id",128);
    if(seen.has(proposal))throw new TypeError("results must not repeat a proposal");seen.add(proposal);
    const result:ModelToolResultBinding={proposal_id:proposal,execution_id:text(nonempty(value.execution_id,"execution_id"),"execution_id",128),execution_version:integer(value.execution_version,"execution_version",1),status:value.status};
    if(value.status==="succeeded"||value.status==="rejected"){
      result.receipt_id=text(nonempty(value.receipt_id!,"receipt_id"),"receipt_id",128);result.content_digest=digest(value.content_digest!);
    }else if(["denied","expired","cancelled"].includes(value.status)){
      if(value.receipt_id||value.content_digest)throw new TypeError("unsent terminal results must not claim an effect receipt");
    }else throw new TypeError("unknown or nonterminal tool effects cannot continue a model");
    return result;
  });
}
function toolMoney<T>(value: T): T {
  if (!value || typeof value!=="object" || !("max_charge_microusd" in value)) throw new ProtocolError("Missing tool charge bound");
  try {if(parseMicroUSD(value.max_charge_microusd as string)>1_000_000_000_000n)throw new RangeError("tool bound");} catch {throw new ProtocolError("Invalid tool charge bound");}
  return value;
}

function automationPins(values: AutomationToolPin[]): AutomationToolPin[] {
  if (!Array.isArray(values) || values.length < 1 || values.length > 16) throw new TypeError("toolPins must contain 1..16 exact pins");
  const seen = new Set<string>();
  return values.map(pin => {
    if (!record(pin)) throw new TypeError("tool pin must be an object");
    const id = text(nonempty(pin.tool_id, "tool_id"), "tool_id", 128);
    if (seen.has(id)) throw new TypeError("toolPins must not repeat a tool");
    seen.add(id);
    return {tool_id:id, tool_version:integer(pin.tool_version,"tool_version",1), tool_profile_digest:digest(pin.tool_profile_digest)};
  });
}

function automationCharge(value: string): string {
  const amount = parseMicroUSD(value);
  if (amount < 1n || amount > 1_000_000_000_000n) throw new RangeError("maxChargeMicrousd must be a canonical decimal string between 1 and 1000000000000");
  return value;
}

function automationState(envelope: Record<string, unknown>): TaskAutomationState {
  const state = object<TaskAutomationState>(envelope);
  if (!record(state.usage) || !Number.isSafeInteger(state.usage.model_turns) || state.usage.model_turns < 0 || !Number.isSafeInteger(state.usage.tool_executions) || state.usage.tool_executions < 0 || !Array.isArray(state.signals) || !state.signals.every(record) || !Array.isArray(state.available_commands) || !state.available_commands.every(command=>typeof command === "string")) throw new ProtocolError("Invalid automation state");
  responseMoney(state.usage.allocated_microusd);
  if (state.grant !== undefined) {
    if (!record(state.grant) || typeof state.grant.id !== "string" || !state.grant.id || !Number.isSafeInteger(state.grant.version) || state.grant.version < 1) throw new ProtocolError("Invalid automation grant");
    try { automationCharge(state.grant.max_charge_microusd); } catch { throw new ProtocolError("Invalid automation charge ceiling"); }
  }
  return state;
}
function toolView(envelope: Record<string, unknown>): ToolExecutionView {
  const value=object<ToolExecutionView>(envelope);
  for(const nested of [value.execution,value.tool,value.approval,value.task]){
    if(!nested||typeof nested!=="object"||!("id" in nested)||typeof nested.id!=="string"||!nested.id)throw new ProtocolError("Missing identified tool view component");
  }
  toolMoney(value.execution);toolMoney(value.tool);
  taskFacts(value.task as unknown as Record<string,unknown>);
  if(!Array.isArray(value.available_commands)||!value.available_commands.every(command=>typeof command==="string"))throw new ProtocolError("Invalid available tool commands");
  return value;
}

function teamDefinition(value: TeamDefinition): TeamDefinition {
  if (!value || !["sequence", "parallel", "delegate_verify"].includes(value.pattern) || !["all", "allow_partial"].includes(value.join_policy)) throw new TypeError("unsupported team pattern or join policy");
  if (!Array.isArray(value.members) || value.members.length < 1 || value.members.length > 32) throw new RangeError("team requires 1..32 members");
  if (value.pattern === "delegate_verify" && value.members.length !== 2) throw new RangeError("delegate_verify requires two members");
  integer(value.max_depth, "max_depth", 1, 16); integer(value.max_children, "max_children", 1, 32);
  if (value.max_parallel !== undefined) integer(value.max_parallel, "max_parallel", 1, value.members.length);
  const ids = new Set<string>();
  for (const member of value.members) {
    nonempty(member.id, "member.id"); nonempty(member.agent_id, "member.agent_id"); integer(member.agent_version, "member.agent_version", 1);
    if (ids.has(member.id)) throw new TypeError("team member IDs must be unique");
    ids.add(member.id);
    if (member.task !== undefined) text(nonempty(member.task, "member.task"), "member.task", 4096);
  }
  return value;
}

function record(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function object<T>(envelope: Record<string, unknown>): T {
  if (!record(envelope.data)) {
    throw new ProtocolError("Expected an object in the response data envelope");
  }
  return envelope.data as T;
}

function page<T>(envelope: Record<string, unknown>): Page<T> {
  if (!Array.isArray(envelope.data) || !envelope.data.every(record)) {
    throw new ProtocolError("Expected a list of objects in the response data envelope");
  }
  const cursor = envelope.next_cursor;
  if (cursor !== undefined && cursor !== null && typeof cursor !== "string") {
    throw new ProtocolError("next_cursor must be a string or null");
  }
  return {
    data: envelope.data as T[],
    ...(typeof cursor === "string" ? { next_cursor: cursor } : {}),
  };
}

function availableModel(value: AvailableModel): AvailableModel {
  const bounded = (n:unknown,min:number,max:number) => typeof n === 'number' && Number.isSafeInteger(n) && n >= min && n <= max;
  if (!bounded(value.max_charge_microusd,0,1_000_000_000_000) ||
      (value.accounting_mode === 'operator_unmetered_local' ? value.max_charge_microusd !== 0 : value.accounting_mode !== 'metered' || value.max_charge_microusd === 0) ||
      !bounded(value.max_input_tokens,1,1_000_000_000) || !bounded(value.max_input_bytes,1,1_048_576) ||
      !bounded(value.max_output_tokens,1,1_000_000) || !bounded(value.max_output_bytes,1,262144) || !bounded(value.timeout_milliseconds,1,600000)) {
    throw new ProtocolError('Invalid model accounting mode or resource bounds');
  }
  return value;
}

function costReport(envelope: Record<string, unknown>): CostReport {
  const report = object<Record<string, unknown>>(envelope);
  for (const key of ["reservations", "entries", "incidents", "quarantines"]) {
    if (!Array.isArray(report[key]) || !report[key].every(record)) {
      throw new ProtocolError(`Cost report ${key} must be a list of objects`);
    }
  }
  for (const key of ["budget_microusd", "pending_quarantine_count"]) {
    const value = report[key];
    if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0 || (key === "budget_microusd" && value > 1_000_000_000_000)) {
      throw new ProtocolError(`Cost report ${key} must be a nonnegative safe integer`);
    }
  }
  for (const key of ["reserved_microusd", "spent_microusd", "unknown_microusd"]) responseMoney(report[key]);
  // Exact nonnegative bigint count; independent of monetary sum and pagination.
  responseMoney(report.unknown_reservation_count);
  for (const item of report.reservations as Record<string, unknown>[]) {
    if (responseMoney(item.amount_microusd) > 1_000_000_000_000n) throw new ProtocolError("Reservation exceeds the admission limit");
    if (item.actual_microusd !== undefined) responseMoney(item.actual_microusd);
  }
  for (const item of report.entries as Record<string, unknown>[]) {
    responseMoney(item.delta_reserved_microusd, true);
    responseMoney(item.delta_spent_microusd, true);
  }
  for (const item of report.incidents as Record<string, unknown>[]) responseMoney(item.amount_microusd);
  for (const item of report.quarantines as Record<string, unknown>[]) responseMoney(item.actual_microusd);
  for (const key of ["financial_hold", "incomplete"]) {
    if (typeof report[key] !== "boolean") throw new ProtocolError(`Cost report ${key} must be a boolean`);
  }
  for (const key of ["next_reservation_cursor", "next_entry_cursor", "next_incident_cursor", "next_quarantine_cursor"]) {
    if (report[key] !== undefined && typeof report[key] !== "string") {
      throw new ProtocolError(`Cost report ${key} must be a string when present`);
    }
  }
  return report as unknown as CostReport;
}

function responseMoney(value: unknown, signed = false): bigint {
  try {
    if (typeof value !== "string") throw new TypeError();
    return parseMicroUSD(value, signed);
  } catch {
    throw new ProtocolError("Expected an exact canonical decimal int64 micro-USD string");
  }
}

function taskFacts(task: Record<string, unknown>): Task {
  responseMoney(task.reserved_microusd);
  responseMoney(task.spent_microusd);
  return task as unknown as Task;
}

function abortable<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const onAbort = (): void => reject(signal.reason);
    promise.then(resolve, reject).finally(() => signal.removeEventListener("abort", onAbort));
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

async function readJson(response: Response): Promise<Record<string, unknown>> {
  const reader = response.body?.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  if (reader) {
    try {
      while (true) {
        const item = await reader.read();
        if (item.done) break;
        length += item.value.byteLength;
        if (length > MAX_RESPONSE_BYTES) {
          await reader.cancel();
          throw new ProtocolError("JSON response exceeds the 16 MiB size limit");
        }
        chunks.push(item.value);
      }
    } finally {
      reader.releaseLock();
    }
  }
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  let result: unknown;
  try {
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    result = JSON.parse(text, (_key: string, value: unknown): unknown => {
      if (typeof value === "number" &&
          (!Number.isFinite(value) || (Number.isInteger(value) && !Number.isSafeInteger(value)))) {
        throw new ProtocolError("JSON response contains a number outside JavaScript's safe range");
      }
      return value;
    });
  } catch (error) {
    if (error instanceof ProtocolError) throw error;
    throw new ProtocolError("Response is not valid UTF-8 JSON", { cause: error });
  }
  if (!record(result)) throw new ProtocolError("Response envelope must be a JSON object");
  return result;
}


function membershipRole(value: string): MembershipRole {
  if (!["owner", "admin", "member", "viewer"].includes(value)) throw new TypeError("Invalid membership role");
  return value as MembershipRole;
}
function membershipState(value: unknown): void {
  if (!record(value)) throw new TypeError("Invalid membership state");
  text(nonempty(value.display_name as string,"displayName"),"displayName",120);
  membershipRole(value.role as string); integer(value.version as number,"version",1);
  if (typeof value.active !== "boolean") throw new TypeError("active must be an explicit boolean");
}
function memberFacts(value: unknown, organizationId: string, principalId?: string): OrganizationMember {
  try {
    membershipState(value);
    if (!record(value) || value.organization_id !== organizationId || typeof value.principal_active !== "boolean") throw new TypeError();
    if(value.principal_kind!==undefined&&value.principal_kind!=="human"&&value.principal_kind!=="service")throw new TypeError("Invalid principal kind");
    identifier(value.principal_id as string,"principalId");
    if(principalId !== undefined && value.principal_id !== principalId)throw new TypeError();
    for (const field of ["created_at","updated_at"]) text(nonempty(value[field] as string,field),field,128);
    if (value.credentials_revoked_at !== undefined) text(nonempty(value.credentials_revoked_at as string,"credentialsRevokedAt"),"credentialsRevokedAt",128);
    if (!Array.isArray(value.available_commands) || !value.available_commands.every(v=>typeof v === "string") || !Array.isArray(value.assignable_roles)) throw new TypeError();
    for (const role of value.assignable_roles) membershipRole(role as string);
    return value as unknown as OrganizationMember;
  } catch { throw new ProtocolError("Invalid organization member response"); }
}
function membershipCapabilities(value: unknown, organizationId: string): MembershipCapabilities {
  try {
    if (!record(value) || value.organization_id !== organizationId || typeof value.can_create_members !== "boolean" || value.self_management_allowed !== false || !Array.isArray(value.assignable_roles)) throw new TypeError();
    for (const role of value.assignable_roles) membershipRole(role as string);
    return value as unknown as MembershipCapabilities;
  } catch { throw new ProtocolError("Invalid membership capabilities response"); }
}
function membershipAudit(value: unknown, organizationId: string): MembershipAuditEntry {
  try {
    if (!record(value) || value.organization_id !== organizationId) throw new TypeError();
    for (const field of ["id","actor_id","target_principal_id"]) identifier(value[field] as string,field);
    text(nonempty(value.reason as string,"reason"),"reason",4096); text(nonempty(value.created_at as string,"createdAt"),"createdAt",128);
    if (!["member.created","member.updated","member.credentials_revoked"].includes(value.action as string)) throw new TypeError();
    membershipState(value.after); if (value.before !== undefined) membershipState(value.before);
    return value as unknown as MembershipAuditEntry;
  } catch { throw new ProtocolError("Invalid membership audit response"); }
}

/** Synchronous configuration and asynchronous, dependency-free HTTP methods. */
export class Client {
  readonly baseUrl: string;
  readonly organizationId: string;
  readonly timeoutMs: number;
  #token: ClientOptions["token"];
  #observedPrincipalId: string | undefined;

  constructor(options: ClientOptions) {
    const url = new URL(nonempty(options.baseUrl, "baseUrl"));
    if (!["http:", "https:"].includes(url.protocol)) {
      throw new TypeError("baseUrl must be an absolute HTTP or HTTPS URL");
    }
    if (url.username || url.password || url.search || url.hash) {
      throw new TypeError("baseUrl cannot contain credentials, query, or fragment");
    }
    const basePath = url.pathname.replace(/\/+$/, "");
    url.pathname = basePath.endsWith("/api/v1") ? basePath : basePath + "/api/v1";
    this.baseUrl = url.toString().replace(/\/$/, "");
    this.organizationId = header(options.organizationId, "organizationId");
    this.timeoutMs = integer(options.timeoutMs ?? 30_000, "timeoutMs", 1, 2_147_483_647);
    if (typeof options.token !== "function") header(options.token, "token");
    this.#token = options.token;
  }

  async configurationCatalog(options: RequestOptions = {}): Promise<Config.ConfigurationCatalog> {
    return configurationFacts('catalog',object<unknown>(await this.#request('GET','/configuration/catalog',options)),this.organizationId);
  }
  /** Read descriptive support and role prerequisites; never a reusable execution permit. */
  async capabilities(options: RequestOptions = {}): Promise<ProductCapabilities> {
    const value = capabilityFacts(object<unknown>(await this.#request('GET', '/capabilities', options)), this.organizationId, this.#observedPrincipalId);
    if (typeof this.#token !== 'function') this.#observedPrincipalId = value.identity.principal_id;
    return value;
  }
  async configurationDestinations(options: ListOptions = {}): Promise<Page<Config.ConfigurationDestination>> {
    const query=this.#listQuery(options);
    const result=page<unknown>(await this.#request('GET','/configuration/destinations',options,undefined,query));
    return {...result,data:result.data.map(v=>configurationFacts('destination',v,this.organizationId))};
  }
  async configurationResources(options: ListOptions = {}): Promise<Page<Config.ConfigurationResource>> {
    const query=this.#listQuery(options);
    const result=page<unknown>(await this.#request('GET','/configuration/resources',options,undefined,query));
    return {...result,data:result.data.map(v=>configurationFacts('resource',v,this.organizationId))};
  }
  async configurationGrants(options: ListOptions = {}): Promise<Page<Config.CapabilityEnvelope>> {
    const query=this.#listQuery(options);
    const result=page<unknown>(await this.#request('GET','/configuration/grants',options,undefined,query));
    return {...result,data:result.data.map(v=>configurationFacts('grant',v,this.organizationId))};
  }
  async configurationPlans(options: ListOptions = {}): Promise<Page<Config.ConfigurationPlan>> {
    const query=this.#listQuery(options);
    const result=page<unknown>(await this.#request('GET','/configuration/plans',options,undefined,query));
    return {...result,data:result.data.map(v=>configurationFacts('plan',v,this.organizationId))};
  }
  async configurationVerifications(options: Config.ConfigurationVerificationOptions = {}): Promise<Page<Config.ConfigurationVerification>> {
    const query=this.#listQuery(options);if(options.planId!==undefined){identifier(options.planId,"planId");query.plan_id=options.planId;}
    const result=page<unknown>(await this.#request('GET','/configuration/verifications',options,undefined,query));
    return {...result,data:result.data.map(v=>configurationFacts('verification',v,this.organizationId,undefined,options.planId))};
  }
  async configurationAudit(options: ListOptions = {}): Promise<Page<Config.ConfigurationAuditEntry>> {
    const query=this.#listQuery(options);
    const result=page<unknown>(await this.#request('GET','/configuration/audit',options,undefined,query));
    return {...result,data:result.data.map(v=>configurationFacts('audit',v,this.organizationId))};
  }
  async configurationGrant(grantId:string,options:RequestOptions={}):Promise<Config.CapabilityEnvelope> {
    return configurationFacts('grant',object<unknown>(await this.#request('GET',`/configuration/grants/${identifier(grantId,'grantId')}`,options)),this.organizationId,grantId);
  }
  async configurationPlan(planId:string,options:RequestOptions={}):Promise<Config.ConfigurationPlan> {
    return configurationFacts('plan',object<unknown>(await this.#request('GET',`/configuration/plans/${identifier(planId,'planId')}`,options)),this.organizationId,planId);
  }
  async configurationVerification(verificationId:string,options:RequestOptions={}):Promise<Config.ConfigurationVerification> {
    return configurationFacts('verification',object<unknown>(await this.#request('GET',`/configuration/verifications/${identifier(verificationId,'verificationId')}`,options)),this.organizationId,verificationId);
  }
  async createConfigurationGrant(input:Config.CreateConfigurationGrant,options:MutationOptions):Promise<Config.CapabilityEnvelope> {
    return configurationFacts('grant',object<unknown>(await this.#request('POST','/configuration/grants',options,configurationInput('createGrant',input))),this.organizationId);
  }
  async revokeConfigurationGrant(grantId:string,input:Config.RevokeConfigurationGrant,options:MutationOptions):Promise<Config.CapabilityEnvelope> {
    return configurationFacts('grant',object<unknown>(await this.#request('POST',`/configuration/grants/${identifier(grantId,'grantId')}/revoke`,options,configurationInput('revokeGrant',input))),this.organizationId,grantId);
  }
  async createConfigurationPlan(input:Config.CreateConfigurationPlan,options:MutationOptions):Promise<Config.ConfigurationPlan> {
    return configurationFacts('plan',object<unknown>(await this.#request('POST','/configuration/plans',options,configurationInput('createPlan',input))),this.organizationId);
  }
  async validateConfigurationPlan(planId:string,input:Config.ValidateConfigurationPlan,options:MutationOptions):Promise<Config.ConfigurationValidation> {
    return configurationFacts('validation',object<unknown>(await this.#request('POST',`/configuration/plans/${identifier(planId,'planId')}/validate`,options,configurationInput('validatePlan',input))),this.organizationId,undefined,planId);
  }
  async approveConfigurationPlan(planId:string,input:Config.ApproveConfigurationPlan,options:MutationOptions):Promise<Config.ConfigurationApproval> {
    return configurationFacts('approval',object<unknown>(await this.#request('POST',`/configuration/plans/${identifier(planId,'planId')}/approve`,options,configurationInput('approvePlan',input))),this.organizationId,undefined,planId);
  }
  async applyConfigurationPlan(planId:string,input:Config.ApplyConfigurationPlan,options:MutationOptions):Promise<Config.ConfigurationPlan> {
    return configurationFacts('plan',object<unknown>(await this.#request('POST',`/configuration/plans/${identifier(planId,'planId')}/apply`,options,configurationInput('applyPlan',input))),this.organizationId,planId);
  }
  async startConfigurationVerification(planId:string,input:Config.StartConfigurationVerification,options:MutationOptions):Promise<Config.ConfigurationVerification> {
    return configurationFacts('verification',object<unknown>(await this.#request('POST',`/configuration/plans/${identifier(planId,'planId')}/verify`,options,configurationInput('verify',input))),this.organizationId,undefined,planId);
  }

  async membershipCapabilities(options: RequestOptions = {}): Promise<MembershipCapabilities> {
    return membershipCapabilities(object<unknown>(await this.#request("GET","/members/capabilities",options)),this.organizationId);
  }
  async listMembers(options: ListOptions = {}): Promise<Page<OrganizationMember>> {
    const result=page<unknown>(await this.#request("GET","/members",options,undefined,this.#listQuery(options)));
    return {...result,data:result.data.map(value=>memberFacts(value,this.organizationId))};
  }
  async getMember(principalId: string, options: RequestOptions = {}): Promise<OrganizationMember> {
    return memberFacts(object<unknown>(await this.#request("GET",`/members/${identifier(principalId,"principalId")}`,options)),this.organizationId,principalId);
  }
  async createMember(input: CreateMemberInput, options: MutationOptions): Promise<OrganizationMember> {
    return memberFacts(object<unknown>(await this.#request("POST","/members",options,{
      display_name:text(nonempty(input.displayName,"displayName"),"displayName",120),role:membershipRole(input.role),reason:text(nonempty(input.reason,"reason"),"reason",4096),
    })),this.organizationId);
  }
  async updateMember(principalId: string, input: UpdateMemberInput, options: MutationOptions): Promise<OrganizationMember> {
    if (typeof input.active !== "boolean") throw new TypeError("active must be an explicit boolean");
    return memberFacts(object<unknown>(await this.#request("POST",`/members/${identifier(principalId,"principalId")}/update`,options,{
      expected_version:integer(input.expectedVersion,"expectedVersion",1),display_name:text(nonempty(input.displayName,"displayName"),"displayName",120),
      role:membershipRole(input.role),active:input.active,reason:text(nonempty(input.reason,"reason"),"reason",4096),
    })),this.organizationId,principalId);
  }
  async revokeMemberCredentials(principalId: string, input: RevokeMemberCredentialsInput, options: MutationOptions): Promise<OrganizationMember> {
    return memberFacts(object<unknown>(await this.#request("POST",`/members/${identifier(principalId,"principalId")}/revoke-credentials`,options,{
      expected_version:integer(input.expectedVersion,"expectedVersion",1),reason:text(nonempty(input.reason,"reason"),"reason",4096),
    })),this.organizationId,principalId);
  }
  async listMembershipAudit(options: MembershipAuditOptions = {}): Promise<Page<MembershipAuditEntry>> {
    const query=this.#listQuery(options); if(options.principalId !== undefined){identifier(options.principalId,"principalId");query.principal_id=options.principalId;}
    const result=page<unknown>(await this.#request("GET","/membership-audit",options,undefined,query));
    return {...result,data:result.data.map(value=>membershipAudit(value,this.organizationId))};
  }

  async getLoginPolicy(options: RequestOptions = {}): Promise<OrganizationLoginPolicy> {
    return loginPolicyFacts(object<unknown>(await this.#request("GET","/login-policy",options)),this.organizationId);
  }
  async updateLoginPolicy(input: UpdateLoginPolicyInput, options: MutationOptions): Promise<OrganizationLoginPolicy> {
    return loginPolicyFacts(object<unknown>(await this.#request("POST","/login-policy/update",options,policyInput(input))),this.organizationId);
  }
  async listServiceAccounts(options: ListOptions = {}): Promise<Page<ServiceAccount>> {
    const result=page<unknown>(await this.#request("GET","/service-accounts",options,undefined,this.#listQuery(options)));
    return {...result,data:result.data.map(v=>serviceAccountFacts(v,this.organizationId))};
  }
  async getServiceAccount(principalId: string, options: RequestOptions = {}): Promise<ServiceAccount> {
    return serviceAccountFacts(object<unknown>(await this.#request("GET",`/service-accounts/${identifier(principalId,"principalId")}`,options)),this.organizationId,principalId);
  }
  async createServiceAccount(input: CreateServiceAccountInput, options: MutationOptions): Promise<ServiceAccount> {
    return serviceAccountFacts(object<unknown>(await this.#request("POST","/service-accounts",options,serviceInput(input))),this.organizationId);
  }
  async updateServiceAccount(principalId: string, input: UpdateServiceAccountInput, options: MutationOptions): Promise<ServiceAccount> {
    return serviceAccountFacts(object<unknown>(await this.#request("POST",`/service-accounts/${identifier(principalId,"principalId")}/update`,options,serviceInput(input,true))),this.organizationId,principalId);
  }
  async listServiceKeys(principalId: string, options: ListOptions = {}): Promise<Page<ServiceKey>> {
    const result=page<unknown>(await this.#request("GET",`/service-accounts/${identifier(principalId,"principalId")}/keys`,options,undefined,this.#listQuery(options)));
    return {...result,data:result.data.map(v=>serviceKeyFacts(v,this.organizationId,principalId))};
  }
  async getServiceKey(principalId: string, keyId: string, options: RequestOptions = {}): Promise<ServiceKey> {
    return serviceKeyFacts(object<unknown>(await this.#request("GET",`/service-accounts/${identifier(principalId,"principalId")}/keys/${identifier(keyId,"keyId")}`,options)),this.organizationId,principalId,keyId);
  }
  async revokeServiceKey(principalId: string, keyId: string, input: RevokeServiceKeyInput, options: MutationOptions): Promise<ServiceKey> {
    return serviceKeyFacts(object<unknown>(await this.#request("POST",`/service-accounts/${identifier(principalId,"principalId")}/keys/${identifier(keyId,"keyId")}/revoke`,options,keyRevokeInput(input))),this.organizationId,principalId,keyId);
  }
  async listSecurityAudit(options: SecurityAuditOptions = {}): Promise<Page<OrganizationSecurityAuditEntry>> {
    const query=this.#listQuery(options);if(options.principalId!==undefined){identifier(options.principalId,"principalId");query.principal_id=options.principalId;}
    const result=page<unknown>(await this.#request("GET","/security-audit",options,undefined,query));
    return {...result,data:result.data.map(v=>securityAuditFacts(v,this.organizationId,options.principalId))};
  }

  async getCurrentIdentity(options: RequestOptions = {}): Promise<CurrentIdentity> {
    const value=object<Record<string,unknown>>(await this.#request("GET", "/me", options));
    try {
      if(!credentialFree(value)||value.organization_id!==this.organizationId||(value.principal_kind!=="human"&&value.principal_kind!=="service"))throw new TypeError();
      identifier(value.principal_id as string,"principalId");membershipRole(value.role as string);
      if(!Array.isArray(value.permissions)||!value.permissions.every(v=>typeof v==="string"))throw new TypeError();
    } catch {throw new ProtocolError("Invalid current identity response");}
    if (typeof this.#token !== 'function') this.#observedPrincipalId = value.principal_id as string;
    return value as unknown as CurrentIdentity;
  }

  async createTask(input: CreateTaskInput, options: MutationOptions): Promise<Task> {
    return taskFacts(object<Record<string, unknown>>(await this.#request("POST", "/tasks", options, {
      goal: nonempty(input.goal, "goal"),
      agent_id: nonempty(input.agentId, "agentId"),
      budget_microusd: integer(input.budgetMicrousd, "budgetMicrousd", 1, 1_000_000_000_000),
      ...(input.maxRuns === undefined ? {} : { max_runs: integer(input.maxRuns, "maxRuns", 1, 10) }),
      ...(input.executionTimeoutSeconds === undefined ? {} : { execution_timeout_seconds: integer(input.executionTimeoutSeconds, "executionTimeoutSeconds", 1, 2_592_000) }),
    })));
  }

  async listTasks(options: ListOptions = {}): Promise<Page<Task>> {
    const result = page<Record<string, unknown>>(await this.#request("GET", "/tasks", options, undefined, this.#listQuery(options)));
    return {...result, data: result.data.map(taskFacts)};
  }

  async getTask(taskId: string, options: RequestOptions = {}): Promise<Task> {
    return taskFacts(object<Record<string, unknown>>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}`, options)));
  }

  async cancelTask(taskId: string, options: MutationOptions): Promise<Task> {
    return taskFacts(object<Record<string, unknown>>(await this.#request("POST", `/tasks/${identifier(taskId, "taskId")}/cancel`, options, {})));
  }

  async reviewTask(taskId: string, input: ReviewTaskInput, options: MutationOptions): Promise<Task> {
    if (!["accept", "reject", "request_changes"].includes(input.decision)) throw new TypeError("unsupported review decision");
    if (input.decision !== "accept") nonempty(input.reason!, "reason");
    return taskFacts(object<Record<string, unknown>>(await this.#request("POST", `/tasks/${identifier(taskId, "taskId")}/review`, options, {
      expected_version: integer(input.expectedVersion, "expectedVersion", 1),
      candidate_id: nonempty(input.candidateId, "candidateId"), artifact_id: nonempty(input.artifactId, "artifactId"),
      artifact_version: integer(input.artifactVersion, "artifactVersion", 1), content_digest: digest(input.contentDigest),
      decision: input.decision, ...(input.reason === undefined ? {} : { reason: text(input.reason, "reason", 4096) }),
    })));
  }

  /** A new potentially billable execution attempt; existing holds are preserved. */
  async retryTask(taskId: string, input: RetryTaskInput, options: MutationOptions): Promise<Task> {
    if (input.acknowledgeNewCost !== true) throw new TypeError("acknowledgeNewCost must explicitly be true");
    return taskFacts(object<Record<string, unknown>>(await this.#request("POST", `/tasks/${identifier(taskId, "taskId")}/retry`, options, {
      expected_version: integer(input.expectedVersion, "expectedVersion", 1), reason: text(nonempty(input.reason, "reason"), "reason", 4096),
      acknowledge_new_cost: true, ...(input.feedback === undefined ? {} : { feedback: text(input.feedback, "feedback", 8192) }),
    })));
  }

  async candidates(taskId: string, options: ListOptions = {}): Promise<Page<TaskCandidate>> {
    return page<TaskCandidate>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/candidates`, options, undefined, this.#listQuery(options)));
  }

  async decisions(taskId: string, options: ListOptions = {}): Promise<Page<TaskDecision>> {
    return page<TaskDecision>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/decisions`, options, undefined, this.#listQuery(options)));
  }

  async children(taskId: string, options: ListOptions = {}): Promise<Page<Task>> {
    const result = page<Record<string, unknown>>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/children`, options, undefined, this.#listQuery(options)));
    return {...result, data: result.data.map(taskFacts)};
  }

  async listAgents(options: ListOptions = {}): Promise<Page<Agent>> {
    return page<Agent>(await this.#request("GET", "/agents", options, undefined, this.#listQuery(options)));
  }

  /** Available organization model profiles; no credentials or endpoint secrets. */
  async models(options: ListOptions = {}): Promise<Page<AvailableModel>> {
    const result = page<AvailableModel>(await this.#request("GET", "/models", options, undefined, this.#listQuery(options)));
    result.data.forEach(availableModel);
    return result;
  }

  async createAgent(input: CreateAgentInput, options: MutationOptions): Promise<Agent> {
    const definition: Record<string, unknown> = {
      name: nonempty(input.name, "name"),
      instructions: nonempty(input.instructions, "instructions"),
    };
    if (input.model !== undefined) definition.model = nonempty(input.model, "model");
    if (input.tools !== undefined) {
      if (!Array.isArray(input.tools) || !input.tools.every(tool => typeof tool === "string")) {
        throw new TypeError("tools must be an array of strings");
      }
      definition.tools = input.tools;
    }
    if (input.budgetMicrousd !== undefined) {
      definition.budget_microusd = integer(input.budgetMicrousd, "budgetMicrousd", 1, 1_000_000_000_000);
    }
    if (input.team !== undefined) definition.team = teamDefinition(input.team);
    return object<Agent>(await this.#request("POST", "/agents", options, definition));
  }

  async publishAgent(agentId: string, options: PublishOptions): Promise<Agent> {
    return object<Agent>(await this.#request("POST", `/agents/${identifier(agentId, "agentId")}/publish`, options, {
      expected_version: integer(options.expectedVersion, "expectedVersion", 1),
    }));
  }

  /** Stored JSON events; sequences retain the complete bigint range. */
  async events(taskId: string, options: EventOptions = {}): Promise<Page<Event>> {
    const query: Record<string, string | number> = {};
    if (options.after !== undefined) query.after = eventSequence(options.after, true);
    if (options.limit !== undefined) query.limit = integer(options.limit, "limit", 1, 100);
    const result = page<Event>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/events`, options, undefined, query));
    result.data.forEach(value => validateEvent(value, this.organizationId, taskId));
    return result;
  }

  async getTaskEventSnapshot(taskId: string, options: RequestOptions = {}): Promise<TaskEventSnapshot> {
    return validateEventSnapshot(object(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/event-snapshot`, options)), this.organizationId, taskId);
  }
  async taskEventFeed(taskId: string, options: EventFeedOptions = {}): Promise<TaskEventBatch> {
    const query: Record<string, string | number> = {};
    if (options.cursor !== undefined) query.cursor = eventCursor(options.cursor, true);
    if (options.limit !== undefined) query.limit = integer(options.limit, "limit", 1, 100);
    return validateEventBatch(object(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/event-feed`, options, undefined, query)), this.organizationId, taskId);
  }
  /** One connection, no automatic reconnect. Persist only a fully processed batch cursor. */
  async *streamTaskEvents(taskId: string, options: EventStreamOptions = {}): AsyncGenerator<TaskEventStreamFrame> {
    const url = new URL(this.baseUrl + `/tasks/${identifier(taskId, "taskId")}/event-stream`);
    if (options.cursor !== undefined) url.searchParams.set("cursor", eventCursor(options.cursor, true));
    const controller = new AbortController();
    const onAbort = (): void => controller.abort(options.signal?.reason);
    options.signal?.addEventListener("abort", onAbort, { once: true });
    if (options.signal?.aborted) onAbort();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    let response: Response | undefined;
    try {
      const supplied = typeof this.#token === "function" ? this.#token() : this.#token;
      const token = await abortable(Promise.resolve(supplied), controller.signal);
      response = await fetch(url, { headers: { Accept: "text/event-stream", Authorization: "Bearer " + header(token, "token"), "X-Organization-ID": this.organizationId, [API_VERSION_HEADER]: API_VERSION }, signal: controller.signal, redirect: "error", credentials: "omit", cache: "no-store" });
      checkAPIVersion(response.headers);
      if (!response.ok) {
        const body = await readJson(response), details = record(body.error) ? body.error : {};
        throw new APIError({ statusCode: response.status, code: typeof details.code === "string" ? details.code : "http_error", message: typeof details.message === "string" ? details.message : "Event subscription rejected", retryable: details.retryable === true });
      }
      clearTimeout(timer);
      yield* readEventFrames(response, this.organizationId, taskId, { signal: controller.signal, ...(options.idleTimeoutMs === undefined ? {} : { idleTimeoutMs: options.idleTimeoutMs }) });
    } catch (error) {
      if (error instanceof AgentPlatformError) throw error;
      throw new TransportError(controller.signal.aborted ? "aborted" : "network", { cause: error });
    } finally {
      clearTimeout(timer); options.signal?.removeEventListener("abort", onAbort); controller.abort();
      if (response?.body && !response.body.locked) await response.body.cancel().catch(() => undefined);
    }
  }

  async getArtifact(taskId: string, artifactId: string, options: ArtifactOptions = {}): Promise<Artifact> {
    const query = options.version === undefined ? {} : {version: integer(options.version, "version", 1)};
    return object<Artifact>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/artifacts/${identifier(artifactId, "artifactId")}`, options, undefined, query));
  }

  async materials(taskId: string, options: ListOptions = {}): Promise<Page<Material>> {
    return page<Material>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/materials`, options, undefined, this.#listQuery(options)));
  }

  async getMaterial(taskId: string, materialId: string, options: RequestOptions = {}): Promise<Material> {
    return object<Material>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/materials/${identifier(materialId, "materialId")}`, options));
  }

  async materialRevisions(taskId: string, materialId: string, options: ListOptions = {}): Promise<Page<MaterialRevision>> {
    return page<MaterialRevision>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/materials/${identifier(materialId, "materialId")}/revisions`, options, undefined, this.#listQuery(options)));
  }

  async getMaterialVersion(taskId: string, materialId: string, version: number, options: RequestOptions = {}): Promise<MaterialContent> {
    return object<MaterialContent>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/materials/${identifier(materialId, "materialId")}/revisions/${integer(version, "version", 1, 128)}`, options));
  }

  async createMaterial(taskId: string, input: CreateMaterialInput, options: MutationOptions): Promise<Material> {
    if (!["text/plain", "text/markdown", "application/json"].includes(input.mediaType)) throw new TypeError("unsupported mediaType");
    return object<Material>(await this.#request("POST", `/tasks/${identifier(taskId, "taskId")}/materials`, options, {
      name: text(nonempty(input.name, "name"), "name", 120), media_type: input.mediaType, content: text(input.content, "content", 262144),
      ...(input.baseCandidateId === undefined ? {} : {base_candidate_id: text(nonempty(input.baseCandidateId, "baseCandidateId"), "baseCandidateId", 128)}),
      ...(input.reason === undefined ? {} : {reason: text(input.reason, "reason", 4096)}),
    }));
  }

  async reviseMaterial(taskId: string, materialId: string, input: ReviseMaterialInput, options: MutationOptions): Promise<Material> {
    const expected = integer(input.expectedVersion, "expectedVersion", 1, 128);
    return object<Material>(await this.#request("POST", `/tasks/${identifier(taskId, "taskId")}/materials/${identifier(materialId, "materialId")}/revisions`, options, {
      expected_version: expected, content: text(input.content, "content", 262144), reason: text(nonempty(input.reason, "reason"), "reason", 4096),
      ...(input.mergeBaseVersion === undefined ? {} : {merge_base_version: integer(input.mergeBaseVersion, "mergeBaseVersion", 1, expected)}),
    }));
  }

  async publishMaterial(taskId: string, materialId: string, input: PublishMaterialInput, options: MutationOptions): Promise<Task> {
    return taskFacts(object<Record<string, unknown>>(await this.#request("POST", `/tasks/${identifier(taskId, "taskId")}/materials/${identifier(materialId, "materialId")}/publish`, options, {
      expected_task_version: integer(input.expectedTaskVersion, "expectedTaskVersion", 1),
      expected_material_version: integer(input.expectedMaterialVersion, "expectedMaterialVersion", 1, 128),
      content_digest: digest(input.contentDigest), parent_candidate_id: text(nonempty(input.parentCandidateId, "parentCandidateId"), "parentCandidateId", 128),
    })));
  }

  async tools(options: ListOptions = {}): Promise<Page<ToolInfo>> {
    const result=page<ToolInfo>(await this.#request("GET", "/tools", options, undefined, this.#listQuery(options)));
    result.data.forEach(toolMoney);return result;
  }

  async toolExecutions(taskId: string, options: ListOptions = {}): Promise<Page<ToolExecution>> {
    const result=page<ToolExecution>(await this.#request("GET", `/tasks/${identifier(taskId,"taskId")}/tool-executions`, options, undefined, this.#listQuery(options)));
    result.data.forEach(toolMoney);return result;
  }

  async getToolExecution(taskId: string, executionId: string, options: RequestOptions = {}): Promise<ToolExecutionView> {
    return toolView(await this.#request("GET", `/tasks/${identifier(taskId,"taskId")}/tool-executions/${identifier(executionId,"executionId")}`, options));
  }

  async prepareToolExecution(taskId: string, input: PrepareToolExecutionInput, options: MutationOptions): Promise<ToolExecutionView> {
    return toolView(await this.#request("POST", `/tasks/${identifier(taskId,"taskId")}/tool-executions`, options, {
      occurrence_id: occurrence(input.occurrenceId), expected_task_version: integer(input.expectedTaskVersion,"expectedTaskVersion",1),
      candidate_id: text(nonempty(input.candidateId,"candidateId"),"candidateId",128), artifact_id: text(nonempty(input.artifactId,"artifactId"),"artifactId",128),
      artifact_version: integer(input.artifactVersion,"artifactVersion",1), content_digest: digest(input.contentDigest),
      tool_id: text(nonempty(input.toolId,"toolId"),"toolId",128), tool_version: integer(input.toolVersion,"toolVersion",1),
      arguments_json: toolArguments(input.argumentsJson), expires_in_seconds: integer(input.expiresInSeconds,"expiresInSeconds",1,86400),
      ...(input.modelProposalId===undefined?{}:{model_proposal_id:text(nonempty(input.modelProposalId,"modelProposalId"),"modelProposalId",128)}),
    }));
  }

  async modelProposals(taskId: string, options: ListOptions = {}): Promise<Page<ModelToolProposal>> {
    return page<ModelToolProposal>(await this.#request("GET", `/tasks/${identifier(taskId,"taskId")}/model-tool-proposals`, options, undefined, this.#listQuery(options)));
  }

  async getModelProposal(taskId: string, proposalId: string, options: RequestOptions = {}): Promise<ModelToolProposal> {
    return object<ModelToolProposal>(await this.#request("GET", `/tasks/${identifier(taskId,"taskId")}/model-tool-proposals/${identifier(proposalId,"proposalId")}`, options));
  }

  async continueModel(taskId: string, input: ContinueModelInput, options: MutationOptions): Promise<Task> {
    if(input.acknowledgeCost!==true)throw new TypeError("acknowledgeCost must be explicitly true");
    return taskFacts(object<Record<string,unknown>>(await this.#request("POST", `/tasks/${identifier(taskId,"taskId")}/continue-model`, options, {
      expected_task_version:integer(input.expectedTaskVersion,"expectedTaskVersion",1),candidate_id:text(nonempty(input.candidateId,"candidateId"),"candidateId",128),
      artifact_id:text(nonempty(input.artifactId,"artifactId"),"artifactId",128),artifact_version:integer(input.artifactVersion,"artifactVersion",1),content_digest:digest(input.contentDigest),
      results:modelResults(input.results),acknowledge_cost:true,reason:text(nonempty(input.reason,"reason"),"reason",4096),
    })));
  }

  async automation(taskId: string, options: RequestOptions = {}): Promise<TaskAutomationState> {
    return automationState(await this.#request("GET", `/tasks/${identifier(taskId,"taskId")}/automation`, options));
  }

  async createAutomation(taskId: string, input: CreateTaskAutomationInput, options: MutationOptions): Promise<TaskAutomationState> {
    if (input.acknowledgeCost !== true) throw new TypeError("acknowledgeCost must be explicitly true");
    if (input.mode !== "read_only" && input.mode !== "prepare_and_continue") throw new TypeError("unsupported automation mode");
    return automationState(await this.#request("POST", `/tasks/${identifier(taskId,"taskId")}/automation`, options, {
      expected_task_version:integer(input.expectedTaskVersion,"expectedTaskVersion",1), mode:input.mode, tool_pins:automationPins(input.toolPins),
      max_model_turns:integer(input.maxModelTurns,"maxModelTurns",1,31), max_tool_executions:integer(input.maxToolExecutions,"maxToolExecutions",1,32),
      max_charge_microusd:automationCharge(input.maxChargeMicrousd), expires_in_seconds:integer(input.expiresInSeconds,"expiresInSeconds",1,86400),
      acknowledge_cost:true, reason:text(nonempty(input.reason,"reason"),"reason",4096),
    }));
  }

  async revokeAutomation(taskId: string, input: RevokeTaskAutomationInput, options: MutationOptions): Promise<TaskAutomationState> {
    return automationState(await this.#request("POST", `/tasks/${identifier(taskId,"taskId")}/automation/revoke`, options, {
      grant_id:text(nonempty(input.grantId,"grantId"),"grantId",128), expected_version:integer(input.expectedVersion,"expectedVersion",1),
      reason:text(nonempty(input.reason,"reason"),"reason",4096),
    }));
  }

  async progressSignals(taskId: string, options: ListOptions = {}): Promise<Page<ProgressSignal>> {
    return page<ProgressSignal>(await this.#request("GET", `/tasks/${identifier(taskId,"taskId")}/progress-signals`, options, undefined, this.#listQuery(options)));
  }

  async decideToolExecution(taskId: string, executionId: string, input: DecideToolExecutionInput, options: MutationOptions): Promise<ToolExecutionView> {
    if(!["approve","deny"].includes(input.decision))throw new TypeError("decision must be approve or deny");
    return toolView(await this.#request("POST", `/tasks/${identifier(taskId,"taskId")}/tool-executions/${identifier(executionId,"executionId")}/decide`, options, {
      expected_task_version: integer(input.expectedTaskVersion,"expectedTaskVersion",1), expected_execution_version: integer(input.expectedExecutionVersion,"expectedExecutionVersion",1),
      expected_approval_version: integer(input.expectedApprovalVersion,"expectedApprovalVersion",1), intent_hash: digest(input.intentHash),
      decision: input.decision, reason: text(nonempty(input.reason,"reason"),"reason",4096),
    }));
  }

  async dispatchToolExecution(taskId: string, executionId: string, input: DispatchToolExecutionInput, options: MutationOptions): Promise<ToolExecutionView> {
    if(input.acknowledgeCost!==true)throw new TypeError("acknowledgeCost must be explicitly true");
    return toolView(await this.#request("POST", `/tasks/${identifier(taskId,"taskId")}/tool-executions/${identifier(executionId,"executionId")}/dispatch`, options, {
      expected_task_version: integer(input.expectedTaskVersion,"expectedTaskVersion",1), expected_execution_version: integer(input.expectedExecutionVersion,"expectedExecutionVersion",1),
      expected_approval_version: integer(input.expectedApprovalVersion,"expectedApprovalVersion",1), intent_hash: digest(input.intentHash), acknowledge_cost: true,
    }));
  }

  async reconcileToolExecution(taskId: string, executionId: string, input: ReconcileToolExecutionInput, options: MutationOptions): Promise<ToolExecutionView> {
    if(!["succeeded","rejected"].includes(input.outcome))throw new TypeError("outcome must be succeeded or rejected");
    return toolView(await this.#request("POST", `/tasks/${identifier(taskId,"taskId")}/tool-executions/${identifier(executionId,"executionId")}/reconcile`, options, {
      expected_task_version: integer(input.expectedTaskVersion,"expectedTaskVersion",1), expected_execution_version: integer(input.expectedExecutionVersion,"expectedExecutionVersion",1),
      intent_hash: digest(input.intentHash), outcome: input.outcome, effect_reference: text(nonempty(input.effectReference,"effectReference"),"effectReference",256),
      result: text(input.result,"result",262144), evidence_digest: digest(input.evidenceDigest), reason: text(nonempty(input.reason,"reason"),"reason",4096),
    }));
  }

  async artifacts(taskId: string, options: ListOptions = {}): Promise<Page<Artifact>> {
    return page<Artifact>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/artifacts`, options, undefined, this.#listQuery(options)));
  }

  async runs(taskId: string, options: ListOptions = {}): Promise<Page<Run>> {
    return page<Run>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/runs`, options, undefined, this.#listQuery(options)));
  }

  async operations(taskId: string, options: ListOptions = {}): Promise<Page<Operation>> {
    return page<Operation>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/operations`, options, undefined, this.#listQuery(options)));
  }

  async actions(taskId: string, options: ListOptions = {}): Promise<Page<Action>> {
    return page<Action>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/actions`, options, undefined, this.#listQuery(options)));
  }

  async approvals(taskId: string, options: ListOptions = {}): Promise<Page<Approval>> {
    return page<Approval>(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/approvals`, options, undefined, this.#listQuery(options)));
  }

  /** Read server accounting for this task's subtree; no billing mutation is exposed. */
  async costs(taskId: string, options: CostOptions = {}): Promise<CostReport> {
    const query: Record<string, string | number> = {};
    if (options.limit !== undefined) query.limit = integer(options.limit, "limit", 1, 100);
    if (options.reservationCursor !== undefined) query.reservation_cursor = options.reservationCursor;
    if (options.entryCursor !== undefined) query.entry_cursor = options.entryCursor;
    if (options.incidentCursor !== undefined) query.incident_cursor = options.incidentCursor;
    if (options.quarantineCursor !== undefined) query.quarantine_cursor = options.quarantineCursor;
    return costReport(await this.#request("GET", `/tasks/${identifier(taskId, "taskId")}/costs`, options, undefined, query));
  }

  #listQuery(options: ListOptions): Record<string, string | number> {
    const query: Record<string, string | number> = {};
    if (options.cursor !== undefined) query.cursor = options.cursor;
    if (options.limit !== undefined) query.limit = integer(options.limit, "limit", 1, 100);
    return query;
  }

  async #request(
    method: "GET" | "POST",
    path: string,
    options: RequestOptions | MutationOptions,
    body?: Record<string, unknown>,
    query: Record<string, string | number> = {},
  ): Promise<Record<string, unknown>> {
    const headers: Record<string, string> = {
      Accept: "application/json",
      "X-Organization-ID": this.organizationId,
      [API_VERSION_HEADER]: API_VERSION,
    };
    if (method === "POST") {
      const key = header((options as MutationOptions).idempotencyKey, "idempotencyKey");
      if (key.length > 128) throw new RangeError("idempotencyKey cannot exceed 128 characters");
      if (key.includes(" ")) throw new TypeError("idempotencyKey cannot contain spaces");
      headers["Idempotency-Key"] = key;
    }
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const url = new URL(this.baseUrl + path);
    for (const [key, value] of Object.entries(query)) url.searchParams.set(key, String(value));

    const controller = new AbortController();
    let abortReason: "timeout" | "aborted" = "aborted";
    const onAbort = (): void => {
      if (!controller.signal.aborted) {
        abortReason = "aborted";
        controller.abort(options.signal?.reason);
      }
    };
    options.signal?.addEventListener("abort", onAbort, { once: true });
    if (options.signal?.aborted) onAbort();
    const timer = setTimeout(() => {
      if (!controller.signal.aborted) {
        abortReason = "timeout";
        controller.abort(new DOMException("Request timed out", "TimeoutError"));
      }
    }, this.timeoutMs);

    try {
      if (controller.signal.aborted) throw new TransportError(abortReason);
      const supplied = typeof this.#token === "function" ? this.#token() : this.#token;
      const token = await abortable(Promise.resolve(supplied), controller.signal);
      headers.Authorization = "Bearer " + header(token, "token");
      const response = await fetch(url, {
        method, headers,
        ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
        signal: controller.signal,
        redirect: "error",
        credentials: "omit",
        cache: "no-store",
      });
      checkAPIVersion(response.headers);
      if (!response.ok) {
        let document: Record<string, unknown> = {};
        try {
          document = await readJson(response);
        } catch (error) {
          if (!(error instanceof ProtocolError)) throw error;
        }
        const details = record(document.error) ? document.error : {};
        const requestId = typeof details.request_id === "string"
          ? details.request_id : response.headers.get("X-Request-ID") ?? undefined;
        throw new APIError({
          statusCode: response.status,
          code: typeof details.code === "string" ? details.code : "http_error",
          message: typeof details.message === "string" ? details.message
            : "Server returned an HTTP error without a valid error envelope",
          requestId,
          retryable: details.retryable === true,
        });
      }
      const document = await readJson(response);
      if (!("data" in document)) throw new ProtocolError("Response is missing the data envelope");
      return document;
    } catch (error) {
      if (error instanceof AgentPlatformError) throw error;
      throw new TransportError(controller.signal.aborted ? abortReason : "network", { cause: error });
    } finally {
      clearTimeout(timer);
      options.signal?.removeEventListener("abort", onAbort);
    }
  }
}

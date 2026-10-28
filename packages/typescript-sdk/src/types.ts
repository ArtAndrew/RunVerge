// SPDX-License-Identifier: Apache-2.0
export type * from "./capabilityTypes.js";
/** Public contract 0.10.0. Responses retain wire names and additional fields. */
/** Exact canonical decimal int64 micro-USD string; parse with parseMicroUSD. */
export type Money = string;

export type TaskStatus =
  | "queued" | "running" | "succeeded" | "failed" | "waiting_input"
  | "waiting_approval" | "blocked" | "cancelled";

export interface Task {
  id: string;
  organization_id: string;
  owner_id: string;
  agent_id: string;
  agent_version: number;
  goal: string;
  status: TaskStatus;
  budget_microusd: number;
  reserved_microusd: Money;
  spent_microusd: Money;
  version: number;
  created_at: string;
  updated_at: string;
  waiting_reason?: string;
  result?: string;
  root_id: string;
  parent_id?: string;
  depth: number;
  financial_hold: boolean;
  model_connection_id?: string;
  model_connection_version?: number;
  current_run_id?: string;
  attempt_count?: number;
  max_runs?: number;
  execution_deadline?: string;
  candidate_id?: string;
  accepted_decision_id?: string;
  [field: string]: unknown;
}

/** Available organization model profile; no endpoint or credential material. */
export interface AvailableModel {
	accounting_mode: "metered" | "operator_unmetered_local";
	max_input_tokens: number;
	max_output_bytes: number;
	timeout_milliseconds: number;
  id: string;
  model: string;
  version: number;
  price_version: string;
  max_charge_microusd: number;
  max_input_bytes: number;
  max_output_tokens: number;
  bound_assumptions: string;
  created_at: string;
  [field: string]: unknown;
}

export interface Run {
  attempt?: number;
  input_digest?: string;
  feedback?: string;
  source_candidate_id?: string;
  id: string;
  organization_id: string;
  task_id: string;
  principal_id: string;
  agent_id: string;
  agent_version: number;
  status: string;
  created_at: string;
  [field: string]: unknown;
}

export interface Operation {
  id: string;
  organization_id: string;
  task_id: string;
  run_id: string;
  status: string;
  lease_owner?: string;
  lease_until?: string;
  fencing_token: number;
  attempts: number;
  max_attempts: number;
  created_at: string;
  [field: string]: unknown;
}

export interface Action {
  id: string;
  organization_id: string;
  task_id: string;
  operation_id: string;
  intent_hash: string;
  idempotency_key: string;
  status: string;
  created_at: string;
  [field: string]: unknown;
}

export interface Approval {
  id: string;
  organization_id: string;
  task_id: string;
  action_id: string;
  intent_hash: string;
  status: string;
  version: number;
  decided_by?: string;
  execution_id?: string;
  decided_at?: string;
  decision_method?: string;
  reason?: string;
  created_at: string;
  expires_at: string;
  [field: string]: unknown;
}

export interface Reservation {
  id: string;
  organization_id: string;
  task_id: string;
  operation_id: string;
  action_id?: string;
  root_id: string;
  scope_task_ids: string[];
  charge_key: string;
  category: string;
  provider: string;
  price_version: string;
  amount_microusd: Money;
  actual_microusd?: Money;
  pending_quarantine_id?: string;
  status: string;
  revision: number;
  created_at: string;
  [field: string]: unknown;
}

export interface LedgerEntry {
  id: string;
  organization_id: string;
  reservation_id: string;
  task_id: string;
  root_id: string;
  kind: string;
  /** Signed integer micro-USD; release and correction entries can be negative. */
  delta_reserved_microusd: Money;
  delta_spent_microusd: Money;
  synthetic: boolean;
  created_at: string;
  [field: string]: unknown;
}

export interface FinancialIncident {
  id: string;
  organization_id: string;
  reservation_id: string;
  task_id: string;
  kind: string;
  amount_microusd: Money;
  created_at: string;
  [field: string]: unknown;
}

export interface CostQuarantine {
  id: string;
  organization_id: string;
  reservation_id: string;
  evidence_id: string;
  task_id: string;
  actual_microusd: Money;
  status: "pending" | "resolved";
  reason: "projection_overflow";
  requested_kind: "settle" | "adjustment";
  base_revision: number;
  revision: number;
  resolution_id?: string;
  created_at: string;
  [field: string]: unknown;
}

/** Trusted ingestion result type; the public SDK exposes no billing mutation. */
export interface AccountingOutcome {
  status: "settled" | "quarantined";
  evidence_id: string;
  quarantine_id?: string;
  reservation: Reservation;
  [field: string]: unknown;
}

/** Trusted ingestion wire type; the public SDK exposes no billing mutation. */
export interface CostEvidence {
  source: string;
  reference: string;
  payload_digest: string;
  actual_microusd: Money;
  final: true;
  currency: "USD";
  reason?: string;
}

export interface CostAdjustment {
  expected_revision: number;
  evidence: CostEvidence;
}

/** One consistent snapshot of the task subtree; each collection has its own cursor. */
export interface CostReport {
  reservations: Reservation[];
  entries: LedgerEntry[];
  incidents: FinancialIncident[];
  quarantines: CostQuarantine[];
  pending_quarantine_count: number;
  incomplete: boolean;
  budget_microusd: number;
  reserved_microusd: Money;
  spent_microusd: Money;
  unknown_microusd: Money;
  unknown_reservation_count: string;
  financial_hold: boolean;
  next_reservation_cursor?: string;
  next_entry_cursor?: string;
  next_incident_cursor?: string;
  next_quarantine_cursor?: string;
  [field: string]: unknown;
}

export interface Agent {
  team?: TeamDefinition;
  /** Server-pinned public definitions; createAgent accepts only tools IDs. */
  readonly model_tools?: readonly ModelToolDefinition[];
  id: string;
  organization_id: string;
  name: string;
  instructions: string;
  model: string;
  tools: string[];
  budget_microusd: number;
  version: number;
  published: boolean;
  created_at: string;
  [field: string]: unknown;
}

export interface EventBase {
  id: string;
  organization_id: string;
  task_id: string;
  sequence: string;
  type: string;
  created_at: string;
  [field: string]: unknown;
}

/** Version zero is deliberately opaque: never parse/rewrite its exact stored JSON. */
export interface LegacyEvent extends EventBase { payload_version: 0; legacy_data_json: string; data?: never }
export type TypedEvent = { [K in keyof import('./eventPayloads.js').EventPayloadMap]: EventBase & {type:K;payload_version:1;data:import('./eventPayloads.js').EventPayloadMap[K];legacy_data_json?:never} }[keyof import('./eventPayloads.js').EventPayloadMap];
export type Event = LegacyEvent | TypedEvent;
export interface EventReplayWindow { after_sequence: string; through_sequence: string; version: number }
export interface TaskEventSnapshot { organization_id: string; task_id: string; task: Task; events: Event[]; cursor: string; replay_cursor: string; window: EventReplayWindow; has_earlier_events: boolean; observed_at: string }
export interface TaskEventBatch { organization_id: string; task_id: string; events: Event[]; cursor: string; has_more: boolean; reset_required: boolean; reset_reason?: "history_expired"; window: EventReplayWindow }
export interface EventStreamError { code: string; message: string; retryable: boolean }
export type TaskEventStreamFrame = { frame_version: 1; kind: "batch" | "reset_required"; batch: TaskEventBatch; error?: never } | { frame_version: 1; kind: "error"; error: EventStreamError; batch?: never };
export interface APICompatibility { api_version: string; supported_versions: string[]; version_header: string; header_required: boolean; event_payload_versions: number[]; legacy_event_policy: string; command_retry_policy: string }
export interface EventFeedOptions extends RequestOptions { cursor?: string; limit?: number }
export interface EventStreamOptions extends RequestOptions { cursor?: string; /** Network inactivity timeout, not the total stream lifetime. */ idleTimeoutMs?: number }

export interface Artifact {
  id: string;
  organization_id: string;
  task_id: string;
  name: string;
  media_type: string;
  content: string;
  version: number;
  created_at: string;
  provenance: Record<string, unknown>;
  [field: string]: unknown;
}

export interface Page<T> {
  data: T[];
  next_cursor?: string;
}

export interface Envelope<T> {
  data: T;
}

export interface RequestOptions {
  /** Cancels waiting locally. Cancel a server task explicitly with cancelTask. */
  signal?: AbortSignal;
}

export interface MutationOptions extends RequestOptions {
  /** Stable key for one operation, reused after a lost response. */
  idempotencyKey: string;
}

export interface ListOptions extends RequestOptions {
  cursor?: string;
  limit?: number;
}

export interface EventOptions extends RequestOptions {
  /** Last processed organization event sequence; gaps are allowed. */
  after?: string;
  limit?: number;
}

export interface CostOptions extends RequestOptions {
  /** Per-collection page size; server default 50, maximum 100. */
  limit?: number;
  reservationCursor?: string;
  entryCursor?: string;
  incidentCursor?: string;
  quarantineCursor?: string;
}

export interface CreateTaskInput {
  maxRuns?: number;
  executionTimeoutSeconds?: number;
  goal: string;
  agentId: string;
  /** Integer millionths of USD, bounded by the admission policy (1e12). */
  budgetMicrousd: number;
}

export interface CreateAgentInput {
  team?: TeamDefinition;
  name: string;
  instructions: string;
  model?: string;
  tools?: string[];
  budgetMicrousd?: number;
}

export interface PublishOptions extends MutationOptions {
  expectedVersion: number;
}

export interface ClientOptions {
  /** Server origin or API root ending in /api/v1. */
  baseUrl: string;
  /** The callback runs for every request and can return a refreshed token. */
  token: string | (() => string | Promise<string>);
  organizationId: string;
  /** Total request timeout, including token acquisition and body reads. */
  timeoutMs?: number;
}


export type ReviewDecision = "accept" | "reject" | "request_changes";
export interface TaskCandidate {
  parent_candidate_id?: string; review_basis_candidate_id?: string;
  material_id?: string; material_version?: number; published_by?: string;
  id: string; organization_id: string; task_id: string; run_id: string;
  operation_id: string; artifact_id: string; artifact_version: number;
  content_digest: string; source: string; created_at: string;
  [field: string]: unknown;
}
export interface TaskDecision {
  id: string; organization_id: string; task_id: string; run_id: string;
  candidate_id: string; artifact_id: string; artifact_version: number;
  content_digest: string; decision: ReviewDecision; reason?: string;
  principal_id: string; method: string;
  task_version_before: number; task_version_after: number;
  parent_decision_id?: string; created_at: string;
  [field: string]: unknown;
}
/** A result decision is neither a billing instruction nor proof of factual truth. */
export interface ReviewTaskInput {
  expectedVersion: number; candidateId: string; artifactId: string;
  artifactVersion: number; contentDigest: string; decision: ReviewDecision;
  reason?: string;
}
/** Starts a new bounded root execution attempt; this is not a transport retry. */
export interface RetryTaskInput {
  expectedVersion: number; reason: string; feedback?: string;
  acknowledgeNewCost: true;
}
export interface TeamMember {
  id: string; agent_id: string; agent_version: number; task?: string;
}
export interface TeamDefinition {
  pattern: "sequence" | "parallel" | "delegate_verify";
  members: TeamMember[];
  join_policy: "all" | "allow_partial";
  max_depth: number; max_children: number; max_parallel?: number;
}

/** Logical draft head; saving does not replace a task candidate or launch work. */
export interface Material {
  id: string; organization_id: string; task_id: string; name: string;
  media_type: "text/plain" | "text/markdown" | "application/json";
  head_version: number; head_content_digest: string; base_candidate_id?: string;
  created_by: string; created_at: string; updated_at: string;
  [field: string]: unknown;
}
export interface MaterialRevision {
  id: string; organization_id: string; task_id: string; material_id: string;
  version: number; base_version: number; merge_base_version?: number;
  content_digest: string; principal_id: string; reason?: string; created_at: string;
  [field: string]: unknown;
}
export interface MaterialContent { revision: MaterialRevision; content: string }
export interface CreateMaterialInput {
  name: string; mediaType: Material["media_type"]; content: string;
  baseCandidateId?: string; reason?: string;
}
export interface ReviseMaterialInput {
  expectedVersion: number; content: string; reason: string; mergeBaseVersion?: number;
}
/** Publish an exact saved revision as a candidate, with no new execution attempt. */
export interface PublishMaterialInput {
  expectedTaskVersion: number; expectedMaterialVersion: number;
  contentDigest: string; parentCandidateId: string;
}
export interface ArtifactOptions extends RequestOptions { version?: number }

/** Active membership for this authenticated request; no credential or session handle. */
export interface CurrentIdentity { principal_id: string; organization_id: string; role: string; principal_kind?: PrincipalKind; permissions: string[] }

/** Pinned public tool metadata. Contains no endpoint or credential reference. */
export interface ToolInfo {
  automation_allowed?: boolean;
  id: string; organization_id: string; version: number; name: string; description: string;
  effect: string; destination: string; provider: string; price_version: string;
  max_charge_microusd: Money; input_schema_json: string; input_schema_digest: string;
  require_distinct_approver: boolean; timeout_seconds: number; max_input_bytes: number;
  max_output_bytes: number; active: boolean; created_at: string;
}
export interface ToolExecution {
  automation_grant_id?: string;
  model_proposal_id?: string;
  occurrence_id: string; id: string; organization_id: string; task_id: string; run_id: string;
  operation_id: string; action_id: string; reservation_id: string; approval_id: string;
  tool_id: string; tool_version: number; tool_profile_digest: string;
  candidate_id: string; artifact_id: string; artifact_version: number; content_digest: string;
  source_task_version: number; dispatched_by?: string; arguments_json?: string; arguments_digest: string; intent_hash: string;
  max_charge_microusd: Money; provider: string; price_version: string; created_by: string;
  status: string; version: number; reason?: string; result_receipt_id?: string; created_at: string; expires_at: string;
}
export interface ToolOutcomeReceipt {
  id: string; organization_id: string; execution_id: string; intent_hash: string; outcome: string;
  method: string; effect_reference: string; artifact_id: string; content_digest: string; evidence_digest: string;
  principal_id?: string; reason?: string; created_at: string;
}
export interface ToolExecutionView {
  execution: ToolExecution; tool: ToolInfo; approval: Approval; task: Task;
  available_commands: string[]; receipt?: ToolOutcomeReceipt;
}
export interface PrepareToolExecutionInput {
  modelProposalId?: string;
  occurrenceId: string; expectedTaskVersion: number; candidateId: string; artifactId: string;
  artifactVersion: number; contentDigest: string; toolId: string; toolVersion: number;
  argumentsJson: string; expiresInSeconds: number;
}

/** Immutable server pin. No endpoint, credential reference or certificate. */
export interface ModelToolDefinition {
  readonly tool_id: string; readonly tool_version: number; readonly tool_profile_digest: string;
  readonly name: string; readonly description: string; readonly input_schema_json: string;
}
export interface ModelToolCall { id: string; name: string; arguments_json: string }
export interface ModelMessage { role: string; content: string; tool_call_id?: string; tool_calls?: ModelToolCall[] }
/** List entries omit arguments_json; getModelProposal returns exact bytes. */
export interface ModelToolProposal {
  id: string; organization_id: string; task_id: string; run_id: string; operation_id: string;
  candidate_id: string; artifact_id: string; artifact_version: number; content_digest: string;
  call_id: string; tool_id: string; tool_version: number; tool_profile_digest: string; tool_name: string;
  arguments_json?: string; arguments_digest: string; execution_id?: string; execution_status?: string; created_at: string;
}
/** Exact terminal observation. Wire names match the durable result binding. */
export interface ModelToolResultBinding {
  proposal_id: string; execution_id: string; execution_version: number;
  status: "succeeded" | "rejected" | "denied" | "expired" | "cancelled";
  receipt_id?: string; content_digest?: string;
}
/** One explicit newly charged model turn within the same Run. */
export interface ContinueModelInput {
  expectedTaskVersion: number; candidateId: string; artifactId: string; artifactVersion: number;
  contentDigest: string; results: ModelToolResultBinding[]; acknowledgeCost: true; reason: string;
}

export interface AutomationToolPin { tool_id: string; tool_version: number; tool_profile_digest: string }
/** Task-scoped delegation; it survives logout until revoked, expired or stopped. */
export interface TaskAutomationGrant {
  id: string; organization_id: string; task_id: string; principal_id: string; owner_id: string;
  agent_id: string; agent_version: number; model_connection_id: string; model_connection_version: number;
  version: number; status: string; mode: "read_only" | "prepare_and_continue"; tool_pins: AutomationToolPin[];
  max_model_turns: number; max_tool_executions: number; max_charge_microusd: Money;
  reason: string; stop_reason?: string; created_at: string; expires_at: string;
}
/** Cumulative task usage; retries and replacement grants do not reset it. */
export interface AutomationUsage { model_turns: number; tool_executions: number; allocated_microusd: Money }
/** Conservative stop evidence, never a proof that a model claim is false. */
export interface ProgressSignal {
  id: string; organization_id: string; task_id: string; grant_id: string; code: string;
  detector_version: string; fingerprint?: string; evidence_ids: string[]; snapshot_digest: string; created_at: string;
}
export interface TaskAutomationState {
  grant?: TaskAutomationGrant; usage: AutomationUsage; signals: ProgressSignal[]; available_commands: string[];
}
/** Finite task-lifetime ceilings. This does not authorize final acceptance. */
export interface CreateTaskAutomationInput {
  expectedTaskVersion: number; mode: "read_only" | "prepare_and_continue"; toolPins: AutomationToolPin[];
  maxModelTurns: number; maxToolExecutions: number; maxChargeMicrousd: Money; expiresInSeconds: number;
  acknowledgeCost: true; reason: string;
}
export interface RevokeTaskAutomationInput { grantId: string; expectedVersion: number; reason: string }
export interface DecideToolExecutionInput {
  expectedTaskVersion: number; expectedExecutionVersion: number; expectedApprovalVersion: number;
  intentHash: string; decision: "approve" | "deny"; reason: string;
}
export interface DispatchToolExecutionInput {
  expectedTaskVersion: number; expectedExecutionVersion: number; expectedApprovalVersion: number;
  intentHash: string; acknowledgeCost: true;
}
export interface ReconcileToolExecutionInput {
  expectedTaskVersion: number; expectedExecutionVersion: number; intentHash: string;
  outcome: "succeeded" | "rejected"; effectReference: string; result: string; evidenceDigest: string; reason: string;
}


export type MembershipRole = "owner" | "admin" | "member" | "viewer";
export interface OrganizationMember {
  organization_id: string; principal_id: string; principal_kind?: PrincipalKind; display_name: string;
  role: MembershipRole; active: boolean; principal_active: boolean; version: number;
  created_at: string; updated_at: string; credentials_revoked_at?: string;
  available_commands: string[]; assignable_roles: MembershipRole[];
  [field: string]: unknown;
}
export interface MembershipCapabilities {
  organization_id: string; can_create_members: boolean;
  assignable_roles: MembershipRole[]; self_management_allowed: false;
  [field: string]: unknown;
}
export interface MembershipAuditState {
  display_name: string; role: MembershipRole; active: boolean; version: number;
}
export interface MembershipAuditEntry {
  id: string; organization_id: string; actor_id: string; target_principal_id: string;
  action: string; reason: string; before?: MembershipAuditState;
  after: MembershipAuditState; created_at: string; [field: string]: unknown;
}
export interface CreateMemberInput { displayName: string; role: MembershipRole; reason: string }
export interface UpdateMemberInput extends CreateMemberInput { expectedVersion: number; active: boolean }
export interface RevokeMemberCredentialsInput { expectedVersion: number; reason: string }
export interface MembershipAuditOptions extends ListOptions { principalId?: string }

export type PrincipalKind = "human" | "service";
export type ServiceRole = "member" | "viewer";
export type LoginPolicyMode = "mixed" | "oidc_required";
export interface OrganizationLoginPolicy {
  organization_id: string; mode: LoginPolicyMode; oidc_issuer: string;
  service_access_enabled: boolean; max_service_key_ttl_seconds: number; version: number;
  created_at: string; updated_at: string; available_commands: string[];
  configured_oidc_issuer?: string; can_require_oidc: boolean; readiness_reasons: string[];
  [field: string]: unknown;
}
export interface UpdateLoginPolicyInput {
  expectedVersion: number; mode: LoginPolicyMode; oidcIssuer: string;
  serviceAccessEnabled: boolean; maxServiceKeyTTLSeconds: number; reason: string;
}
export interface ServiceAccount extends Omit<OrganizationMember,"role"|"principal_kind"|"assignable_roles"> {
  organization_id: string; principal_id: string; principal_kind: "service"; display_name: string;
  role: ServiceRole; active: boolean; principal_active: boolean; version: number;
  created_at: string; updated_at: string; credentials_revoked_at?: string;
  available_commands: string[]; assignable_roles: ServiceRole[];
}
export interface CreateServiceAccountInput { displayName: string; role: ServiceRole; reason: string }
export interface UpdateServiceAccountInput extends CreateServiceAccountInput { expectedVersion: number; active: boolean }
export interface ServiceKey {
  id: string; organization_id: string; principal_id: string; role_ceiling: ServiceRole;
  version: number; revoked: boolean; created_at: string; expires_at: string;
  status: string; available_commands: string[]; [field: string]: unknown;
}
export interface RevokeServiceKeyInput { expectedVersion: number; reason: string }
export interface OrganizationSecurityAuditState {
  version: number; mode?: LoginPolicyMode; oidc_issuer?: string; service_access_enabled?: boolean;
  max_service_key_ttl_seconds?: number; display_name?: string; role?: ServiceRole;
  active?: boolean; role_ceiling?: ServiceRole; expires_at?: string; revoked?: boolean;
}
export interface OrganizationSecurityAuditEntry {
  id: string; organization_id: string; actor_type: string; actor_id?: string;
  object_type: string; object_id: string; target_principal_id?: string; action: string;
  reason: string; before?: OrganizationSecurityAuditState; after: OrganizationSecurityAuditState;
  created_at: string; [field: string]: unknown;
}
export interface SecurityAuditOptions extends ListOptions { principalId?: string }

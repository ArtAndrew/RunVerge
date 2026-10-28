# SPDX-License-Identifier: Apache-2.0
"""JSON-compatible API types; response dictionaries preserve extra fields."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Generic, Literal, TypeVar, TypedDict

from .money import Money
from .event_payloads import TypedEvent

T = TypeVar("T")
TaskStatus = Literal[
    "queued", "running", "succeeded", "failed", "waiting_input",
    "waiting_approval", "blocked", "cancelled",
]


@dataclass(frozen=True)
class Page(Generic[T]):
    """One response page. Pass ``next_cursor`` to the next list request."""

    data: list[T]
    next_cursor: str | None = None


class _RequiredTask(TypedDict):
    id: str
    organization_id: str
    owner_id: str
    goal: str
    agent_id: str
    agent_version: int
    status: TaskStatus
    budget_microusd: int
    reserved_microusd: Money
    spent_microusd: Money
    version: int
    created_at: str
    updated_at: str
    root_id: str
    depth: int
    financial_hold: bool


class Task(_RequiredTask, total=False):
    waiting_reason: str
    result: str
    parent_id: str
    model_connection_id: str
    model_connection_version: int
    current_run_id: str
    attempt_count: int
    max_runs: int
    execution_deadline: str
    candidate_id: str
    accepted_decision_id: str


class AvailableModel(TypedDict):
    accounting_mode: Literal["metered", "operator_unmetered_local"]
    max_input_tokens: int
    max_output_bytes: int
    timeout_milliseconds: int
    id: str
    model: str
    version: int
    price_version: str
    max_charge_microusd: int
    max_input_bytes: int
    max_output_tokens: int
    bound_assumptions: str
    created_at: str


class _RequiredRun(TypedDict):
    id: str
    organization_id: str
    task_id: str
    principal_id: str
    agent_id: str
    agent_version: int
    status: str
    created_at: str


class Run(_RequiredRun, total=False):
    attempt: int
    input_digest: str
    feedback: str
    source_candidate_id: str


class _RequiredOperation(TypedDict):
    id: str
    organization_id: str
    task_id: str
    run_id: str
    status: str
    fencing_token: int
    attempts: int
    max_attempts: int
    created_at: str


class Operation(_RequiredOperation, total=False):
    lease_owner: str
    lease_until: str


class Action(TypedDict):
    id: str
    organization_id: str
    task_id: str
    operation_id: str
    intent_hash: str
    idempotency_key: str
    status: str
    created_at: str


class _RequiredApproval(TypedDict):
    id: str
    organization_id: str
    task_id: str
    action_id: str
    intent_hash: str
    status: str
    version: int
    created_at: str
    expires_at: str


class Approval(_RequiredApproval, total=False):
    decided_by: str
    execution_id: str
    decided_at: str
    decision_method: str
    reason: str


class _RequiredReservation(TypedDict):
    id: str
    organization_id: str
    task_id: str
    operation_id: str
    root_id: str
    scope_task_ids: list[str]
    charge_key: str
    category: str
    provider: str
    price_version: str
    amount_microusd: Money
    status: str
    revision: int
    created_at: str


class Reservation(_RequiredReservation, total=False):
    action_id: str
    actual_microusd: Money
    pending_quarantine_id: str


class LedgerEntry(TypedDict):
    id: str
    organization_id: str
    reservation_id: str
    task_id: str
    root_id: str
    kind: str
    # Signed micro-USD: releases and corrections can have negative deltas.
    delta_reserved_microusd: Money
    delta_spent_microusd: Money
    synthetic: bool
    created_at: str


class FinancialIncident(TypedDict):
    id: str
    organization_id: str
    reservation_id: str
    task_id: str
    kind: str
    amount_microusd: Money
    created_at: str


class _RequiredCostQuarantine(TypedDict):
    id: str
    organization_id: str
    reservation_id: str
    evidence_id: str
    task_id: str
    actual_microusd: Money
    status: Literal["pending", "resolved"]
    reason: Literal["projection_overflow"]
    requested_kind: Literal["settle", "adjustment"]
    base_revision: int
    revision: int
    created_at: str


class CostQuarantine(_RequiredCostQuarantine, total=False):
    resolution_id: str


class _RequiredAccountingOutcome(TypedDict):
    status: Literal["settled", "quarantined"]
    evidence_id: str
    reservation: Reservation


class AccountingOutcome(_RequiredAccountingOutcome, total=False):
    quarantine_id: str


class _RequiredCostEvidence(TypedDict):
    source: str
    reference: str
    payload_digest: str
    actual_microusd: Money
    final: Literal[True]
    currency: Literal["USD"]


class CostEvidence(_RequiredCostEvidence, total=False):
    """Trusted ingestion wire type; this SDK exposes no billing mutations."""

    reason: str


class CostAdjustment(TypedDict):
    expected_revision: int
    evidence: CostEvidence


class _RequiredCostReport(TypedDict):
    reservations: list[Reservation]
    entries: list[LedgerEntry]
    incidents: list[FinancialIncident]
    quarantines: list[CostQuarantine]
    pending_quarantine_count: int
    incomplete: bool
    budget_microusd: int
    reserved_microusd: Money
    spent_microusd: Money
    unknown_microusd: Money
    unknown_reservation_count: str
    financial_hold: bool


class CostReport(_RequiredCostReport, total=False):
    """One consistent subtree snapshot; each collection has its own cursor."""

    next_reservation_cursor: str
    next_entry_cursor: str
    next_incident_cursor: str
    next_quarantine_cursor: str


class _RequiredAgent(TypedDict):
    id: str
    organization_id: str
    name: str
    instructions: str
    model: str
    tools: list[str]
    budget_microusd: int
    version: int
    published: bool
    created_at: str


class Agent(_RequiredAgent, total=False):
    team: TeamDefinition
    model_tools: list[ModelToolDefinition]


class EventBase(TypedDict):
    id: str
    organization_id: str
    type: str
    task_id: str
    sequence: str
    created_at: str


class LegacyEvent(EventBase):
    payload_version: Literal[0]
    legacy_data_json: str


Event = LegacyEvent | TypedEvent


class EventReplayWindow(TypedDict):
    after_sequence: str
    through_sequence: str
    version: int


class TaskEventSnapshot(TypedDict):
    organization_id: str
    task_id: str
    task: Task
    events: list[Event]
    cursor: str
    replay_cursor: str
    window: EventReplayWindow
    has_earlier_events: bool
    observed_at: str


class _RequiredEventBatch(TypedDict):
    organization_id: str
    task_id: str
    events: list[Event]
    cursor: str
    has_more: bool
    reset_required: bool
    window: EventReplayWindow


class TaskEventBatch(_RequiredEventBatch, total=False):
    reset_reason: Literal["history_expired"]


class EventStreamError(TypedDict):
    code: str
    message: str
    retryable: bool


class _RequiredStreamFrame(TypedDict):
    frame_version: Literal[1]
    kind: Literal["batch", "reset_required", "error"]


class TaskEventStreamFrame(_RequiredStreamFrame, total=False):
    batch: TaskEventBatch
    error: EventStreamError


class APICompatibility(TypedDict):
    api_version: str
    supported_versions: list[str]
    version_header: str
    header_required: bool
    event_payload_versions: list[int]
    legacy_event_policy: str
    command_retry_policy: str


class Artifact(TypedDict):
    id: str
    organization_id: str
    task_id: str
    name: str
    media_type: str
    content: str
    version: int
    created_at: str
    provenance: dict[str, Any]


ReviewDecision = Literal["accept", "reject", "request_changes"]


class _RequiredTaskCandidate(TypedDict):
    id: str
    organization_id: str
    task_id: str
    run_id: str
    operation_id: str
    artifact_id: str
    artifact_version: int
    content_digest: str
    source: str
    created_at: str


class TaskCandidate(_RequiredTaskCandidate, total=False):
    parent_candidate_id: str
    review_basis_candidate_id: str
    material_id: str
    material_version: int
    published_by: str


class _RequiredTaskDecision(TypedDict):
    id: str
    organization_id: str
    task_id: str
    run_id: str
    candidate_id: str
    artifact_id: str
    artifact_version: int
    content_digest: str
    decision: ReviewDecision
    principal_id: str
    method: str
    task_version_before: int
    task_version_after: int
    created_at: str


class TaskDecision(_RequiredTaskDecision, total=False):
    reason: str
    parent_decision_id: str


class _RequiredTeamMember(TypedDict):
    id: str
    agent_id: str
    agent_version: int


class TeamMember(_RequiredTeamMember, total=False):
    task: str


class _RequiredTeamDefinition(TypedDict):
    pattern: Literal["sequence", "parallel", "delegate_verify"]
    members: list[TeamMember]
    join_policy: Literal["all", "allow_partial"]
    max_depth: int
    max_children: int


class TeamDefinition(_RequiredTeamDefinition, total=False):
    max_parallel: int


class _RequiredMaterial(TypedDict):
    id: str
    organization_id: str
    task_id: str
    name: str
    media_type: Literal["text/plain", "text/markdown", "application/json"]
    head_version: int
    head_content_digest: str
    created_by: str
    created_at: str
    updated_at: str


class Material(_RequiredMaterial, total=False):
    base_candidate_id: str


class _RequiredMaterialRevision(TypedDict):
    id: str
    organization_id: str
    task_id: str
    material_id: str
    version: int
    base_version: int
    content_digest: str
    principal_id: str
    created_at: str


class MaterialRevision(_RequiredMaterialRevision, total=False):
    merge_base_version: int
    reason: str


class MaterialContent(TypedDict):
    revision: MaterialRevision
    content: str


class _RequiredCurrentIdentity(TypedDict):
    """Active membership of the authenticated request; no credentials."""
    principal_id: str
    organization_id: str
    role: str
    permissions: list[str]

class CurrentIdentity(_RequiredCurrentIdentity, total=False):
    principal_kind: Literal["human", "service"]



class _RequiredToolInfo(TypedDict):
    id: str
    organization_id: str
    version: int
    name: str
    description: str
    effect: str
    destination: str
    provider: str
    price_version: str
    max_charge_microusd: Money
    input_schema_json: str
    input_schema_digest: str
    require_distinct_approver: bool
    timeout_seconds: int
    max_input_bytes: int
    max_output_bytes: int
    active: bool
    created_at: str


class ToolInfo(_RequiredToolInfo, total=False):
    automation_allowed: bool


class _RequiredToolExecution(TypedDict):
    occurrence_id: str
    id: str
    organization_id: str
    task_id: str
    run_id: str
    operation_id: str
    action_id: str
    reservation_id: str
    approval_id: str
    tool_id: str
    tool_version: int
    tool_profile_digest: str
    candidate_id: str
    artifact_id: str
    artifact_version: int
    content_digest: str
    source_task_version: int
    arguments_digest: str
    intent_hash: str
    max_charge_microusd: Money
    provider: str
    price_version: str
    created_by: str
    status: str
    version: int
    created_at: str
    expires_at: str


class ToolExecution(_RequiredToolExecution, total=False):
    automation_grant_id: str
    model_proposal_id: str
    dispatched_by: str
    arguments_json: str
    reason: str
    result_receipt_id: str


class _RequiredToolOutcomeReceipt(TypedDict):
    id: str
    organization_id: str
    execution_id: str
    intent_hash: str
    outcome: str
    method: str
    effect_reference: str
    artifact_id: str
    content_digest: str
    evidence_digest: str
    created_at: str


class ToolOutcomeReceipt(_RequiredToolOutcomeReceipt, total=False):
    principal_id: str
    reason: str


class _RequiredToolExecutionView(TypedDict):
    execution: ToolExecution
    tool: ToolInfo
    approval: Approval
    task: Task
    available_commands: list[str]


class ToolExecutionView(_RequiredToolExecutionView, total=False):
    receipt: ToolOutcomeReceipt


class ModelToolDefinition(TypedDict):
    """Read-only server pin; create_agent accepts only registered tools IDs."""
    tool_id: str
    tool_version: int
    tool_profile_digest: str
    name: str
    description: str
    input_schema_json: str


class ModelToolCall(TypedDict):
    id: str
    name: str
    arguments_json: str


class _RequiredModelMessage(TypedDict):
    role: str
    content: str


class ModelMessage(_RequiredModelMessage, total=False):
    tool_call_id: str
    tool_calls: list[ModelToolCall]


class _RequiredModelToolProposal(TypedDict):
    id: str
    organization_id: str
    task_id: str
    run_id: str
    operation_id: str
    candidate_id: str
    artifact_id: str
    artifact_version: int
    content_digest: str
    call_id: str
    tool_id: str
    tool_version: int
    tool_profile_digest: str
    tool_name: str
    arguments_digest: str
    created_at: str


class ModelToolProposal(_RequiredModelToolProposal, total=False):
    """Metadata lists omit arguments_json; exact reads return original bytes."""
    arguments_json: str
    execution_id: str
    execution_status: str


class _RequiredModelToolResultBinding(TypedDict):
    proposal_id: str
    execution_id: str
    execution_version: int
    status: Literal["succeeded", "rejected", "denied", "expired", "cancelled"]


class ModelToolResultBinding(_RequiredModelToolResultBinding, total=False):
    receipt_id: str
    content_digest: str


class AutomationToolPin(TypedDict):
    tool_id: str
    tool_version: int
    tool_profile_digest: str


class _RequiredTaskAutomationGrant(TypedDict):
    id: str
    organization_id: str
    task_id: str
    principal_id: str
    owner_id: str
    agent_id: str
    agent_version: int
    model_connection_id: str
    model_connection_version: int
    version: int
    status: str
    mode: Literal["read_only", "prepare_and_continue"]
    tool_pins: list[AutomationToolPin]
    max_model_turns: int
    max_tool_executions: int
    max_charge_microusd: Money
    reason: str
    created_at: str
    expires_at: str


class TaskAutomationGrant(_RequiredTaskAutomationGrant, total=False):
    """Task delegation independent of the browser login session."""
    stop_reason: str


class AutomationUsage(TypedDict):
    """Cumulative task usage; retries and replacement grants do not reset it."""
    model_turns: int
    tool_executions: int
    allocated_microusd: Money


class _RequiredProgressSignal(TypedDict):
    id: str
    organization_id: str
    task_id: str
    grant_id: str
    code: str
    detector_version: str
    evidence_ids: list[str]
    snapshot_digest: str
    created_at: str


class ProgressSignal(_RequiredProgressSignal, total=False):
    """Conservative stop evidence, never a proof that a model claim is false."""
    fingerprint: str


class _RequiredTaskAutomationState(TypedDict):
    usage: AutomationUsage
    signals: list[ProgressSignal]
    available_commands: list[str]


class TaskAutomationState(_RequiredTaskAutomationState, total=False):
    grant: TaskAutomationGrant


MembershipRole = Literal["owner", "admin", "member", "viewer"]

class _RequiredOrganizationMember(TypedDict):
    organization_id: str
    principal_id: str
    display_name: str
    role: MembershipRole
    active: bool
    principal_active: bool
    version: int
    created_at: str
    updated_at: str
    available_commands: list[str]
    assignable_roles: list[MembershipRole]

class OrganizationMember(_RequiredOrganizationMember, total=False):
    principal_kind: Literal["human", "service"]
    credentials_revoked_at: str

class MembershipCapabilities(TypedDict):
    organization_id: str
    can_create_members: bool
    assignable_roles: list[MembershipRole]
    self_management_allowed: Literal[False]

class MembershipAuditState(TypedDict):
    display_name: str
    role: MembershipRole
    active: bool
    version: int

class _RequiredMembershipAuditEntry(TypedDict):
    id: str
    organization_id: str
    actor_id: str
    target_principal_id: str
    action: str
    reason: str
    after: MembershipAuditState
    created_at: str

class MembershipAuditEntry(_RequiredMembershipAuditEntry, total=False):
    before: MembershipAuditState

PrincipalKind = Literal["human", "service"]
ServiceRole = Literal["member", "viewer"]
LoginPolicyMode = Literal["mixed", "oidc_required"]
class _RequiredOrganizationLoginPolicy(TypedDict):
    organization_id: str
    mode: LoginPolicyMode
    oidc_issuer: str
    service_access_enabled: bool
    max_service_key_ttl_seconds: int
    version: int
    created_at: str
    updated_at: str
    available_commands: list[str]
    can_require_oidc: bool
    readiness_reasons: list[str]
class OrganizationLoginPolicy(_RequiredOrganizationLoginPolicy, total=False):
    configured_oidc_issuer: str
class _RequiredServiceAccount(TypedDict):
    organization_id: str
    principal_id: str
    principal_kind: Literal["service"]
    display_name: str
    role: ServiceRole
    active: bool
    principal_active: bool
    version: int
    created_at: str
    updated_at: str
    available_commands: list[str]
    assignable_roles: list[ServiceRole]
class ServiceAccount(_RequiredServiceAccount, total=False):
    credentials_revoked_at: str
class ServiceKey(TypedDict):
    id: str
    organization_id: str
    principal_id: str
    role_ceiling: ServiceRole
    version: int
    revoked: bool
    created_at: str
    expires_at: str
    status: str
    available_commands: list[str]
class _RequiredOrganizationSecurityAuditState(TypedDict):
    version: int
class OrganizationSecurityAuditState(_RequiredOrganizationSecurityAuditState, total=False):
    mode: LoginPolicyMode
    oidc_issuer: str
    service_access_enabled: bool
    max_service_key_ttl_seconds: int
    display_name: str
    role: ServiceRole
    active: bool
    role_ceiling: ServiceRole
    expires_at: str
    revoked: bool
class _RequiredOrganizationSecurityAuditEntry(TypedDict):
    id: str
    organization_id: str
    actor_type: str
    object_type: str
    object_id: str
    action: str
    reason: str
    after: OrganizationSecurityAuditState
    created_at: str
class OrganizationSecurityAuditEntry(_RequiredOrganizationSecurityAuditEntry, total=False):
    actor_id: str
    target_principal_id: str
    before: OrganizationSecurityAuditState

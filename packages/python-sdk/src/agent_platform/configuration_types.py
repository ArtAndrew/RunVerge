# SPDX-License-Identifier: Apache-2.0
"""Public wire declarations; trusted host and worker profiles are deliberately absent."""
from typing import Literal, TypedDict, TypeAlias

ConfigurationAction: TypeAlias = Literal["inspect", "plan", "validate", "apply", "verify", "delegate"]
ConfigurationKind: TypeAlias = Literal["model", "mcp"]

class ConfigurationDestinationRef(TypedDict):
    id: str
    version: int

class ConfigurationScope(TypedDict):
    destination_id: str
    destination_version: int
    kind: ConfigurationKind
    target_id: str
    credential_purpose_id: str
    credential_purpose_version: int

class _RequiredConfigurationDestination(TypedDict):
    id: str
    organization_id: str
    version: int
    kind: ConfigurationKind
    name: str
    target_id: str
    credential_purpose_id: str
    credential_purpose_version: int
    requires_approval: bool
    max_verify_charge_microusd: str
    discovery_unmetered_approved: bool
    active: bool
    created_at: str


class ConfigurationDestination(_RequiredConfigurationDestination, total=False):
    model: str

class ConfigurationCatalog(TypedDict):
    organization_id: str
    revision: str
    audience: Literal["platform.configuration.v1"]
    actions: list[ConfigurationAction]
    can_issue_root_grants: bool
    can_approve: bool
    max_operations: int
    max_grant_depth: int
    available_commands: list[str]

class _RequiredCapabilityEnvelope(TypedDict):
    id: str
    organization_id: str
    audience: Literal["platform.configuration.v1"]
    issuer_id: str
    subject_id: str
    depth: int
    version: int
    actions: list[ConfigurationAction]
    scopes: list[ConfigurationScope]
    max_plans: int
    used_plans: int
    max_verifications: int
    used_verifications: int
    max_allocated_charge_microusd: str
    allocated_charge_microusd: str
    revoked: bool
    status: str
    created_at: str
    expires_at: str
    available_commands: list[str]


class CapabilityEnvelope(_RequiredCapabilityEnvelope, total=False):
    parent_grant_id: str

class _RequiredCreateConfigurationGrant(TypedDict):
    subject_id: str
    actions: list[ConfigurationAction]
    destinations: list[ConfigurationDestinationRef]
    ttl_seconds: int
    max_plans: int
    max_verifications: int
    max_allocated_charge_microusd: str
    reason: str


class CreateConfigurationGrant(_RequiredCreateConfigurationGrant, total=False):
    parent_grant_id: str
    expected_parent_version: int

class RevokeConfigurationGrant(TypedDict):
    expected_version: int
    reason: str

class ConfigurationOperationInput(TypedDict):
    id: str
    destination_id: str
    destination_version: int
    expected_resource_version: int
    active: bool

class ConfigurationOperation(TypedDict):
    id: str
    scope: ConfigurationScope
    expected_resource_version: int
    active: bool

class ConfigurationDiff(TypedDict):
    operation_id: str
    kind: ConfigurationKind
    target_id: str
    expected_version: int
    new_version: int
    active: bool
    retired_versions: list[int]
    may_stop_pinned_tasks: bool

class _RequiredCreateConfigurationPlan(TypedDict):
    base_revision: str
    ttl_seconds: int
    operations: list[ConfigurationOperationInput]
    reason: str


class CreateConfigurationPlan(_RequiredCreateConfigurationPlan, total=False):
    grant_id: str

class _RequiredConfigurationPlan(TypedDict):
    id: str
    organization_id: str
    actor_id: str
    version: int
    status: str
    base_revision: str
    digest: str
    operations: list[ConfigurationOperation]
    diff: list[ConfigurationDiff]
    reason: str
    created_at: str
    expires_at: str
    applied_revision: str
    available_commands: list[str]


class ConfigurationPlan(_RequiredConfigurationPlan, total=False):
    grant_id: str
    applied_at: str
    receipt_id: str

class _RequiredPolicyDecision(TypedDict):
    id: str
    organization_id: str
    action: str
    target_id: str
    outcome: str
    reason_codes: list[str]
    configuration_revision: str
    obligations: list[str]
    evaluated_at: str


class PolicyDecision(_RequiredPolicyDecision, total=False):
    grant_id: str

class ValidateConfigurationPlan(TypedDict):
    expected_version: int
    plan_digest: str

class ConfigurationValidation(TypedDict):
    id: str
    organization_id: str
    plan_id: str
    plan_version: int
    plan_digest: str
    base_revision: str
    current_revision: str
    valid: bool
    decision: PolicyDecision
    diff: list[ConfigurationDiff]
    created_at: str

class ApproveConfigurationPlan(TypedDict):
    expected_version: int
    plan_digest: str
    validation_id: str
    reason: str

class ConfigurationApproval(TypedDict):
    id: str
    organization_id: str
    plan_id: str
    plan_digest: str
    validation_id: str
    principal_id: str
    reason: str
    created_at: str

class StartConfigurationVerification(TypedDict):
    expected_plan_version: int
    plan_digest: str
    operation_id: str
    max_charge_microusd: str
    acknowledge_cost: Literal[True]
    reason: str

class _RequiredConfigurationVerification(TypedDict):
    id: str
    organization_id: str
    plan_id: str
    operation_id: str
    actor_id: str
    scope: ConfigurationScope
    resource_version: int
    kind: ConfigurationKind
    status: str
    version: int
    max_charge_microusd: str
    cost_status: str
    tool_count: int
    created_at: str
    updated_at: str


class ConfigurationVerification(_RequiredConfigurationVerification, total=False):
    grant_id: str
    task_id: str
    protocol_version: str
    catalog_digest: str
    response_digest: str
    reason_code: str

class ConfigurationResource(TypedDict):
    organization_id: str
    kind: ConfigurationKind
    id: str
    version: int
    scope: ConfigurationScope
    active: bool
    created_at: str

class _RequiredConfigurationAuditEntry(TypedDict):
    id: str
    organization_id: str
    actor_type: str
    object_type: str
    object_id: str
    action: str
    digest: str
    reason: str
    created_at: str


class ConfigurationAuditEntry(_RequiredConfigurationAuditEntry, total=False):
    actor_id: str

ApplyConfigurationPlan: TypeAlias = ApproveConfigurationPlan

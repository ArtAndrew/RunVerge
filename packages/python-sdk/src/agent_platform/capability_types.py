# SPDX-License-Identifier: Apache-2.0
"""Descriptive catalogue wire types. These are never execution permissions."""
from typing import Literal, TypedDict
from .types import CurrentIdentity

CapabilityEffect = Literal["read_only", "platform_write", "queues_external_work", "external_dispatch", "records_observation"]
CapabilitySurface = Literal["http", "cli", "python", "typescript", "mcp", "workspace"]


class CapabilityBinding(TypedDict):
    surface: CapabilitySurface
    status: Literal["available", "operator_opt_in", "not_exposed"]
    name: str


class CapabilityOperation(TypedDict):
    id: str
    capability_id: str
    method: Literal["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]
    path: str
    input_schema: str
    output_schema: str
    response_kind: Literal["object", "list", "sse"]
    success_status: int
    access_model: Literal["public", "current_membership", "role_and_resource", "scoped_configuration", "human_owner"]
    permission_alternatives: list[list[str]]
    runtime_guards: list[str]
    effect: CapabilityEffect
    cost_semantics: Literal["none", "may_reserve_and_queue", "reserves_upper_bound", "possible_charge_existing_reservation", "observation_no_settlement"]
    retry_policy: Literal["read_with_current_authorization", "explicit_same_key_after_inspection", "scoped_stream_resume"]
    bindings: list[CapabilityBinding]
    documentation: list[str]


class CapabilityProfile(TypedDict):
    id: str
    name: str
    status: Literal["implemented", "bounded", "not_implemented"]
    summary: str
    constraints: list[str]
    documentation: list[str]


class CapabilityRole(TypedDict):
    principal_kind: Literal["human", "service"]
    role: Literal["viewer", "member", "admin", "owner"]
    permissions: list[str]


class CapabilityStep(TypedDict):
    id: str
    instruction: str
    actor: str
    operation_ids: list[str]
    commands: list[list[str]]
    effect: CapabilityEffect
    cost_acknowledgement: bool


class CapabilityOnboarding(TypedDict):
    id: str
    persona: str
    status: Literal["available", "partial", "not_implemented"]
    summary: str
    prerequisites: list[str]
    steps: list[CapabilityStep]
    documentation: list[str]


class ProductCapabilities(TypedDict):
    catalog_version: Literal["1"]
    api_contract: Literal["0.14.0"]
    assessment: Literal["support_and_role_observation"]
    read_only: Literal[True]
    observed_at: str
    identity: CurrentIdentity
    authorization_rules: list[str]
    operations: list[CapabilityOperation]
    profiles: list[CapabilityProfile]
    roles: list[CapabilityRole]
    onboarding: list[CapabilityOnboarding]
    remediation_command: list[str]

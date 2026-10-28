# SPDX-License-Identifier: Apache-2.0
"""Small, synchronous HTTP client using only the Python standard library."""

from __future__ import annotations

import json
import math
import re
from collections.abc import Mapping, Iterator
from contextlib import contextmanager
from typing import Any, cast
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from .errors import APIError, ProtocolError, TransportError
from .configuration import configuration_facts, configuration_input
from .capabilities import capability_facts
from .capability_types import ProductCapabilities
from . import configuration_types as config_types
from . import events as event_contract
from .types import TaskEventSnapshot, TaskEventBatch, TaskEventStreamFrame
from .types import ModelToolProposal, ModelToolResultBinding, ToolInfo, ToolExecution, ToolExecutionView, CurrentIdentity, Material, MaterialRevision, MaterialContent, Action, Agent, Approval, Artifact, AvailableModel, CostReport, Event, Operation, Page, Run, Task, ReviewDecision, TaskCandidate, TaskDecision, TeamDefinition
from .money import parse_micro_usd
from .types import AutomationToolPin, TaskAutomationState, ProgressSignal
from .security import credential_free, login_policy_facts, service_account_facts, service_key_facts, security_audit_facts, policy_input, service_input, key_revoke_input
from .types import PrincipalKind, ServiceRole, LoginPolicyMode, OrganizationLoginPolicy, ServiceAccount, ServiceKey, OrganizationSecurityAuditState, OrganizationSecurityAuditEntry
from .types import MembershipRole, OrganizationMember, MembershipCapabilities, MembershipAuditEntry


class _NoRedirects(HTTPRedirectHandler):
    """Never forward authorization headers to an HTTP redirect target."""

    def redirect_request(self, req: Any, fp: Any, code: int, msg: str,
                         headers: Any, newurl: str) -> None:
        return None


def _automation_pins(values: list[AutomationToolPin]) -> list[AutomationToolPin]:
    if not isinstance(values, list) or not 1 <= len(values) <= 16:
        raise ValueError("tool_pins must contain 1..16 exact pins")
    seen: set[str] = set()
    pins = []
    for pin in values:
        if not isinstance(pin, dict):
            raise ValueError("tool pin must be an object")
        identifier = _text(_nonempty(pin.get("tool_id"), "tool_id"), "tool_id", 128)
        if identifier in seen:
            raise ValueError("tool_pins must not repeat a tool")
        seen.add(identifier)
        pins.append({"tool_id": identifier, "tool_version": _integer(pin.get("tool_version"), "tool_version", minimum=1),
                     "tool_profile_digest": _digest(pin.get("tool_profile_digest"))})
    return cast(list[AutomationToolPin], pins)


def _automation_charge(value: str) -> str:
    amount = parse_micro_usd(value)
    if not 1 <= amount <= 1_000_000_000_000:
        raise ValueError("max_charge_microusd must be a canonical decimal string between 1 and 1000000000000")
    return value


def _automation_state(value: dict[str, Any]) -> TaskAutomationState:
    usage = value.get("usage")
    if not isinstance(usage, dict) or not isinstance(value.get("signals"), list) or not all(isinstance(item, dict) for item in value["signals"]) or not isinstance(value.get("available_commands"), list) or not all(isinstance(item, str) for item in value["available_commands"]):
        raise ProtocolError("Invalid automation state")
    for key in ("model_turns", "tool_executions"):
        if type(usage.get(key)) is not int or usage[key] < 0:
            raise ProtocolError("Invalid automation usage")
    _response_money(usage.get("allocated_microusd"))
    if "grant" in value:
        grant = value["grant"]
        if not isinstance(grant, dict) or not isinstance(grant.get("id"), str) or not grant["id"] or type(grant.get("version")) is not int or grant["version"] < 1:
            raise ProtocolError("Invalid automation grant")
        try:
            _automation_charge(grant.get("max_charge_microusd"))
        except (ValueError, TypeError):
            raise ProtocolError("Invalid automation charge ceiling") from None
    return cast(TaskAutomationState, value)


def _nonempty(value: str, name: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"{name} must be a nonempty string")
    return value


def _header(value: str, name: str) -> str:
    _nonempty(value, name)
    if any(ord(character) < 32 or ord(character) > 126 for character in value):
        raise ValueError(f"{name} must contain only printable ASCII characters")
    return value


def _identifier(value: str, name: str) -> str:
    _nonempty(value, name)
    if value in (".", "..") or "/" in value or "\\" in value:
        raise ValueError(f"{name} must be a single identifier")
    return quote(value, safe="")


def _integer(value: int, name: str, *, minimum: int = 0, maximum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise ValueError(f"{name} must be an integer greater than or equal to {minimum}")
    if maximum is not None and value > maximum:
        raise ValueError(f"{name} cannot exceed {maximum}")
    return value


def _text(value: str, name: str, maximum: int) -> str:
    if not isinstance(value, str) or "\x00" in value:
        raise ValueError(f"{name} must be text without NUL")
    if len(value.encode("utf-8")) > maximum:
        raise ValueError(f"{name} exceeds its UTF-8 byte limit")
    return value


def _digest(value: str) -> str:
    if not isinstance(value, str) or re.fullmatch(r"[0-9a-f]{64}", value) is None:
        raise ValueError("content_digest must be a lowercase SHA-256 digest")
    return value


def _tool_arguments(value: str) -> str:
    _text(value, "arguments_json", 32768)
    def pairs(items):
        result = {}
        for key, item in items:
            if key in result:
                raise ValueError("arguments_json contains duplicate object keys")
            result[key] = item
        return result
    def constant(value):
        raise ValueError("arguments_json contains a non-JSON number")
    try:
        parsed = json.loads(value, object_pairs_hook=pairs, parse_int=str, parse_float=str, parse_constant=constant)
    except (json.JSONDecodeError, RecursionError) as exc:
        raise ValueError("arguments_json must be a JSON object") from None
    if not isinstance(parsed, dict):
        raise ValueError("arguments_json must be a JSON object")
    return value  # Preserve original numeric lexemes, spaces and UTF-8 exactly.


def _occurrence(value: str) -> str:
    if not isinstance(value, str) or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", value) is None:
        raise ValueError("occurrence_id must be a stable 1..128 character identifier")
    return value


def _tool_money(value):
    if not isinstance(value, dict) or "max_charge_microusd" not in value:
        raise ProtocolError("Missing tool charge bound")
    if _response_money(value["max_charge_microusd"]) > 1_000_000_000_000:
        raise ProtocolError("Invalid tool charge bound")
    return value


def _model_results(values: list[ModelToolResultBinding]) -> list[ModelToolResultBinding]:
    if not isinstance(values, list) or not 1 <= len(values) <= 8:
        raise ValueError("results must bind 1..8 terminal proposals")
    results, seen = [], set()
    for value in values:
        if not isinstance(value, dict): raise ValueError("result must be an exact terminal binding")
        proposal = _text(_nonempty(value.get("proposal_id"), "proposal_id"), "proposal_id", 128)
        if proposal in seen: raise ValueError("results must not repeat a proposal")
        seen.add(proposal)
        result = {"proposal_id": proposal, "execution_id": _text(_nonempty(value.get("execution_id"), "execution_id"), "execution_id", 128),
                  "execution_version": _integer(value.get("execution_version"), "execution_version", minimum=1), "status": value.get("status")}
        if result["status"] in ("succeeded", "rejected"):
            result["receipt_id"] = _text(_nonempty(value.get("receipt_id"), "receipt_id"), "receipt_id", 128)
            result["content_digest"] = _digest(value.get("content_digest"))
        elif result["status"] in ("denied", "expired", "cancelled"):
            if value.get("receipt_id") or value.get("content_digest"): raise ValueError("unsent results must not claim an effect receipt")
        else: raise ValueError("unknown or nonterminal tool effects cannot continue a model")
        results.append(cast(ModelToolResultBinding, result))
    return results


def _tool_view(value) -> ToolExecutionView:
    if not isinstance(value, dict):
        raise ProtocolError("Expected tool execution view")
    for field in ("execution", "tool", "approval", "task"):
        nested = value.get(field)
        if not isinstance(nested, dict) or not isinstance(nested.get("id"), str) or not nested["id"]:
            raise ProtocolError("Missing identified tool view component")
    _tool_money(value.get("execution")); _tool_money(value.get("tool"))
    if not isinstance(value.get("task"), dict):
        raise ProtocolError("Missing tool task snapshot")
    _task_facts(value["task"])
    if not isinstance(value.get("available_commands"), list) or any(not isinstance(item, str) for item in value["available_commands"]):
        raise ProtocolError("Invalid available tool commands")
    return cast(ToolExecutionView, value)


def _team(value: TeamDefinition) -> TeamDefinition:
    if not isinstance(value, dict) or value.get("pattern") not in ("sequence", "parallel", "delegate_verify") or value.get("join_policy") not in ("all", "allow_partial"):
        raise ValueError("unsupported team pattern or join policy")
    members = value.get("members")
    if not isinstance(members, list) or not 1 <= len(members) <= 32:
        raise ValueError("team requires 1..32 members")
    if value["pattern"] == "delegate_verify" and len(members) != 2:
        raise ValueError("delegate_verify requires two members")
    _integer(value.get("max_depth"), "max_depth", minimum=1, maximum=16)
    _integer(value.get("max_children"), "max_children", minimum=1, maximum=32)
    if "max_parallel" in value:
        _integer(value["max_parallel"], "max_parallel", minimum=1, maximum=len(members))
    ids: set[str] = set()
    for member in members:
        if not isinstance(member, dict):
            raise ValueError("team members must be objects")
        identifier = _nonempty(member.get("id"), "member.id")
        _nonempty(member.get("agent_id"), "member.agent_id")
        _integer(member.get("agent_version"), "member.agent_version", minimum=1)
        if identifier in ids:
            raise ValueError("team member IDs must be unique")
        ids.add(identifier)
        if "task" in member:
            _text(_nonempty(member["task"], "member.task"), "member.task", 4096)
    return value


def _response_money(value: Any, *, signed: bool = False) -> int:
    try:
        return parse_micro_usd(value, signed=signed)
    except (TypeError, ValueError):
        raise ProtocolError("Expected an exact canonical decimal int64 micro-USD string") from None


def _task_facts(task: dict[str, Any]) -> Task:
    _response_money(task.get("reserved_microusd"))
    _response_money(task.get("spent_microusd"))
    return cast(Task, task)



def _membership_role(value: str) -> MembershipRole:
    if value not in ("owner", "admin", "member", "viewer"):
        raise ValueError("role must be owner, admin, member or viewer")
    return cast(MembershipRole, value)


def _membership_state(value):
    if not isinstance(value, dict): raise ValueError("member state must be an object")
    _text(_nonempty(value.get("display_name"), "display_name"), "display_name", 120)
    _membership_role(value.get("role"))
    _integer(value.get("version"), "version", minimum=1, maximum=9007199254740991)
    if type(value.get("active")) is not bool: raise ValueError("active must be an explicit boolean")
    return value


def _member_facts(value, organization_id: str, principal_id: str | None = None) -> OrganizationMember:
    try:
        _membership_state(value)
        if value.get("principal_kind", "human") not in ("human", "service"): raise ValueError("invalid principal kind")
        if value.get("organization_id") != organization_id or type(value.get("principal_active")) is not bool:
            raise ValueError("incorrect member scope or principal state")
        _identifier(value.get("principal_id"), "principal_id")
        if principal_id is not None and value["principal_id"] != principal_id: raise ValueError("incorrect member identity")
        for field in ("created_at", "updated_at"):
            _text(_nonempty(value.get(field), field), field, 128)
        if "credentials_revoked_at" in value:
            _text(_nonempty(value["credentials_revoked_at"], "credentials_revoked_at"), "credentials_revoked_at", 128)
        if not isinstance(value.get("available_commands"), list) or any(not isinstance(item, str) for item in value["available_commands"]): raise ValueError("invalid member commands")
        if not isinstance(value.get("assignable_roles"), list): raise ValueError("invalid assignable roles")
        for role in value["assignable_roles"]: _membership_role(role)
    except (ValueError, TypeError, KeyError):
        raise ProtocolError("Invalid organization member response") from None
    return cast(OrganizationMember, value)


def _membership_capabilities(value, organization_id: str) -> MembershipCapabilities:
    try:
        if not isinstance(value, dict) or value.get("organization_id") != organization_id or type(value.get("can_create_members")) is not bool or value.get("self_management_allowed") is not False or not isinstance(value.get("assignable_roles"), list):
            raise ValueError("invalid membership capabilities")
        for role in value["assignable_roles"]: _membership_role(role)
    except (ValueError, TypeError):
        raise ProtocolError("Invalid membership capabilities response") from None
    return cast(MembershipCapabilities, value)


def _membership_audit(value, organization_id: str) -> MembershipAuditEntry:
    try:
        if not isinstance(value, dict) or value.get("organization_id") != organization_id: raise ValueError("incorrect audit scope")
        for field in ("id", "actor_id", "target_principal_id"): _identifier(value.get(field), field)
        _text(_nonempty(value.get("reason"), "reason"), "reason", 4096)
        _text(_nonempty(value.get("created_at"), "created_at"), "created_at", 128)
        if value.get("action") not in ("member.created", "member.updated", "member.credentials_revoked"): raise ValueError("invalid audit action")
        _membership_state(value.get("after"))
        if "before" in value: _membership_state(value["before"])
    except (ValueError, TypeError, KeyError):
        raise ProtocolError("Invalid membership audit response") from None
    return cast(MembershipAuditEntry, value)


class Client:
    """Connect to one organization on an Agent Platform server.

    ``base_url`` may be a server origin or the API root ending in ``/api/v1``.
    TLS certificate verification is enabled by Python's default HTTPS handler.
    Redirects and automatic retries are disabled. Use HTTPS outside local
    development, and reuse a mutation's idempotency key after a lost response.
    """

    def __init__(
        self,
        base_url: str,
        *,
        token: str,
        organization_id: str,
        timeout: float = 30.0,
    ) -> None:
        parts = urlsplit(_nonempty(base_url, "base_url"))
        if parts.scheme not in ("http", "https") or not parts.hostname:
            raise ValueError("base_url must be an absolute HTTP or HTTPS URL")
        if parts.username or parts.password or parts.query or parts.fragment:
            raise ValueError("base_url cannot contain credentials, query, or fragment")
        if isinstance(timeout, bool) or not math.isfinite(timeout) or timeout <= 0:
            raise ValueError("timeout must be a finite positive number")
        self.base_url = base_url.rstrip("/")
        if not self.base_url.endswith("/api/v1"):
            self.base_url += "/api/v1"
        self.timeout = timeout
        self._token = _header(token, "token")
        self._observed_principal_id: str | None = None
        self.organization_id = _header(organization_id, "organization_id")
        self._opener = build_opener(ProxyHandler({}), _NoRedirects())

    def __repr__(self) -> str:
        return f"Client(base_url={self.base_url!r}, organization_id={self.organization_id!r})"

    def configuration_catalog(self) -> config_types.ConfigurationCatalog:
        return cast(config_types.ConfigurationCatalog, configuration_facts("catalog", self._object(self._request("GET", "/configuration/catalog")), self.organization_id))

    def capabilities(self) -> ProductCapabilities:
        """Read descriptive support and role prerequisites, never an execution permit."""
        value = capability_facts(self._object(self._request("GET", "/capabilities")), self.organization_id, self._observed_principal_id)
        self._observed_principal_id = value["identity"]["principal_id"]
        return value

    def configuration_destinations(self, *, cursor: str | None = None, limit: int | None = None) -> Page[config_types.ConfigurationDestination]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        query = {"cursor": cursor, "limit": limit}
        result = self._page(self._request("GET", "/configuration/destinations", query=query))
        return Page([cast(config_types.ConfigurationDestination, configuration_facts("destination", value, self.organization_id)) for value in result.data], result.next_cursor)

    def configuration_resources(self, *, cursor: str | None = None, limit: int | None = None) -> Page[config_types.ConfigurationResource]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        query = {"cursor": cursor, "limit": limit}
        result = self._page(self._request("GET", "/configuration/resources", query=query))
        return Page([cast(config_types.ConfigurationResource, configuration_facts("resource", value, self.organization_id)) for value in result.data], result.next_cursor)

    def configuration_grants(self, *, cursor: str | None = None, limit: int | None = None) -> Page[config_types.CapabilityEnvelope]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        query = {"cursor": cursor, "limit": limit}
        result = self._page(self._request("GET", "/configuration/grants", query=query))
        return Page([cast(config_types.CapabilityEnvelope, configuration_facts("grant", value, self.organization_id)) for value in result.data], result.next_cursor)

    def configuration_plans(self, *, cursor: str | None = None, limit: int | None = None) -> Page[config_types.ConfigurationPlan]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        query = {"cursor": cursor, "limit": limit}
        result = self._page(self._request("GET", "/configuration/plans", query=query))
        return Page([cast(config_types.ConfigurationPlan, configuration_facts("plan", value, self.organization_id)) for value in result.data], result.next_cursor)

    def configuration_verifications(self, *, cursor: str | None = None, limit: int | None = None, plan_id: str | None = None) -> Page[config_types.ConfigurationVerification]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        query = {"cursor": cursor, "limit": limit}
        if plan_id is not None:
            _identifier(plan_id, "plan_id")
            query["plan_id"] = plan_id
        result = self._page(self._request("GET", "/configuration/verifications", query=query))
        return Page([cast(config_types.ConfigurationVerification, configuration_facts("verification", value, self.organization_id, plan_id=plan_id)) for value in result.data], result.next_cursor)

    def configuration_audit(self, *, cursor: str | None = None, limit: int | None = None) -> Page[config_types.ConfigurationAuditEntry]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        query = {"cursor": cursor, "limit": limit}
        result = self._page(self._request("GET", "/configuration/audit", query=query))
        return Page([cast(config_types.ConfigurationAuditEntry, configuration_facts("audit", value, self.organization_id)) for value in result.data], result.next_cursor)

    def configuration_grant(self, grant_id: str) -> config_types.CapabilityEnvelope:
        return cast(config_types.CapabilityEnvelope, configuration_facts("grant", self._object(self._request("GET", f"/configuration/grants/{_identifier(grant_id, 'grant_id')}")), self.organization_id, grant_id))

    def configuration_plan(self, plan_id: str) -> config_types.ConfigurationPlan:
        return cast(config_types.ConfigurationPlan, configuration_facts("plan", self._object(self._request("GET", f"/configuration/plans/{_identifier(plan_id, 'plan_id')}")), self.organization_id, plan_id))

    def configuration_verification(self, verification_id: str) -> config_types.ConfigurationVerification:
        return cast(config_types.ConfigurationVerification, configuration_facts("verification", self._object(self._request("GET", f"/configuration/verifications/{_identifier(verification_id, 'verification_id')}")), self.organization_id, verification_id))

    def create_configuration_grant(self, input: config_types.CreateConfigurationGrant, *, idempotency_key: str) -> config_types.CapabilityEnvelope:
        body = configuration_input("createGrant", input)
        return cast(config_types.CapabilityEnvelope, configuration_facts("grant", self._object(self._request("POST", "/configuration/grants", body=body, idempotency_key=idempotency_key)), self.organization_id))

    def revoke_configuration_grant(self, grant_id: str, input: config_types.RevokeConfigurationGrant, *, idempotency_key: str) -> config_types.CapabilityEnvelope:
        body = configuration_input("revokeGrant", input)
        return cast(config_types.CapabilityEnvelope, configuration_facts("grant", self._object(self._request("POST", f"/configuration/grants/{_identifier(grant_id, 'grant_id')}/revoke", body=body, idempotency_key=idempotency_key)), self.organization_id, grant_id))

    def create_configuration_plan(self, input: config_types.CreateConfigurationPlan, *, idempotency_key: str) -> config_types.ConfigurationPlan:
        body = configuration_input("createPlan", input)
        return cast(config_types.ConfigurationPlan, configuration_facts("plan", self._object(self._request("POST", "/configuration/plans", body=body, idempotency_key=idempotency_key)), self.organization_id))

    def validate_configuration_plan(self, plan_id: str, input: config_types.ValidateConfigurationPlan, *, idempotency_key: str) -> config_types.ConfigurationValidation:
        body = configuration_input("validatePlan", input)
        return cast(config_types.ConfigurationValidation, configuration_facts("validation", self._object(self._request("POST", f"/configuration/plans/{_identifier(plan_id, 'plan_id')}/validate", body=body, idempotency_key=idempotency_key)), self.organization_id, plan_id=plan_id))

    def approve_configuration_plan(self, plan_id: str, input: config_types.ApproveConfigurationPlan, *, idempotency_key: str) -> config_types.ConfigurationApproval:
        body = configuration_input("approvePlan", input)
        return cast(config_types.ConfigurationApproval, configuration_facts("approval", self._object(self._request("POST", f"/configuration/plans/{_identifier(plan_id, 'plan_id')}/approve", body=body, idempotency_key=idempotency_key)), self.organization_id, plan_id=plan_id))

    def apply_configuration_plan(self, plan_id: str, input: config_types.ApplyConfigurationPlan, *, idempotency_key: str) -> config_types.ConfigurationPlan:
        body = configuration_input("applyPlan", input)
        return cast(config_types.ConfigurationPlan, configuration_facts("plan", self._object(self._request("POST", f"/configuration/plans/{_identifier(plan_id, 'plan_id')}/apply", body=body, idempotency_key=idempotency_key)), self.organization_id, plan_id))

    def start_configuration_verification(self, plan_id: str, input: config_types.StartConfigurationVerification, *, idempotency_key: str) -> config_types.ConfigurationVerification:
        body = configuration_input("verify", input)
        return cast(config_types.ConfigurationVerification, configuration_facts("verification", self._object(self._request("POST", f"/configuration/plans/{_identifier(plan_id, 'plan_id')}/verify", body=body, idempotency_key=idempotency_key)), self.organization_id, plan_id=plan_id))

    def create_task(
        self,
        goal: str,
        agent_id: str,
        budget_microusd: int,
        *,
        idempotency_key: str,
        max_runs: int | None = None,
        execution_timeout_seconds: int | None = None,
    ) -> Task:
        """Submit work. The budget is an integer number of millionths of USD."""
        _nonempty(goal, "goal")
        _nonempty(agent_id, "agent_id")
        _integer(budget_microusd, "budget_microusd", minimum=1, maximum=1_000_000_000_000)
        body = {"goal": goal, "agent_id": agent_id, "budget_microusd": budget_microusd}
        if max_runs is not None:
            body["max_runs"] = _integer(max_runs, "max_runs", minimum=1, maximum=10)
        if execution_timeout_seconds is not None:
            body["execution_timeout_seconds"] = _integer(execution_timeout_seconds, "execution_timeout_seconds", minimum=1, maximum=2_592_000)
        return _task_facts(self._object(self._request(
            "POST", "/tasks", body=body,
            idempotency_key=idempotency_key,
        )))

    def list_tasks(self, *, cursor: str | None = None, limit: int | None = None) -> Page[Task]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        page = self._page(self._request(
            "GET", "/tasks", query={"cursor": cursor, "limit": limit},
        ))
        return Page([_task_facts(task) for task in page.data], page.next_cursor)

    def get_task(self, task_id: str) -> Task:
        return _task_facts(self._object(self._request(
            "GET", f"/tasks/{_identifier(task_id, 'task_id')}",
        )))

    def cancel_task(self, task_id: str, *, idempotency_key: str) -> Task:
        return _task_facts(self._object(self._request(
            "POST", f"/tasks/{_identifier(task_id, 'task_id')}/cancel",
            body={}, idempotency_key=idempotency_key,
        )))

    def review_task(
        self, task_id: str, *, expected_version: int, candidate_id: str,
        artifact_id: str, artifact_version: int, content_digest: str,
        decision: ReviewDecision, idempotency_key: str, reason: str | None = None,
    ) -> Task:
        """Decide on the exact candidate; this neither settles cost nor proves truth."""
        if decision not in ("accept", "reject", "request_changes"):
            raise ValueError("unsupported review decision")
        if decision != "accept":
            _nonempty(reason, "reason")
        body = {
            "expected_version": _integer(expected_version, "expected_version", minimum=1),
            "candidate_id": _nonempty(candidate_id, "candidate_id"),
            "artifact_id": _nonempty(artifact_id, "artifact_id"),
            "artifact_version": _integer(artifact_version, "artifact_version", minimum=1),
            "content_digest": _digest(content_digest), "decision": decision,
        }
        if reason is not None:
            body["reason"] = _text(reason, "reason", 4096)
        return _task_facts(self._object(self._request(
            "POST", f"/tasks/{_identifier(task_id, 'task_id')}/review",
            body=body, idempotency_key=idempotency_key,
        )))

    def retry_task(
        self, task_id: str, *, expected_version: int, reason: str,
        acknowledge_new_cost: bool, idempotency_key: str, feedback: str | None = None,
    ) -> Task:
        """Start a new potentially billable root attempt; preserve existing holds."""
        if acknowledge_new_cost is not True:
            raise ValueError("acknowledge_new_cost must explicitly be True")
        body = {"expected_version": _integer(expected_version, "expected_version", minimum=1),
                "reason": _text(_nonempty(reason, "reason"), "reason", 4096), "acknowledge_new_cost": True}
        if feedback is not None:
            body["feedback"] = _text(feedback, "feedback", 8192)
        return _task_facts(self._object(self._request(
            "POST", f"/tasks/{_identifier(task_id, 'task_id')}/retry",
            body=body, idempotency_key=idempotency_key,
        )))

    def candidates(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[TaskCandidate]:
        return cast(Page[TaskCandidate], self._execution_page(task_id, "candidates", cursor, limit))

    def decisions(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[TaskDecision]:
        return cast(Page[TaskDecision], self._execution_page(task_id, "decisions", cursor, limit))

    def children(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[Task]:
        page = self._execution_page(task_id, "children", cursor, limit)
        return Page([_task_facts(task) for task in page.data], page.next_cursor)

    def membership_capabilities(self) -> MembershipCapabilities:
        """Read fresh server decisions; this never grants additional authority."""
        return _membership_capabilities(self._object(self._request("GET", "/members/capabilities")), self.organization_id)

    def list_members(self, *, cursor: str | None = None, limit: int | None = None) -> Page[OrganizationMember]:
        if limit is not None: _integer(limit, "limit", minimum=1, maximum=100)
        page = self._page(self._request("GET", "/members", query={"cursor": cursor, "limit": limit}))
        return Page([_member_facts(value, self.organization_id) for value in page.data], page.next_cursor)

    def get_member(self, principal_id: str) -> OrganizationMember:
        return _member_facts(self._object(self._request("GET", f"/members/{_identifier(principal_id, 'principal_id')}")), self.organization_id, principal_id)

    def create_member(self, display_name: str, role: MembershipRole, *, reason: str, idempotency_key: str) -> OrganizationMember:
        """Provision a NEW local principal; no email linking, invitation or secret."""
        body = {"display_name": _text(_nonempty(display_name, "display_name"), "display_name", 120), "role": _membership_role(role),
                "reason": _text(_nonempty(reason, "reason"), "reason", 4096)}
        return _member_facts(self._object(self._request("POST", "/members", body=body, idempotency_key=idempotency_key)), self.organization_id)

    def update_member(self, principal_id: str, *, expected_version: int, display_name: str, role: MembershipRole, active: bool, reason: str, idempotency_key: str) -> OrganizationMember:
        if type(active) is not bool: raise ValueError("active must be an explicit boolean")
        body = {"expected_version": _integer(expected_version, "expected_version", minimum=1, maximum=9007199254740991),
                "display_name": _text(_nonempty(display_name, "display_name"), "display_name", 120), "role": _membership_role(role),
                "active": active, "reason": _text(_nonempty(reason, "reason"), "reason", 4096)}
        return _member_facts(self._object(self._request("POST", f"/members/{_identifier(principal_id, 'principal_id')}/update", body=body, idempotency_key=idempotency_key)), self.organization_id, principal_id)

    def revoke_member_credentials(self, principal_id: str, *, expected_version: int, reason: str, idempotency_key: str) -> OrganizationMember:
        """Invalidate existing credentials in THIS org; independent task grants continue."""
        body = {"expected_version": _integer(expected_version, "expected_version", minimum=1, maximum=9007199254740991),
                "reason": _text(_nonempty(reason, "reason"), "reason", 4096)}
        return _member_facts(self._object(self._request("POST", f"/members/{_identifier(principal_id, 'principal_id')}/revoke-credentials", body=body, idempotency_key=idempotency_key)), self.organization_id, principal_id)

    def list_membership_audit(self, *, principal_id: str | None = None, cursor: str | None = None, limit: int | None = None) -> Page[MembershipAuditEntry]:
        if limit is not None: _integer(limit, "limit", minimum=1, maximum=100)
        if principal_id is not None: _identifier(principal_id, "principal_id")
        page = self._page(self._request("GET", "/membership-audit", query={"principal_id": principal_id, "cursor": cursor, "limit": limit}))
        return Page([_membership_audit(value, self.organization_id) for value in page.data], page.next_cursor)

    def get_login_policy(self) -> OrganizationLoginPolicy:
        return login_policy_facts(self._object(self._request("GET", "/login-policy")), self.organization_id)

    def update_login_policy(self, *, expected_version: int, mode: LoginPolicyMode, oidc_issuer: str, service_access_enabled: bool, max_service_key_ttl_seconds: int, reason: str, idempotency_key: str) -> OrganizationLoginPolicy:
        body = policy_input(expected_version, mode, oidc_issuer, service_access_enabled, max_service_key_ttl_seconds, reason)
        return login_policy_facts(self._object(self._request("POST", "/login-policy/update", body=body, idempotency_key=idempotency_key)), self.organization_id)

    def list_service_accounts(self, *, cursor: str | None = None, limit: int | None = None) -> Page[ServiceAccount]:
        if limit is not None: _integer(limit, "limit", minimum=1, maximum=100)
        page = self._page(self._request("GET", "/service-accounts", query={"cursor": cursor, "limit": limit}))
        return Page([service_account_facts(v, self.organization_id) for v in page.data], page.next_cursor)

    def get_service_account(self, principal_id: str) -> ServiceAccount:
        path = "/service-accounts/" + _identifier(principal_id, "principal_id")
        return service_account_facts(self._object(self._request("GET", path)), self.organization_id, principal_id)

    def create_service_account(self, display_name: str, role: ServiceRole, *, reason: str, idempotency_key: str) -> ServiceAccount:
        return service_account_facts(self._object(self._request("POST", "/service-accounts", body=service_input(display_name, role, reason), idempotency_key=idempotency_key)), self.organization_id)

    def update_service_account(self, principal_id: str, *, expected_version: int, display_name: str, role: ServiceRole, active: bool, reason: str, idempotency_key: str) -> ServiceAccount:
        body = service_input(display_name, role, reason, expected_version, active, update=True)
        return service_account_facts(self._object(self._request("POST", "/service-accounts/" + _identifier(principal_id, "principal_id") + "/update", body=body, idempotency_key=idempotency_key)), self.organization_id, principal_id)

    def list_service_keys(self, principal_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[ServiceKey]:
        if limit is not None: _integer(limit, "limit", minimum=1, maximum=100)
        page = self._page(self._request("GET", "/service-accounts/" + _identifier(principal_id, "principal_id") + "/keys", query={"cursor": cursor, "limit": limit}))
        return Page([service_key_facts(v, self.organization_id, principal_id) for v in page.data], page.next_cursor)

    def get_service_key(self, principal_id: str, key_id: str) -> ServiceKey:
        path = "/service-accounts/" + _identifier(principal_id, "principal_id") + "/keys/" + _identifier(key_id, "key_id")
        return service_key_facts(self._object(self._request("GET", path)), self.organization_id, principal_id, key_id)

    def revoke_service_key(self, principal_id: str, key_id: str, *, expected_version: int, reason: str, idempotency_key: str) -> ServiceKey:
        path = "/service-accounts/" + _identifier(principal_id, "principal_id") + "/keys/" + _identifier(key_id, "key_id") + "/revoke"
        return service_key_facts(self._object(self._request("POST", path, body=key_revoke_input(expected_version, reason), idempotency_key=idempotency_key)), self.organization_id, principal_id, key_id)

    def list_security_audit(self, *, principal_id: str | None = None, cursor: str | None = None, limit: int | None = None) -> Page[OrganizationSecurityAuditEntry]:
        if limit is not None: _integer(limit, "limit", minimum=1, maximum=100)
        if principal_id is not None: _identifier(principal_id, "principal_id")
        page = self._page(self._request("GET", "/security-audit", query={"principal_id": principal_id, "cursor": cursor, "limit": limit}))
        return Page([security_audit_facts(v, self.organization_id, principal_id) for v in page.data], page.next_cursor)

    def get_current_identity(self) -> CurrentIdentity:
        """Read the current authenticated membership without exposing credentials."""
        value = self._object(self._request("GET", "/me"))
        try:
            if not credential_free(value) or value.get("organization_id") != self.organization_id or value.get("principal_kind") not in ("human", "service"): raise ValueError("invalid current identity scope/kind")
            _identifier(value.get("principal_id"), "principal_id"); _membership_role(value.get("role"))
            if not isinstance(value.get("permissions"), list) or any(not isinstance(v, str) for v in value["permissions"]): raise ValueError("invalid current permissions")
        except (ValueError, TypeError): raise ProtocolError("Invalid current identity response") from None
        self._observed_principal_id = value["principal_id"]
        return cast(CurrentIdentity, value)

    def list_agents(self, *, cursor: str | None = None, limit: int | None = None) -> Page[Agent]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        return cast(Page[Agent], self._page(self._request(
            "GET", "/agents", query={"cursor": cursor, "limit": limit},
        )))

    def list_models(self, *, cursor: str | None = None, limit: int | None = None) -> Page[AvailableModel]:
        """List available organization model profiles, without credentials/endpoints."""
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        result = cast(Page[AvailableModel], self._page(self._request(
            "GET", "/models", query={"cursor": cursor, "limit": limit},
        )))
        for value in result.data:
            for key, minimum, maximum in (("max_charge_microusd", 0, 1_000_000_000_000),
                    ("max_input_tokens", 1, 1_000_000_000), ("max_input_bytes", 1, 1_048_576),
                    ("max_output_tokens", 1, 1_000_000), ("max_output_bytes", 1, 262144),
                    ("timeout_milliseconds", 1, 600000)):
                number = value.get(key)
                if isinstance(number, bool) or not isinstance(number, int) or not minimum <= number <= maximum:
                    raise ProtocolError("Invalid model accounting mode or resource bounds")
            mode, charge = value.get("accounting_mode"), value["max_charge_microusd"]
            if (mode == "operator_unmetered_local" and charge != 0) or (mode != "operator_unmetered_local" and (mode != "metered" or charge == 0)):
                raise ProtocolError("Invalid model accounting mode or resource bounds")
        return result

    def create_agent(
        self,
        name: str,
        instructions: str,
        *,
        idempotency_key: str,
        model: str | None = None,
        tools: list[str] | None = None,
        budget_microusd: int | None = None,
        team: TeamDefinition | None = None,
    ) -> Agent:
        """Create a draft; publish its expected version before creating tasks.

        ``fixture:echo`` is a deterministic development fixture, not a real LLM.
        Select a configured model explicitly for real inference.
        """
        definition: dict[str, Any] = {
            "name": _nonempty(name, "name"),
            "instructions": _nonempty(instructions, "instructions"),
        }
        if model is not None:
            definition["model"] = _nonempty(model, "model")
        if tools is not None:
            if not isinstance(tools, list) or any(not isinstance(tool, str) for tool in tools):
                raise ValueError("tools must be a list of strings")
            definition["tools"] = tools
        if budget_microusd is not None:
            definition["budget_microusd"] = _integer(
                budget_microusd, "budget_microusd", minimum=1, maximum=1_000_000_000_000,
            )
        if team is not None:
            definition["team"] = _team(team)
        return cast(Agent, self._object(self._request(
            "POST", "/agents", body=definition,
            idempotency_key=idempotency_key,
        )))

    def publish_agent(
        self, agent_id: str, *, expected_version: int, idempotency_key: str,
    ) -> Agent:
        """Publish exactly the inspected draft version; stale versions conflict."""
        _integer(expected_version, "expected_version", minimum=1)
        return cast(Agent, self._object(self._request(
            "POST", f"/agents/{_identifier(agent_id, 'agent_id')}/publish",
            body={"expected_version": expected_version}, idempotency_key=idempotency_key,
        )))

    def events(self, task_id: str, *, after: str | None = None, limit: int | None = None) -> Page[Event]:
        """Read stored events. Decimal sequence strings preserve full bigint precision."""
        if after is not None:
            try:
                event_contract.sequence(after, True)
            except ProtocolError as exc:
                raise ValueError("after must be a canonical int64 decimal string") from exc
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        result = self._page(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/events", query={"after": after, "limit": limit}))
        for value in result.data:
            event_contract.event(value, self.organization_id, task_id)
        return cast(Page[Event], result)

    def get_task_event_snapshot(self, task_id: str) -> TaskEventSnapshot:
        value = self._object(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/event-snapshot"))
        return cast(TaskEventSnapshot, event_contract.snapshot(value, self.organization_id, task_id))

    def task_event_feed(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> TaskEventBatch:
        if cursor is not None:
            event_contract.cursor(cursor, True)
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        value = self._object(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/event-feed", query={"cursor": cursor, "limit": limit}))
        return cast(TaskEventBatch, event_contract.batch(value, self.organization_id, task_id))

    @contextmanager
    def stream_task_events(self, task_id: str, *, cursor: str | None = None) -> Iterator[Iterator[TaskEventStreamFrame]]:
        """One connection, no hidden reconnect; use ``with`` to close it on early exit.

        ``timeout`` bounds connect and idle reads, not the complete stream lifetime.
        Persist a batch cursor only after processing every event in that batch.
        """
        path = f"/tasks/{_identifier(task_id, 'task_id')}/event-stream"
        if cursor is not None:
            path += "?" + urlencode({"cursor": event_contract.cursor(cursor, True)})
        request = Request(self.base_url + path, headers={"Accept": "text/event-stream", "Authorization": "Bearer " + self._token, "X-Organization-ID": self.organization_id, event_contract.API_VERSION_HEADER: event_contract.API_VERSION}, method="GET")
        response = None
        try:
            try:
                response = self._opener.open(request, timeout=self.timeout)
            except HTTPError as exc:
                with exc:
                    event_contract.check_version(exc.headers)
                    try:
                        details = self._decode(exc.read(16 * 1024 * 1024 + 1)).get("error", {})
                    except ProtocolError:
                        details = {}
                    if not isinstance(details, dict):
                        details = {}
                    raise APIError(status_code=exc.code, code=details.get("code", "http_error"), message=details.get("message", "Event subscription rejected"), retryable=details.get("retryable") is True) from None
            event_contract.check_version(response.headers)
            if response.headers.get_content_type() != "text/event-stream":
                raise ProtocolError("Expected a successful SSE response")
            parser = event_contract.EventStreamParser(self.organization_id, task_id)
            def frames() -> Iterator[TaskEventStreamFrame]:
                try:
                    while True:
                        chunk = response.read1(4096)
                        if not chunk:
                            parser.finish()
                            return
                        for frame in parser.push(chunk):
                            yield cast(TaskEventStreamFrame, frame)
                            if frame["kind"] != "batch":
                                return
                except (URLError, TimeoutError, OSError) as exc:
                    raise TransportError("Event connection interrupted; retain the last processed cursor") from exc
            yield frames()
        except (URLError, TimeoutError, OSError) as exc:
            raise TransportError("Unable to connect to the event stream") from exc
        finally:
            if response is not None:
                response.close()

    def artifacts(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[Artifact]:
        return cast(Page[Artifact], self._execution_page(task_id, "artifacts", cursor, limit))

    def get_artifact(self, task_id: str, artifact_id: str, *, version: int | None = None) -> Artifact:
        if version is not None:
            _integer(version, "version", minimum=1)
        return cast(Artifact, self._object(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/artifacts/{_identifier(artifact_id, 'artifact_id')}", query={"version": version})))

    def materials(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[Material]:
        return cast(Page[Material], self._execution_page(task_id, "materials", cursor, limit))

    def get_material(self, task_id: str, material_id: str) -> Material:
        return cast(Material, self._object(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/materials/{_identifier(material_id, 'material_id')}")))

    def material_revisions(self, task_id: str, material_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[MaterialRevision]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        return cast(Page[MaterialRevision], self._page(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/materials/{_identifier(material_id, 'material_id')}/revisions", query={"cursor": cursor, "limit": limit})))

    def get_material_version(self, task_id: str, material_id: str, version: int) -> MaterialContent:
        _integer(version, "version", minimum=1, maximum=128)
        return cast(MaterialContent, self._object(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/materials/{_identifier(material_id, 'material_id')}/revisions/{version}")))

    def create_material(self, task_id: str, name: str, media_type: str, content: str, *, idempotency_key: str, base_candidate_id: str | None = None, reason: str | None = None) -> Material:
        """Save a draft; empty content is valid and no execution is launched."""
        if media_type not in ("text/plain", "text/markdown", "application/json"):
            raise ValueError("unsupported media_type")
        body = {"name": _text(_nonempty(name, "name"), "name", 120), "media_type": media_type, "content": _text(content, "content", 262144)}
        if base_candidate_id is not None:
            body["base_candidate_id"] = _text(_nonempty(base_candidate_id, "base_candidate_id"), "base_candidate_id", 128)
        if reason is not None:
            body["reason"] = _text(reason, "reason", 4096)
        return cast(Material, self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/materials", body=body, idempotency_key=idempotency_key)))

    def revise_material(self, task_id: str, material_id: str, content: str, *, expected_version: int, reason: str, idempotency_key: str, merge_base_version: int | None = None) -> Material:
        """Append a version; a 409 does not retry or discard caller text."""
        _integer(expected_version, "expected_version", minimum=1, maximum=128)
        body = {"expected_version": expected_version, "content": _text(content, "content", 262144), "reason": _text(_nonempty(reason, "reason"), "reason", 4096)}
        if merge_base_version is not None:
            body["merge_base_version"] = _integer(merge_base_version, "merge_base_version", minimum=1, maximum=expected_version)
        return cast(Material, self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/materials/{_identifier(material_id, 'material_id')}/revisions", body=body, idempotency_key=idempotency_key)))

    def publish_material(self, task_id: str, material_id: str, *, expected_task_version: int, expected_material_version: int, content_digest: str, parent_candidate_id: str, idempotency_key: str) -> Task:
        """Propose an exact saved revision; this does not accept it or launch work."""
        body = {"expected_task_version": _integer(expected_task_version, "expected_task_version", minimum=1), "expected_material_version": _integer(expected_material_version, "expected_material_version", minimum=1, maximum=128), "content_digest": _digest(content_digest), "parent_candidate_id": _text(_nonempty(parent_candidate_id, "parent_candidate_id"), "parent_candidate_id", 128)}
        return _task_facts(self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/materials/{_identifier(material_id, 'material_id')}/publish", body=body, idempotency_key=idempotency_key)))

    def runs(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[Run]:
        return cast(Page[Run], self._execution_page(task_id, "runs", cursor, limit))

    def operations(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[Operation]:
        return cast(Page[Operation], self._execution_page(task_id, "operations", cursor, limit))

    def tools(self, *, cursor: str | None = None, limit: int | None = None) -> Page[ToolInfo]:
        if limit is not None: _integer(limit, "limit", minimum=1, maximum=100)
        result = self._page(self._request("GET", "/tools", query={"cursor": cursor, "limit": limit}))
        for value in result.data: _tool_money(value)
        return cast(Page[ToolInfo], result)

    def tool_executions(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[ToolExecution]:
        result = self._execution_page(task_id, "tool-executions", cursor=cursor, limit=limit)
        for value in result.data: _tool_money(value)
        return cast(Page[ToolExecution], result)

    def get_tool_execution(self, task_id: str, execution_id: str) -> ToolExecutionView:
        return _tool_view(self._object(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/tool-executions/{_identifier(execution_id, 'execution_id')}")))

    def prepare_tool_execution(self, task_id: str, *, occurrence_id: str, expected_task_version: int,
                               candidate_id: str, artifact_id: str, artifact_version: int, content_digest: str,
                               tool_id: str, tool_version: int, arguments_json: str, expires_in_seconds: int,
                               idempotency_key: str, model_proposal_id: str | None = None) -> ToolExecutionView:
        body = {"occurrence_id": _occurrence(occurrence_id), "expected_task_version": _integer(expected_task_version,"expected_task_version",minimum=1),
                "candidate_id": _text(_nonempty(candidate_id,"candidate_id"),"candidate_id",128), "artifact_id": _text(_nonempty(artifact_id,"artifact_id"),"artifact_id",128),
                "artifact_version": _integer(artifact_version,"artifact_version",minimum=1), "content_digest": _digest(content_digest),
                "tool_id": _text(_nonempty(tool_id,"tool_id"),"tool_id",128), "tool_version": _integer(tool_version,"tool_version",minimum=1),
                "arguments_json": _tool_arguments(arguments_json), "expires_in_seconds": _integer(expires_in_seconds,"expires_in_seconds",minimum=1,maximum=86400)}
        if model_proposal_id is not None:
            body["model_proposal_id"] = _text(_nonempty(model_proposal_id,"model_proposal_id"),"model_proposal_id",128)
        return _tool_view(self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/tool-executions", body=body,idempotency_key=idempotency_key)))

    def model_proposals(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[ModelToolProposal]:
        return cast(Page[ModelToolProposal], self._execution_page(task_id, "model-tool-proposals", cursor=cursor, limit=limit))

    def get_model_proposal(self, task_id: str, proposal_id: str) -> ModelToolProposal:
        return cast(ModelToolProposal, self._object(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/model-tool-proposals/{_identifier(proposal_id, 'proposal_id')}")))

    def continue_model(self, task_id: str, *, expected_task_version: int, candidate_id: str, artifact_id: str,
                       artifact_version: int, content_digest: str, results: list[ModelToolResultBinding],
                       acknowledge_cost: bool, reason: str, idempotency_key: str) -> Task:
        """Explicitly admit one new charged model turn; never dispatch tools or retry."""
        if acknowledge_cost is not True: raise ValueError("acknowledge_cost must be explicitly true")
        body = {"expected_task_version": _integer(expected_task_version,"expected_task_version",minimum=1),
                "candidate_id": _text(_nonempty(candidate_id,"candidate_id"),"candidate_id",128),
                "artifact_id": _text(_nonempty(artifact_id,"artifact_id"),"artifact_id",128),
                "artifact_version": _integer(artifact_version,"artifact_version",minimum=1), "content_digest": _digest(content_digest),
                "results": _model_results(results), "acknowledge_cost": True, "reason": _text(_nonempty(reason,"reason"),"reason",4096)}
        return _task_facts(self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/continue-model",body=body,idempotency_key=idempotency_key)))

    def automation(self, task_id: str) -> TaskAutomationState:
        return _automation_state(self._object(self._request("GET", f"/tasks/{_identifier(task_id, 'task_id')}/automation")))

    def create_automation(self, task_id: str, *, expected_task_version: int, mode: str, tool_pins: list[AutomationToolPin],
                          max_model_turns: int, max_tool_executions: int, max_charge_microusd: str, expires_in_seconds: int,
                          acknowledge_cost: bool, reason: str, idempotency_key: str) -> TaskAutomationState:
        """Grant finite task-lifetime automation ceilings; final acceptance remains explicit."""
        if acknowledge_cost is not True:
            raise ValueError("acknowledge_cost must be explicitly true")
        if mode not in ("read_only", "prepare_and_continue"):
            raise ValueError("unsupported automation mode")
        body = {"expected_task_version": _integer(expected_task_version, "expected_task_version", minimum=1), "mode": mode,
                "tool_pins": _automation_pins(tool_pins), "max_model_turns": _integer(max_model_turns, "max_model_turns", minimum=1, maximum=31),
                "max_tool_executions": _integer(max_tool_executions, "max_tool_executions", minimum=1, maximum=32),
                "max_charge_microusd": _automation_charge(max_charge_microusd),
                "expires_in_seconds": _integer(expires_in_seconds, "expires_in_seconds", minimum=1, maximum=86400),
                "acknowledge_cost": True, "reason": _text(_nonempty(reason, "reason"), "reason", 4096)}
        return _automation_state(self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/automation", body=body, idempotency_key=idempotency_key)))

    def revoke_automation(self, task_id: str, *, grant_id: str, expected_version: int, reason: str, idempotency_key: str) -> TaskAutomationState:
        body = {"grant_id": _text(_nonempty(grant_id, "grant_id"), "grant_id", 128),
                "expected_version": _integer(expected_version, "expected_version", minimum=1),
                "reason": _text(_nonempty(reason, "reason"), "reason", 4096)}
        return _automation_state(self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/automation/revoke", body=body, idempotency_key=idempotency_key)))

    def progress_signals(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[ProgressSignal]:
        return cast(Page[ProgressSignal], self._execution_page(task_id, "progress-signals", cursor=cursor, limit=limit))

    def decide_tool_execution(self, task_id: str, execution_id: str, *, expected_task_version: int,
                              expected_execution_version: int, expected_approval_version: int, intent_hash: str,
                              decision: str, reason: str, idempotency_key: str) -> ToolExecutionView:
        if decision not in ("approve", "deny"): raise ValueError("decision must be approve or deny")
        body = {"expected_task_version": _integer(expected_task_version,"expected_task_version",minimum=1),
                "expected_execution_version": _integer(expected_execution_version,"expected_execution_version",minimum=1),
                "expected_approval_version": _integer(expected_approval_version,"expected_approval_version",minimum=1),
                "intent_hash": _digest(intent_hash), "decision": decision, "reason": _text(_nonempty(reason,"reason"),"reason",4096)}
        return _tool_view(self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/tool-executions/{_identifier(execution_id, 'execution_id')}/decide",body=body,idempotency_key=idempotency_key)))

    def dispatch_tool_execution(self, task_id: str, execution_id: str, *, expected_task_version: int,
                                expected_execution_version: int, expected_approval_version: int, intent_hash: str,
                                acknowledge_cost: bool, idempotency_key: str) -> ToolExecutionView:
        if acknowledge_cost is not True: raise ValueError("acknowledge_cost must be explicitly true")
        body = {"expected_task_version": _integer(expected_task_version,"expected_task_version",minimum=1),
                "expected_execution_version": _integer(expected_execution_version,"expected_execution_version",minimum=1),
                "expected_approval_version": _integer(expected_approval_version,"expected_approval_version",minimum=1), "intent_hash": _digest(intent_hash), "acknowledge_cost": True}
        return _tool_view(self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/tool-executions/{_identifier(execution_id, 'execution_id')}/dispatch",body=body,idempotency_key=idempotency_key)))

    def reconcile_tool_execution(self, task_id: str, execution_id: str, *, expected_task_version: int,
                                 expected_execution_version: int, intent_hash: str, outcome: str, effect_reference: str,
                                 result: str, evidence_digest: str, reason: str, idempotency_key: str) -> ToolExecutionView:
        if outcome not in ("succeeded", "rejected"): raise ValueError("outcome must be succeeded or rejected")
        body = {"expected_task_version": _integer(expected_task_version,"expected_task_version",minimum=1),
                "expected_execution_version": _integer(expected_execution_version,"expected_execution_version",minimum=1), "intent_hash": _digest(intent_hash),
                "outcome": outcome, "effect_reference": _text(_nonempty(effect_reference,"effect_reference"),"effect_reference",256),
                "result": _text(result,"result",262144), "evidence_digest": _digest(evidence_digest), "reason": _text(_nonempty(reason,"reason"),"reason",4096)}
        return _tool_view(self._object(self._request("POST", f"/tasks/{_identifier(task_id, 'task_id')}/tool-executions/{_identifier(execution_id, 'execution_id')}/reconcile",body=body,idempotency_key=idempotency_key)))

    def actions(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[Action]:
        return cast(Page[Action], self._execution_page(task_id, "actions", cursor, limit))

    def approvals(self, task_id: str, *, cursor: str | None = None, limit: int | None = None) -> Page[Approval]:
        return cast(Page[Approval], self._execution_page(task_id, "approvals", cursor, limit))

    def costs(
        self, task_id: str, *, limit: int | None = None,
        reservation_cursor: str | None = None, entry_cursor: str | None = None,
        incident_cursor: str | None = None, quarantine_cursor: str | None = None,
    ) -> CostReport:
        """Read subtree accounting; each collection is independently paginated.

        Totals come from the server snapshot, not a sum of the current page.
        This SDK does not expose billing ingestion or adjustment mutations.
        """
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        report = self._object(self._request(
            "GET", f"/tasks/{_identifier(task_id, 'task_id')}/costs",
            query={"limit": limit, "reservation_cursor": reservation_cursor,
                   "entry_cursor": entry_cursor, "incident_cursor": incident_cursor,
                   "quarantine_cursor": quarantine_cursor},
        ))
        for key in ("reservations", "entries", "incidents", "quarantines"):
            if not isinstance(report.get(key), list) or any(not isinstance(item, dict) for item in report[key]):
                raise ProtocolError(f"Cost report {key} must be a list of objects")
        for key, maximum in (("budget_microusd", 1_000_000_000_000), ("pending_quarantine_count", 9_007_199_254_740_991)):
            value = report.get(key)
            if isinstance(value, bool) or not isinstance(value, int) or value < 0 or value > maximum:
                raise ProtocolError(f"Cost report {key} must be a bounded nonnegative integer")
        for key in ("reserved_microusd", "spent_microusd", "unknown_microusd"):
            _response_money(report.get(key))
        # Exact full-subtree bigint count, including zero-amount uncertainties.
        _response_money(report.get("unknown_reservation_count"))
        for item in report["reservations"]:
            if _response_money(item.get("amount_microusd")) > 1_000_000_000_000:
                raise ProtocolError("Reservation exceeds the admission limit")
            if "actual_microusd" in item:
                _response_money(item["actual_microusd"])
        for item in report["entries"]:
            _response_money(item.get("delta_reserved_microusd"), signed=True)
            _response_money(item.get("delta_spent_microusd"), signed=True)
        for item in report["incidents"]:
            _response_money(item.get("amount_microusd"))
        for item in report["quarantines"]:
            _response_money(item.get("actual_microusd"))
        for key in ("financial_hold", "incomplete"):
            if not isinstance(report.get(key), bool):
                raise ProtocolError(f"Cost report {key} must be a boolean")
        for key in ("next_reservation_cursor", "next_entry_cursor", "next_incident_cursor", "next_quarantine_cursor"):
            if key in report and not isinstance(report[key], str):
                raise ProtocolError(f"Cost report {key} must be a string when present")
        return cast(CostReport, report)

    def _execution_page(
        self, task_id: str, collection: str, cursor: str | None, limit: int | None,
    ) -> Page[dict[str, Any]]:
        if limit is not None:
            _integer(limit, "limit", minimum=1, maximum=100)
        return self._page(self._request(
            "GET", f"/tasks/{_identifier(task_id, 'task_id')}/{collection}",
            query={"cursor": cursor, "limit": limit},
        ))

    def _request(
        self,
        method: str,
        path: str,
        *,
        body: Mapping[str, Any] | None = None,
        query: Mapping[str, str | int | None] | None = None,
        idempotency_key: str | None = None,
    ) -> dict[str, Any]:
        url = self.base_url + path
        if query:
            filtered = {key: value for key, value in query.items() if value is not None}
            if filtered:
                url += "?" + urlencode(filtered)
        headers = {
            "Accept": "application/json",
            "Authorization": "Bearer " + self._token,
            "X-Organization-ID": self.organization_id,
            "User-Agent": "agent-platform-python/0.14.0",
            event_contract.API_VERSION_HEADER: event_contract.API_VERSION,
        }
        if method not in ("GET", "HEAD"):
            if idempotency_key is None:
                raise ValueError("idempotency_key is required for mutations")
            headers["Idempotency-Key"] = _header(idempotency_key, "idempotency_key")
            if len(idempotency_key) > 128:
                raise ValueError("idempotency_key cannot exceed 128 characters")
            if " " in idempotency_key:
                raise ValueError("idempotency_key cannot contain spaces")
        payload = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            payload = json.dumps(body, ensure_ascii=False, allow_nan=False).encode("utf-8")
        request = Request(url, data=payload, headers=headers, method=method)
        try:
            with self._opener.open(request, timeout=self.timeout) as response:
                event_contract.check_version(response.headers)
                document = self._decode(response.read(16 * 1024 * 1024 + 1))
        except HTTPError as exc:
            with exc:
                event_contract.check_version(exc.headers)
                raw = exc.read(16 * 1024 * 1024 + 1)
                try:
                    error_document = self._decode(raw)
                except ProtocolError:
                    error_document = {}
                details = error_document.get("error")
                if not isinstance(details, dict):
                    details = {}
                request_id = details.get("request_id") or exc.headers.get("X-Request-ID")
                raise APIError(
                    status_code=exc.code,
                    code=details.get("code") if isinstance(details.get("code"), str) else "http_error",
                    message=details.get("message") if isinstance(details.get("message"), str)
                    else "Server returned an HTTP error without a valid error envelope",
                    request_id=request_id if isinstance(request_id, str) else None,
                    retryable=details.get("retryable") is True,
                ) from None
        except (URLError, TimeoutError, OSError) as exc:
            raise TransportError(
                "Unable to complete the API request; the result of a mutation may be unknown"
            ) from exc
        if "data" not in document:
            raise ProtocolError("Response is missing the data envelope")
        return document

    @staticmethod
    def _decode(raw: bytes) -> dict[str, Any]:
        if len(raw) > 16 * 1024 * 1024:
            raise ProtocolError("JSON response exceeds the 16 MiB size limit")
        try:
            document = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ProtocolError("Response is not valid UTF-8 JSON") from exc
        if not isinstance(document, dict):
            raise ProtocolError("Response envelope must be a JSON object")
        return document

    @staticmethod
    def _object(envelope: dict[str, Any]) -> dict[str, Any]:
        data = envelope["data"]
        if not isinstance(data, dict):
            raise ProtocolError("Expected an object in the response data envelope")
        return data

    @staticmethod
    def _page(envelope: dict[str, Any]) -> Page[dict[str, Any]]:
        data = envelope["data"]
        if not isinstance(data, list) or any(not isinstance(item, dict) for item in data):
            raise ProtocolError("Expected a list of objects in the response data envelope")
        cursor = envelope.get("next_cursor")
        if cursor is not None and not isinstance(cursor, str):
            raise ProtocolError("next_cursor must be a string or null")
        return Page(data=data, next_cursor=cursor)

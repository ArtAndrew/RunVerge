# SPDX-License-Identifier: Apache-2.0
"""Strict bounded configuration wire validation; no host profile is public."""
import re
from datetime import datetime
from .errors import ProtocolError


def text(maximum, empty=False):
    def check(v):
        if not isinstance(v, str) or (not empty and not v.strip()) or "\0" in v:
            return False
        try:
            return len(v.encode("utf-8")) <= maximum
        except UnicodeError:
            return False
    return check


def integer(low=0, high=2147483647):
    return lambda v: type(v) is int and low <= v <= high


def one(*values):
    return lambda v: any(type(v) is type(x) and v == x for x in values)


def array(check, low=0, high=100):
    return lambda v: isinstance(v, list) and low <= len(v) <= high and all(check(x) for x in v)


def decimal(maximum="9223372036854775807"):
    return lambda v: isinstance(v, str) and len(v) <= len(maximum) and re.fullmatch(r"0|[1-9][0-9]*", v) is not None and (len(v) < len(maximum) or v <= maximum)


def date(v):
    if not isinstance(v, str) or len(v) > 64 or re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-]\d\d:\d\d)", v) is None:
        return False
    try:
        return datetime.fromisoformat(v.replace("Z", "+00:00")).tzinfo is not None
    except ValueError:
        return False


def shape(required, optional=None):
    optional = optional or {}
    def check(v):
        return isinstance(v, dict) and len(v) <= len(required) + len(optional) and all(k in v and c(v[k]) for k, c in required.items()) and all(k in required or (k in optional and optional[k](value)) for k, value in v.items())
    return check


identifier = text(128)
reason = text(4096)
boolean = lambda v: type(v) is bool
kind = one("model", "mcp")
audience = one("platform.configuration.v1")
actions = array(one("inspect", "plan", "validate", "apply", "verify", "delegate"), 0, 6)
commands = array(text(128), 0, 32)
money = decimal("1000000000000")
digest = lambda v: isinstance(v, str) and re.fullmatch(r"[a-f0-9]{64}", v) is not None
scope = shape(dict(destination_id=identifier, destination_version=integer(1), kind=kind, target_id=identifier, credential_purpose_id=identifier, credential_purpose_version=integer(1)))
diff = shape(dict(operation_id=identifier, kind=kind, target_id=identifier, expected_version=integer(), new_version=integer(1), active=boolean, retired_versions=array(integer(1), 0, 10000), may_stop_pinned_tasks=boolean))
operation = shape(dict(id=identifier, scope=scope, expected_resource_version=integer(), active=boolean))
destination = shape(dict(id=identifier, organization_id=identifier, version=integer(1), kind=kind, name=text(120), target_id=identifier, credential_purpose_id=identifier, credential_purpose_version=integer(1), requires_approval=boolean, max_verify_charge_microusd=money, discovery_unmetered_approved=boolean, active=boolean, created_at=date), dict(model=text(256)))
catalog = shape(dict(organization_id=identifier, revision=decimal(), audience=audience, actions=actions, can_issue_root_grants=boolean, can_approve=boolean, max_operations=integer(1, 8), max_grant_depth=integer(1, 4), available_commands=commands))
grant = shape(dict(id=identifier, organization_id=identifier, audience=audience, issuer_id=identifier, subject_id=identifier, depth=integer(0, 4), version=integer(1), actions=actions, scopes=array(scope, 1, 16), max_plans=integer(1, 100), used_plans=integer(0, 100), max_verifications=integer(1, 100), used_verifications=integer(0, 100), max_allocated_charge_microusd=money, allocated_charge_microusd=money, revoked=boolean, status=text(128), created_at=date, expires_at=date, available_commands=commands), dict(parent_grant_id=identifier))
plan = shape(dict(id=identifier, organization_id=identifier, actor_id=identifier, version=integer(1), status=text(128), base_revision=decimal(), digest=digest, operations=array(operation, 1, 8), diff=array(diff, 1, 8), reason=reason, created_at=date, expires_at=date, applied_revision=decimal(), available_commands=commands), dict(grant_id=identifier, applied_at=date, receipt_id=identifier))
decision = shape(dict(id=identifier, organization_id=identifier, action=text(128), target_id=identifier, outcome=text(128), reason_codes=array(text(128)), configuration_revision=decimal(), obligations=array(text(128)), evaluated_at=date), dict(grant_id=identifier))
validation = shape(dict(id=identifier, organization_id=identifier, plan_id=identifier, plan_version=integer(1), plan_digest=digest, base_revision=decimal(), current_revision=decimal(), valid=boolean, decision=decision, diff=array(diff, 1, 8), created_at=date))
approval = shape(dict(id=identifier, organization_id=identifier, plan_id=identifier, plan_digest=digest, validation_id=identifier, principal_id=identifier, reason=reason, created_at=date))
verification = shape(dict(id=identifier, organization_id=identifier, plan_id=identifier, operation_id=identifier, actor_id=identifier, scope=scope, resource_version=integer(1), kind=kind, status=text(128), version=integer(1), max_charge_microusd=money, cost_status=text(128), tool_count=integer(), created_at=date, updated_at=date), dict(grant_id=identifier, task_id=identifier, protocol_version=text(128), catalog_digest=digest, response_digest=digest, reason_code=text(128)))
resource = shape(dict(organization_id=identifier, kind=kind, id=identifier, version=integer(1), scope=scope, active=boolean, created_at=date))
audit = shape(dict(id=identifier, organization_id=identifier, actor_type=text(128), object_type=text(128), object_id=identifier, action=text(128), digest=text(64, True), reason=text(4096, True), created_at=date), dict(actor_id=identifier))
_CHECKS = dict(catalog=catalog, destination=destination, grant=grant, plan=plan, validation=validation, approval=approval, verification=verification, resource=resource, audit=audit)


def configuration_facts(record_type, value, org, target=None, plan_id=None):
    if record_type not in _CHECKS or not _CHECKS[record_type](value) or value["organization_id"] != org or (target is not None and value.get("id") != target) or (plan_id is not None and value.get("plan_id") != plan_id):
        raise ProtocolError("Invalid public configuration response")
    if record_type == "validation" and (value["decision"]["organization_id"] != org or value["decision"]["target_id"] != value["plan_id"]):
        raise ProtocolError("Invalid configuration decision scope")
    return value


_VERSION_DIGEST = dict(expected_version=integer(1), plan_digest=digest)
_OP_INPUT = shape(dict(id=identifier, destination_id=identifier, destination_version=integer(1), expected_resource_version=integer(), active=boolean))
_INPUTS = dict(
    createGrant=shape(dict(subject_id=identifier, actions=array(one("inspect", "plan", "validate", "apply", "verify", "delegate"), 1, 6), destinations=array(shape(dict(id=identifier, version=integer(1))), 1, 16), ttl_seconds=integer(1, 86400), max_plans=integer(1, 100), max_verifications=integer(1, 100), max_allocated_charge_microusd=money, reason=reason), dict(parent_grant_id=identifier, expected_parent_version=integer(1))),
    revokeGrant=shape(dict(expected_version=integer(1), reason=reason)),
    createPlan=shape(dict(base_revision=decimal(), ttl_seconds=integer(1, 3600), operations=array(_OP_INPUT, 1, 8), reason=reason), dict(grant_id=identifier)),
    validatePlan=shape(_VERSION_DIGEST),
    approvePlan=shape(dict(**_VERSION_DIGEST, validation_id=identifier, reason=reason)),
    applyPlan=shape(dict(**_VERSION_DIGEST, validation_id=identifier, reason=reason)),
    verify=shape(dict(expected_plan_version=integer(1), plan_digest=digest, operation_id=identifier, max_charge_microusd=money, acknowledge_cost=one(True), reason=reason)),
)


def configuration_input(record_type, value):
    if record_type not in _INPUTS or not _INPUTS[record_type](value):
        raise ValueError("Invalid configuration input")
    if record_type == "createGrant":
        if bool(value.get("parent_grant_id")) != bool(value.get("expected_parent_version")) or len(set(value["actions"])) != len(value["actions"]) or len({x["id"] for x in value["destinations"]}) != len(value["destinations"]):
            raise ValueError("Grant pins/actions must be distinct and parent version explicit")
    if record_type == "createPlan":
        ops = value["operations"]
        if len({x["id"] for x in ops}) != len(ops) or len({x["destination_id"] for x in ops}) != len(ops):
            raise ValueError("Plan operation IDs/destinations must be distinct")
    return value

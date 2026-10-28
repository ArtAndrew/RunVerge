# SPDX-License-Identifier: Apache-2.0
"""Bounded validation of public catalogue data; no authorization or command runner."""
from datetime import datetime
import re
from typing import cast
from .capability_types import ProductCapabilities
from .errors import ProtocolError
from .security import credential_free

_SURFACES = {"http", "cli", "python", "typescript", "mcp", "workspace"}
_ROLES = {(kind, role) for kind, roles in (("human", ("viewer", "member", "admin", "owner")), ("service", ("viewer", "member"))) for role in roles}
_EFFECTS = {"read_only", "platform_write", "queues_external_work", "external_dispatch", "records_observation"}


def _text(value, maximum=4096, empty=False):
    if not isinstance(value, str) or (not empty and not value.strip()) or "\0" in value or len(value.encode("utf-8")) > maximum:
        raise ValueError()
    return value


def _id(value):
    if re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.:-]*", _text(value, 128)) is None:
        raise ValueError()
    return value


def _list(value, minimum, maximum):
    if not isinstance(value, list) or not minimum <= len(value) <= maximum:
        raise ValueError()
    return value


def _strings(value, minimum=0, maximum=32, *, ids=False):
    result = _list(value, minimum, maximum)
    for item in result:
        (_id if ids else _text)(item)
    if ids and len(set(result)) != len(result):
        raise ValueError()
    return result


def _enum(value, values):
    if not isinstance(value, str) or value not in values:
        raise ValueError()


def _docs(value):
    for ref in _list(value, 1, 8):
        _text(ref, 256)
        if re.fullmatch(r"(?:docs|contracts)/[A-Za-z0-9_./-]+(?:#[A-Za-z0-9_.-]+)?", ref) is None or any(part in ("", ".", "..") for part in ref.split("#", 1)[0].split("/")):
            raise ValueError()


def _command(value):
    args = _list(value, 1, 20)
    if args[0] != "platform":
        raise ValueError()
    for arg in args:
        _text(arg, 512)
        if re.fullmatch(r"(?:[A-Za-z0-9_.,:{}=@+/-]+|<[A-Za-z0-9_.-]+>)", arg) is None or arg.startswith("/") or "://" in arg or ".." in arg.split("/"):
            raise ValueError()


def _unique(values, key="id"):
    seen = set()
    for value in values:
        if not isinstance(value, dict):
            raise ValueError()
        identifier = _id(value.get(key))
        if identifier in seen:
            raise ValueError()
        seen.add(identifier)
    return seen


def capability_facts(value, organization_id: str, principal_id: str | None = None) -> ProductCapabilities:
    """Validate an observation. Permissions and guards remain descriptive data."""
    try:
        if not credential_free(value, maximum_nodes=50000):
            raise ValueError()
        # Reject authority-shaped extensions rather than accidentally returning a permit.
        pending = [value]
        while pending:
            item = pending.pop()
            if isinstance(item, dict):
                if any(re.sub(r"[^a-z0-9]", "", key.lower()) in ("canexecute", "executionpermit", "authorized") for key in item):
                    raise ValueError()
                pending.extend(item.values())
            elif isinstance(item, list):
                pending.extend(item)
        if value.get("catalog_version") != "1" or value.get("api_contract") != "0.14.0" or value.get("assessment") != "support_and_role_observation" or value.get("read_only") is not True:
            raise ValueError()
        observed = _text(value.get("observed_at"), 128)
        if re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z", observed) is None:
            raise ValueError()
        datetime.fromisoformat(observed.replace("Z", "+00:00"))
        identity = value["identity"]
        if not isinstance(identity, dict) or identity.get("organization_id") != organization_id or (principal_id is not None and identity.get("principal_id") != principal_id):
            raise ValueError()
        _id(identity.get("principal_id")); _text(identity.get("organization_id"), 128)
        identity_pair = (identity.get("principal_kind"), identity.get("role"))
        if identity_pair not in _ROLES:
            raise ValueError()
        permissions = _strings(identity.get("permissions"), maximum=64, ids=True)
        _strings(value["authorization_rules"], 1, 32)
        operations = _list(value["operations"], 1, 128)
        operation_ids = _unique(operations)
        routes = set()
        for operation in operations:
            _id(operation["capability_id"])
            _enum(operation["method"], {"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"})
            path = _text(operation["path"], 512)
            if re.fullmatch(r"/api/v1(?:/(?:[A-Za-z0-9_-]+|\{[A-Za-z][A-Za-z0-9_]*\}))+", path) is None:
                raise ValueError()
            route = (operation["method"], re.sub(r"\{[^}]+\}", "{}", path))
            if route in routes:
                raise ValueError()
            routes.add(route)
            _text(operation["input_schema"], 128, True); _text(operation["output_schema"], 128)
            _enum(operation["response_kind"], {"object", "list", "sse"})
            if type(operation["success_status"]) is not int or not 200 <= operation["success_status"] <= 299:
                raise ValueError()
            _enum(operation["access_model"], {"public", "current_membership", "role_and_resource", "scoped_configuration", "human_owner"})
            alternatives = _list(operation["permission_alternatives"], 0, 4)
            for group in alternatives:
                _strings(group, 1, 4, ids=True)
            if len({tuple(sorted(group)) for group in alternatives}) != len(alternatives):
                raise ValueError()
            _strings(operation["runtime_guards"], 1, 32)
            _enum(operation["effect"], _EFFECTS)
            _enum(operation["cost_semantics"], {"none", "may_reserve_and_queue", "reserves_upper_bound", "possible_charge_existing_reservation", "observation_no_settlement"})
            _enum(operation["retry_policy"], {"read_with_current_authorization", "explicit_same_key_after_inspection", "scoped_stream_resume"})
            bindings = _list(operation["bindings"], 6, 6)
            if _unique(bindings, "surface") != _SURFACES:
                raise ValueError()
            for binding in bindings:
                _enum(binding["status"], {"available", "operator_opt_in", "not_exposed"})
                _text(binding["name"], 512, binding["status"] == "not_exposed")
                if binding["status"] == "not_exposed" and binding["name"] != "":
                    raise ValueError()
            _docs(operation["documentation"])
        profiles = _list(value["profiles"], 1, 32)
        _unique(profiles)
        for profile in profiles:
            _text(profile["name"]); _text(profile["summary"])
            _enum(profile["status"], {"implemented", "bounded", "not_implemented"})
            _strings(profile["constraints"], 1, 32); _docs(profile["documentation"])
        roles = _list(value["roles"], 6, 6)
        pairs = set()
        for role in roles:
            pair = (role["principal_kind"], role["role"])
            if pair not in _ROLES or pair in pairs:
                raise ValueError()
            pairs.add(pair)
            _strings(role["permissions"], maximum=64, ids=True)
            if pair == identity_pair and not set(permissions).issubset(role["permissions"]):
                raise ValueError()
        onboarding = _list(value["onboarding"], 6, 6)
        _unique(onboarding)
        for guide in onboarding:
            _text(guide["persona"]); _text(guide["summary"])
            _enum(guide["status"], {"available", "partial", "not_implemented"})
            _strings(guide["prerequisites"], 1, 32); _docs(guide["documentation"])
            steps = _list(guide["steps"], 0, 12)
            _unique(steps)
            for step in steps:
                _text(step["instruction"]); _text(step["actor"])
                if not set(_strings(step["operation_ids"], maximum=16, ids=True)).issubset(operation_ids):
                    raise ValueError()
                _enum(step["effect"], _EFFECTS)
                if type(step["cost_acknowledgement"]) is not bool:
                    raise ValueError()
                for command in _list(step["commands"], 0, 4):
                    _command(command)
        _command(value["remediation_command"])
        return cast(ProductCapabilities, value)
    except (KeyError, TypeError, ValueError, OverflowError, UnicodeError, RecursionError):
        raise ProtocolError("Invalid product capabilities response") from None

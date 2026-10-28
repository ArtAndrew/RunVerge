# SPDX-License-Identifier: Apache-2.0
"""Bounded public security metadata validation. Credential issuance is host-only."""
from datetime import datetime
from typing import cast
from .errors import ProtocolError
from .types import OrganizationLoginPolicy, ServiceAccount, ServiceKey, OrganizationSecurityAuditEntry

_SECRET_FIELDS = {"token", "rawtoken", "accesstoken", "refreshtoken", "idtoken", "sessiontoken", "bearertoken", "secret", "clientsecret", "password", "privatekey", "apikey", "authorization", "credential", "credentials", "tokenhash", "tokenprefix", "secretreference", "credentialhash", "tokendigest"}
def _no_secret(value, maximum_nodes=10000):
    pending=[(value,0)]; nodes=0
    while pending:
        value,depth=pending.pop(); nodes+=1
        if nodes>maximum_nodes or depth>32: return False
        if not isinstance(value,(dict,list)): continue
        if nodes+len(pending)+len(value)>maximum_nodes: return False
        entries=value.items() if isinstance(value,dict) else enumerate(value)
        for key,child in entries:
            normalized="".join(c for c in str(key).lower() if c.isascii() and c.isalnum())
            if normalized in _SECRET_FIELDS: return False
            pending.append((child,depth+1))
    return True
def credential_free(value, *, maximum_nodes=10000):
    return isinstance(value,dict) and _no_secret(value, maximum_nodes)

def _text(value, maximum, empty=False):
    if not isinstance(value, str) or (not empty and not value.strip()) or "\0" in value: raise ValueError("invalid security text")
    if len(value.encode("utf-8")) > maximum: raise ValueError("security text too long")
    return value

def _int(value, maximum=9007199254740991):
    if type(value) is not int or not 1 <= value <= maximum: raise ValueError("invalid security integer")
    return value

def _role(value):
    if value not in ("member", "viewer"): raise ValueError("invalid service role")
    return value

def _mode(value):
    if value not in ("mixed", "oidc_required"): raise ValueError("invalid login mode")
    return value

def _bool(value):
    if type(value) is not bool: raise ValueError("explicit boolean required")
    return value

def _date(value):
    _text(value, 128)
    if datetime.fromisoformat(value.replace("Z", "+00:00")).tzinfo is None: raise ValueError("timestamp timezone required")

def _strings(value):
    if not isinstance(value, list) or any(not isinstance(v, str) for v in value): raise ValueError("invalid command list")

def _base(value, org):
    if not isinstance(value, dict) or value.get("organization_id") != org or not _no_secret(value): raise ValueError("invalid security scope")
    _int(value.get("version"))

def login_policy_facts(value, org) -> OrganizationLoginPolicy:
    try:
        _base(value, org); _mode(value.get("mode")); _text(value.get("oidc_issuer"), 2048, True)
        _bool(value.get("service_access_enabled")); _bool(value.get("can_require_oidc")); _int(value.get("max_service_key_ttl_seconds"), 86400)
        for f in ("created_at", "updated_at"): _date(value.get(f))
        for f in ("available_commands", "readiness_reasons"): _strings(value.get(f))
        if "configured_oidc_issuer" in value: _text(value["configured_oidc_issuer"], 2048)
    except (TypeError, ValueError, KeyError, OverflowError): raise ProtocolError("Invalid login policy response") from None
    return cast(OrganizationLoginPolicy, value)

def service_account_facts(value, org, pid=None) -> ServiceAccount:
    try:
        _base(value, org)
        if value.get("principal_kind") != "service" or (pid is not None and value.get("principal_id") != pid): raise ValueError("invalid service identity")
        _text(value.get("principal_id"), 128); _text(value.get("display_name"), 120); _role(value.get("role")); _bool(value.get("active")); _bool(value.get("principal_active"))
        for f in ("created_at", "updated_at"): _date(value.get(f))
        if "credentials_revoked_at" in value: _date(value["credentials_revoked_at"])
        _strings(value.get("available_commands")); _strings(value.get("assignable_roles"))
        for role in value["assignable_roles"]: _role(role)
    except (TypeError, ValueError, KeyError, OverflowError): raise ProtocolError("Invalid service account response") from None
    return cast(ServiceAccount, value)

def service_key_facts(value, org, pid, key=None) -> ServiceKey:
    try:
        _base(value, org)
        if value.get("principal_id") != pid or (key is not None and value.get("id") != key): raise ValueError("invalid key identity")
        _text(value.get("id"), 128); _role(value.get("role_ceiling")); _bool(value.get("revoked")); _text(value.get("status"), 128); _strings(value.get("available_commands"))
        for f in ("created_at", "expires_at"): _date(value.get(f))
    except (TypeError, ValueError, KeyError, OverflowError): raise ProtocolError("Invalid service key response") from None
    return cast(ServiceKey, value)

def _audit_state(value):
    if not isinstance(value, dict) or not _no_secret(value): raise ValueError("invalid audit state")
    _int(value.get("version"))
    for f in ("active", "revoked", "service_access_enabled"):
        if f in value: _bool(value[f])
    for f in ("role", "role_ceiling"):
        if f in value: _role(value[f])
    if "mode" in value: _mode(value["mode"])
    if "oidc_issuer" in value: _text(value["oidc_issuer"], 2048, True)
    if "display_name" in value: _text(value["display_name"], 120)
    if "expires_at" in value: _date(value["expires_at"])
    if "max_service_key_ttl_seconds" in value: _int(value["max_service_key_ttl_seconds"], 86400)

def security_audit_facts(value, org, pid=None) -> OrganizationSecurityAuditEntry:
    try:
        if not isinstance(value, dict) or value.get("organization_id") != org or not _no_secret(value) or (pid is not None and value.get("target_principal_id") != pid): raise ValueError("invalid audit scope")
        for f in ("id", "actor_type", "object_type", "object_id", "action"): _text(value.get(f), 128)
        for f in ("actor_id", "target_principal_id"):
            if f in value: _text(value[f], 128)
        _text(value.get("reason"), 4096); _date(value.get("created_at")); _audit_state(value.get("after"))
        if "before" in value: _audit_state(value["before"])
    except (TypeError, ValueError, KeyError, OverflowError): raise ProtocolError("Invalid security audit response") from None
    return cast(OrganizationSecurityAuditEntry, value)

def policy_input(version, mode, issuer, enabled, ttl, reason):
    return dict(expected_version=_int(version), mode=_mode(mode), oidc_issuer=_text(issuer, 2048, True), service_access_enabled=_bool(enabled), max_service_key_ttl_seconds=_int(ttl, 86400), reason=_text(reason, 4096))

def service_input(name, role, reason, version=None, active=None, update=False):
    value=dict(display_name=_text(name, 120), role=_role(role), reason=_text(reason, 4096))
    if update: value.update(expected_version=_int(version), active=_bool(active))
    return value

def key_revoke_input(version, reason):
    return dict(expected_version=_int(version), reason=_text(reason, 4096))

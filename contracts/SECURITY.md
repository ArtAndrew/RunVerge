# Organization security contract 0.10

This revision adds organization login policy and organization-bound service
identities to the existing local platform. It does not finish the complete W03
corporate identity lifecycle, fine-grained authorization or provider deployment.

## Current authority

`GET /me` optionally adds `principal_kind: human|service`. Fresh reads always
include it; historical cached responses may omit it. `permissions` is the
effective profile, not a promise that a later request will be authorized.
Unknown principal kinds fail closed in the server. The organization header
selects an authorized membership; it never establishes identity.

Human identities retain existing roles. Service identities have an immutable
home organization and kind, and only `member` or `viewer` memberships. Each key
has an immutable `role_ceiling: member|viewer`; every authentication and repeated
authorization intersects that ceiling with the current membership role. A viewer
key cannot admit writes even when its account is promoted. Service profiles
exclude human result review, administrative operations, approvals,
reconciliation and issuing automation grants. Agents may prepare and dispatch
their own actions only through the existing exact approval and cost guards.
This revision has bounded role profiles, not arbitrary per-key scopes.

Services cannot acquire human bearer tokens, browser sessions or OIDC mappings,
change identity kind or gain memberships in another organization. These rules
also have database guards. The generic member directory marks service accounts;
its human update and credential-revocation commands reject service targets.

## Login policy

`GET /login-policy` returns `OrganizationLoginPolicy`. It includes organization,
mode, exact issuer, service access flag, maximum lifetime for newly issued service
keys, CAS version, timestamps and current caller capabilities. Management reads
require a human with `identity.manage`. Only a current human owner may update
policy. `available_commands` contains `update` when permitted; it does not
authorize a particular requested transition by itself.

`POST /login-policy/update` requires all fields of
`UpdateOrganizationLoginPolicy`: `expected_version`, `mode`, `oidc_issuer`,
explicit `service_access_enabled`, `max_service_key_ttl_seconds` and `reason`.
Modes are `mixed` and `oidc_required`. Mixed requires an empty issuer. Required
OIDC accepts only the exact issuer of the server's successfully initialized
browser login service. A supplied URL, database mapping or client readiness
claim is never sufficient. Browser authentication, current active exact issuer
mapping and current owner membership are rechecked after lock waits.

Changing mode or issuer requires that trusted browser session. A bearer owner in
mixed mode can change service access and the new-key lifetime without changing
mode or issuer. An admin cannot change policy. `can_require_oidc` describes
current readiness to change the SSO mode; `configured_oidc_issuer` is trusted
runtime configuration. `readiness_reasons` uses `oidc_not_configured`,
`browser_owner_required` and `issuer_mismatch`. Missing server login configuration
never causes an enforced organization to fall back to bearer authentication.

Lifetime is an integer from 1 to 86400 seconds; the default maximum is 3600.
Changing the maximum affects **new issuance only**. Existing expiry is immutable.
Service access defaults to enabled, with independent authorization from human
login mode. Service identities remain denied when service access is disabled.

## Service accounts and keys

Implemented HTTP routes, all under `/api/v1`:

| Method | Path | Result |
|---|---|---|
| GET | `/service-accounts` | Paginated service account metadata |
| POST | `/service-accounts` | New service account, no credential |
| GET | `/service-accounts/{principal_id}` | Exact account and capabilities |
| POST | `/service-accounts/{principal_id}/update` | Full account CAS update |
| GET | `/service-accounts/{principal_id}/keys` | Paginated safe key metadata |
| GET | `/service-accounts/{principal_id}/keys/{key_id}` | Exact safe key metadata |
| POST | `/service-accounts/{principal_id}/keys/{key_id}/revoke` | Exact key CAS revocation |
| GET | `/security-audit` | Paginated append-only safe audit |

Management requires a current human with `identity.manage`. Creation accepts
`display_name`, `role` and `reason`. Update additionally requires exact
`expected_version` and explicit `active`. Names are nonblank UTF-8 up to120 bytes;
reasons are nonblank UTF-8 up to4096 bytes. Unknown fields and null are rejected.
All mutation fields are required. An unchanged update with a new key returns409.
Every POST requires durable `Idempotency-Key`; current authority and resource
scope are checked before replay. Lists use default50/max100 and opaque cursors;
audit additionally accepts optional exact `principal_id`. Unknown, repeated,
empty or malformed query fields are rejected. Cross-organization objects are404.

The trusted host issues credentials with:

```sh
python3 scripts/dev.py issue-service-token \
  --organization ORG_ID --principal SERVICE_ID --role-ceiling viewer \
  --ttl 1h --reason "Read-only integration" --out /PRIVATE/new-service.json
```

The selected organization must be the immutable home organization. A key uses
256 random bits and mandatory bounded expiry. The database stores only its
hash. Raw material is written only to a new private `0600` file, never public
HTTP/MCP, stdout, key metadata, audit or UI. The file must not already exist.
If saving fails after issuance, the command attempts an exact hash revocation
with an independent cleanup context and reports any unconfirmed cleanup.
The resulting private file can be passed directly to `mcp-stdio --credentials
PATH`. Optional issuance metadata never establishes identity or permissions;
current token and organization authorization remain authoritative. There is no
HTTP issuance route and no SDK method pretending to issue a key.
Trusted-host commands intentionally have database authority and must be deployed
only with the organization's trusted operator controls.

Key metadata includes id, organization, principal, immutable ceiling, expiry,
version, revoked flag, status and capabilities. Status is `active`, `expired`,
`revoked` or `unavailable`; it is recomputed from current key/account/policy
facts. Revoke accepts exact `expected_version` and `reason`. It never returns a
token, hash, prefix, endpoint secret or secret reference.

## Revocation and accepted work

Key expiry and revocation stop **new requests**. Work already admitted by that
key is a durable command and may continue after the key expires or is revoked.
Neither key TTL nor logout is an execution deadline. To revoke existing execution
consent, disable the service account or disable organization service access;
task cancellation and exact grant revocation remain separate explicit controls.

Account role/status changes advance the existing membership execution epoch.
Disabling/re-enabling also invalidates old credentials. Service access changes in
either direction advance a separate service policy epoch and credential cutoff.
Old keys, Runs, Operations, grants and action authority do not revive when access
is later enabled. A newly issued key and a fresh explicit authorized operation
are needed. A role ceiling never changes on an existing key.

Human login mode/issuer changes in either direction advance the human policy
epoch and bearer cutoff. Previously admitted human execution consent becomes
stale, including consent from a still-valid browser session. Fresh explicit
continuation/retry captures current authority; ancestor Run consent remains
mandatory for implicit child execution. Pure service authority is unaffected by
human SSO transitions, but a service-owned operation relying on human approval or
grant still depends on that human consent. TTL-only policy changes do not revoke
either execution epoch.

All authority checks use policy, principal and membership locks before task,
operation and money locks. Policy and account management share the existing IAM
gate. Frozen private relational snapshots preserve immutable historical wire
payloads. Pending unsent jobs can release only provably unused reservations.
Sent/unknown effects are not automatically resent or financially released.
Late exact factual receipts and accounting remain admissible after authority is
revoked; recording a bill still does not prove business success.

If an identity provider or trusted server configuration is lost, recovery is an
explicit trusted-host command:

```sh
python3 scripts/dev.py recover-login-policy \
  --organization ORG_ID --reason "Recorded operator recovery"
```

It records `policy.recovered`, resets enforced login to mixed and invalidates old
human consent/bearers again. It preserves service access and lifetime settings.
It never issues a token, restores old credentials or exposes a public bypass.
Issue fresh human credentials explicitly after recovery.

## Audit, MCP and remaining work

Security audit is separate from task events and financial journals. It records
typed safe before/after snapshots, exact object/principal IDs, reason, actor
type (`principal` or `trusted_host`) and time. Entries cannot be updated or
deleted. No IdP subject or credential value is included. Only human management
callers may read it in their current organization.

The same ten routes have ten bounded private MCP tools: six readonly tools and
four mutations individually enabled by the operator. Readonly discovery now
contains37 tools; opting into all current mutations yields60. MCP arguments do
not accept caller identity, issuer readiness or credentials. The bearer adapter
cannot perform browser-required SSO transitions. Existing MCP transport/OAuth
limitations remain as documented in [MCP-ADAPTER.md](../docs/MCP-ADAPTER.md).

Remaining full-product work includes fine-grained capability grants, broader
corporate identity provisioning/federation, key issuance through approved secret
stores, complete external MCP OAuth, execution isolation, provider lifecycle,
performance evidence, independent usability and release acceptance. None is
inferred complete from this revision's endpoint count or passing tests.

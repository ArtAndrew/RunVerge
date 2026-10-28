# Organization membership lifecycle — contract0.9

This contract describes the implemented local revision0.9. The seven advertised
OpenAPI routes have actual handlers shared by SDK/UI/MCP. Integrator owns this
contract and applied immutable migration016. Full SCIM/SSO enforcement and
identity-provider onboarding remain in scope for subsequent acceptance, not
implied by these membership endpoints.

All routes use the ordinary current credential and organization membership.
Capabilities are readable by any authenticated member; all other routes require
`identity.manage` permission. Owner/admin may read this organization only.
An admin cannot create, promote, update or revoke credentials of an owner.
Only an owner can manage another owner. Self-update and self-revocation are
rejected with409 invalid_transition: use another authorized administrator.
There must remain at least one active member with owner role and an active
global principal. Mutation ordering protects that invariant across concurrent
administrators; callers never compute it from a paginated list.

## Routes and shapes

Base `/api/v1`:

- `GET /members/capabilities` → MembershipCapabilities.
- `GET /members?cursor=...&limit=...` → paginated OrganizationMember list.
- `GET /members/{principal_id}` → OrganizationMember.
- `POST /members` → OrganizationMember201; input `{display_name,role,reason}`.
- `POST /members/{principal_id}/update` → OrganizationMember200; input
  `{expected_version,display_name,role,active,reason}`. Every field is required;
  `active` must be explicitly true or false. Missing/null does not mean false.
- `POST /members/{principal_id}/revoke-credentials` → OrganizationMember200;
  input `{expected_version,reason}`.
- `GET /membership-audit?principal_id=...&cursor=...&limit=...` → paginated
  MembershipAuditEntry list; principal_id is optional and filters this org only.

Limits: list default50/max100; opaque cursor. Unknown/repeated/empty query
parameters, noncanonical nested fields, invalid Unicode, duplicate JSON, nulls
and unknown inputs are rejected. Display name is nonblank valid UTF-8, at most
120 bytes, no NUL; reason is nonblank at most4096 bytes. Role is exactly
owner/admin/member/viewer. Expected version is a positive bounded integer.
All POST require the common scoped durable idempotency key, with current
permission and target policy checked before returning a cached result. Identical
replay returns its original snapshot, not proof that old capability flags are
still current. Changed payload conflicts409. A stale expected_version conflicts409. An identical full update with a new key
is rejected409 invalid_transition before writes/audit; cached same-key replay
retains its original result.

OrganizationMember: organization_id, principal_id, display_name, role, active
(membership), principal_active (read-only global state), version, created_at,
updated_at, optional credentials_revoked_at, available_commands:string[] and
assignable_roles:string[]. Commands are `update` and `revoke_credentials`.
Capabilities are current server decisions, not delegated authorization.
MembershipCapabilities: organization_id, can_create_members, assignable_roles,
self_management_allowed=false. No browser/token/session hashes, issuer/subject,
secret references, internal generations or security epochs are exposed.

Creation allocates a NEW opaque global principal and one local membership.
Display names are organization-local; renaming a member never renames a global
principal. No caller-supplied principal ID, email, issuer/subject, global lookup,
account merge or secret token is accepted or returned. Creation is provisioning,
not a delivered invitation or proof of external identity ownership. Login is
connected separately through existing trusted-host identity linking or an
explicit trusted-host development credential issuer. Existing identity mappings
cannot silently switch principals. Automatic corporate onboarding remains open.

Audit entries contain id, organization_id, actor_id, target_principal_id,
action (`member.created`, `member.updated`, `member.credentials_revoked`), reason,
optional before, after and created_at. Before/after contain only local
display_name, role, active and version. Audit is append-only in the same
transaction as the mutation and idempotency response. It is separate from task
events/outbox: no fake Task is created and no task ID or money is required.

## Authority and revocation semantics

Role/status changes increment a membership security epoch. New decisions capture
current actor/owner epochs in private relational columns; old authority cannot
revive after a role cycle or deactivate/reactivate. A name-only edit does not
change authority. A role edit keeps credentials usable with the NEW current
role; it does not force a redundant login.

An active-status change, including re-enable, additionally advances this
organization member's credential cutoff. An explicit revoke-credentials command
advances ONLY that cutoff: it does not deactivate the member, alter their role
or revoke independent durable task grants. The UI must make that distinction
visible. Disable the member or revoke the exact task grant when stopping that
durable authorization is intended; ordinary logout semantics remain unchanged.

Bearer tokens and browser sessions receive a DB-issued immutable origin from
one PostgreSQL sequence. Admission requires origin > member cutoff. Browser
rotation copies the old origin, so rotating an old cookie does not regain a
revoked organization's access. Fresh login or trusted issuance creates a new
origin. No wall-clock comparison determines this authority. Revocation affects
only this organization's member: the same global credential may remain valid
in another organization. Global principal, token, session and issuer/subject
mappings are not modified by organization administrators.

Epochs are captured for Run owner/initiator, Operation owner/authority principal,
automation owner/issuer, tool owner/creator, approval decider and dispatcher.
A new explicitly authorized Continue operation captures the current caller and
owner without rewriting historical Run authority. Implicit team coordination
still checks its original Run authority. A new explicit Retry has new authority.
Unknown holds and prior allocations are never reset by lifecycle changes.

The linearization boundary is committed admission: new claims/reservations/
dispatch permits check current authority after waits. An already committed short
send permit may race revocation and cannot be retroactively undone. Sent/unknown
effects keep their budget holds. Exact late factual receipts/accounting remain
recordable; recording a fact never restores permission or restarts work.

## Persistence and lock ownership

Migration016 adds membership display_name/version/security_epoch/credential_cutoff
and timestamps, shared credential origin, private authority snapshots, append-only
audit and guards. Existing immutable JSON/profile/intent digests remain unchanged.
Applied001–015 are immutable; existing credentials and authorities preserve access
at migration until an actual security transition occurs.

IAM mutations acquire a per-organization IAM gate BEFORE locked authentication:
nonlocking canonical credential lookup → IAM advisory gate → fresh locked
current authentication → target policy/CAS/update/audit. They do not hold an
organization money row or tree lock while waiting for member authority. Ordinary
Mutate/MutateGuarded preserve their behavior; only the IAM wrapper uses this
pre-authorization gate. Audit never calls the task emit() helper.

Independent acceptance covers tenant isolation, admin/owner/self policy,
last-owner races, reciprocal admin changes, stale CAS, expiry-after-lock-wait,
idempotent replay after role loss, rotate-no-resurrection and A/B credential
scope, old queued work/grants/decisions after re-enable, explicit fresh actions,
and preservation of sent unknown liabilities and late factual evidence.

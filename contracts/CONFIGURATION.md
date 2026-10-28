# Configuration plane: implementation contract for revision 0.12

Status: implemented and locally verified as part of the advertised0.12.0 API.
The verification record (internal historical reference omitted) separates isolated
server/consumer scenarios from the actual local deployment and populated upgrade (internal historical reference omitted).
Main now uses SQL020. This is bounded local evidence, not full work-order
acceptance, MCP/OAuth compliance or enterprise/provider production readiness.

## Purpose and boundaries

An administrator or explicitly delegated agent can inspect approved connection
slots, create an immutable plan, validate its exact changes, obtain a required
owner approval, apply once, and separately request one bounded connection check.
The ordinary platform credential identifies the caller. A CapabilityEnvelope is
a durable delegation record, not another bearer token or executable permission.

The first profile supports installed model connections and HTTPS MCP registry
connections. Trusted host commands register versioned destinations and credential
purposes. Public commands never accept a URL, executable, environment variable,
CA, raw key, pricing profile, arbitrary tool mapping, or network policy. Each
destination fixes kind, target ID and exact credential-purpose ID/version. The
purpose fixes kind and SHA-256 of the exact canonical endpoint string. A model
profile has an empty credential reference until application fills the approved
purpose. Only redacted descriptors cross HTTP/SDK/MCP boundaries.

MCP registration/discovery does not install executable ToolConnections, grant
tools/call permission, or constitute OAuth/sandbox compliance. MCP verification
requires an explicit host assertion that discovery is unmetered; a metered
discovery profile is rejected. Model checks use the ordinary durable Task/Run,
budget reservation and dispatch path. A structurally valid response proves only
connectivity. It does not accept a user result or settle a provider bill.

## Authorization and attenuation

Human owners/admins can plan, validate, apply and verify approved destinations.
Only a human owner issues root grants and approves plans requiring approval.
Other human members and service members need an explicit current grant. Service
viewer keys cannot mutate even when membership or the stored grant is stronger.
The configuration authorizer does not add administrative role permissions to
service identities. Catalog discovery is available to authenticated members,
including viewers, but returns no destinations or secret references.

Actions: inspect, plan, validate, apply, verify, delegate. A grant pins audience
platform.configuration.v1, organization, issuer, subject, exact scopes, authority
and organization-service-policy epochs. Every ancestor must remain current.
Child actions/scopes, expiry and limits are subsets of the parent. Depth is at
most4; at most16 distinct destinations,100 plans,100 verifications and1e12
micro-USD cumulative allocated exposure per grant. All ancestor counters are
charged atomically and monotonically. Children never reset a parent's cap.
Issuing a child requires delegate and exact expected parent version. Root TTL
is1..86400 seconds. Revocation is one-way and increments authority version;
counter increments do not invalidate otherwise valid descendants or plans.

Every mutation rechecks current authorization before durable idempotency replay.
New admissions require a live credential. Accepted background work instead uses
the persisted principal kind, admitted role and execution epochs, plus current
grant ancestry. Expiring/revoking the original key prevents new admissions;
disabling the account, changing the relevant execution epochs or revoking the
grant prevents unsent verification stages. Late factual observations and
financial liabilities remain recordable after that revocation. Revoking a grant
does not roll back an already applied administrator change. Retiring a
destination/purpose version prevents future admissions using its bound resource
versions, including already copied model profiles; retirement is irreversible.

Human owners/admins can inspect all organization configuration records. Other
members can inspect their own grants (issuer or subject), plans, verifications
and related audit records. Listing destinations/resources additionally requires
a live inspect grant. Destination descriptors require exact destination pins;
resource-version metadata is visible for its allowed kind/target slot, including
older destination pins, so a newly approved version can explicitly replace an
existing target. This metadata grants no authority over the older scope.
Historical own records
remain readable with current member access after grant expiry; they confer no
new mutation authority. Cross-organization or unrelated object IDs return404.

## HTTP surface and SDK names

All routes below are relative to /api/v1/configuration. Normal current-version,
authentication, CSRF, strict UTF-8 JSON and scoped idempotency rules apply.
List routes accept only cursor/limit (default50,max100), except verifications,
which additionally accepts an optional plan_id. Queryless routes reject all
query parameters. Public types are the corresponding Go domain DTOs in
internal/domain/configuration_types.go; private host/worker structs are excluded
from generated public schemas. Money and configuration revisions use canonical
decimal JSON strings. Resource versions and counters are bounded JSON integers.

| Method and path | Store method / TS method | Python method | Result |
|---|---|---|---|
| GET /catalog | ConfigurationCatalog / configurationCatalog | configuration_catalog | ConfigurationCatalog |
| GET /destinations | ConfigurationDestinations / configurationDestinations | configuration_destinations | list ConfigurationDestination |
| GET /resources | ConfigurationResources / configurationResources | configuration_resources | list ConfigurationResource |
| GET /grants | ConfigurationGrants / configurationGrants | configuration_grants | list CapabilityEnvelope |
| GET /grants/{grant_id} | ConfigurationGrant / configurationGrant | configuration_grant | CapabilityEnvelope |
| POST /grants | CreateConfigurationGrant / createConfigurationGrant | create_configuration_grant | CapabilityEnvelope,201 |
| POST /grants/{grant_id}/revoke | RevokeConfigurationGrant / revokeConfigurationGrant | revoke_configuration_grant | CapabilityEnvelope,200 |
| GET /plans | ConfigurationPlans / configurationPlans | configuration_plans | list ConfigurationPlan |
| GET /plans/{plan_id} | ConfigurationPlan / configurationPlan | configuration_plan | ConfigurationPlan |
| POST /plans | CreateConfigurationPlan / createConfigurationPlan | create_configuration_plan | ConfigurationPlan,201 |
| POST /plans/{plan_id}/validate | ValidateConfigurationPlan / validateConfigurationPlan | validate_configuration_plan | ConfigurationValidation,200 |
| POST /plans/{plan_id}/approve | ApproveConfigurationPlan / approveConfigurationPlan | approve_configuration_plan | ConfigurationApproval,201 |
| POST /plans/{plan_id}/apply | ApplyConfigurationPlan / applyConfigurationPlan | apply_configuration_plan | ConfigurationPlan,200 |
| POST /plans/{plan_id}/verify | StartConfigurationVerification / startConfigurationVerification | start_configuration_verification | ConfigurationVerification,201 |
| GET /verifications | ConfigurationVerifications / configurationVerifications | configuration_verifications | list ConfigurationVerification |
| GET /verifications/{verification_id} | ConfigurationVerification / configurationVerification | configuration_verification | ConfigurationVerification |
| GET /audit | ConfigurationAudit / configurationAudit | configuration_audit | list ConfigurationAuditEntry |

Store reads take (ctx,principal); exact reads append ID; lists append cursor and
limit, except verifications append planID,cursor,limit. List returns
([]DTO,nextCursor,error). Mutations take (ctx,principal,[objectID,]key,input)
and return (json.RawMessage,error), matching existing durable mutation helpers.
Consumers use explicit caller-provided idempotency keys, never retry POSTs or
automatically create a replacement verification. No public endpoint installs
trusted host profiles or returns private configuration.

## Plan, validation, approval and application

CreateConfigurationPlan pins an exact base_revision, explicit TTL upper bound
1..3600 seconds (actual expiry is clipped to every grant ancestor)
and1..8 operations. Each operation has a unique ID, approved destination ID and
version, exact expected_resource_version (zero means absent), and required
explicit active boolean. Duplicate target slots are invalid. Other fields are
rejected, including a caller-selected credential purpose. The server expands
the destination/purpose pair into immutable ConfigurationScope and computes a
SHA-256 digest of the canonical intent. The diff lists new versions, exact
retired versions and whether pinned tasks may stop. Activating a model must not
retire another logical connection ID with the same model name: reject that
collision. Replacing the same target is explicit in the diff.

Static validation performs no external request, consumes no model budget and
persists an immutable explanation. An authorized request returns200 with
valid=false and bounded PolicyDecision reason_codes/obligations when the exact
plan is stale or expired. A statically valid plan awaiting owner approval has
valid=true, decision outcome requires_approval and an owner_approval
obligation; the approval binds that same validation ID. An authority
admission failure remains an ordinary403/404 safe error. PolicyDecision is an
explanation at one instant, not a reusable permit. Version/digest mismatch is409.
Approval binds the exact plan digest and validation ID and current approving
owner epochs. Services cannot approve. Apply rechecks all authority, current
profiles, owner approval when required, validation ID and exact base/resource
versions in one transaction. It activates/saves settings immediately; no network
request, hidden verification or rollback runs as part of apply. The plan changes
planned/version1 to applied/version2 exactly once. Current configuration
revision may advance more than once inside a plan and is an opaque CAS token.

## Separate connection verification

Only an applied active operation can be checked. The request pins plan version,
digest and operation ID, explicit max_charge_microusd, acknowledge_cost=true and
a reason. The ceiling cannot exceed destination, model or any ancestor grant
limit. At most one verification exists per plan/operation regardless of a new
idempotency key; a lost reply cannot create another paid job. A failed/unknown
check requires a new explicit plan/operation to create another attempt.

Model checks create an ordinary pinned no-tools agent/task with max_runs=1 and
a fixed server-owned prompt. Reservation occurs through the existing cost
ledger before external I/O. Raw response text and provider_request_id are
discarded before public artifacts/database persistence because either can echo
the credential. Public receipts expose only safe structural facts, counts and
digests. Usage estimates remain estimates; unresolved bills retain reservations.

MCP checks use a bounded worker slot and the registered HTTPS client, no
AuthorizeCall callback, no tools/call, transport retry or fallback. The durable
dispatch marker commits before Connect/ListTools, including before credential
transmission during negotiation. Raw peer descriptions, errors and tool bodies
are not persisted or exposed. Only protocol version, bounded count and catalog
digest are recorded. A sent occurrence cannot return to queued or be resent;
after a crash it becomes unknown, while an original late observation can still
record response_received with its original dispatch identity. A known unsent
expired lease can be reclaimed with a higher fencing token. Verification
response_received is separate from financial settlement and factual correctness.

## CLI, usability and evidence

Ordinary platform config commands use HTTP and the current platform credential;
they dispatch before the process opens DATABASE_URL. Support machine-readable
JSON for catalog, lists, exact reads and all plan/grant commands. Separate host
commands install purpose/destination profiles from new private JSON files with
no raw secret. CLI JSON must never print credentials or private config.

The web UI labels saved settings as connection not checked until a separate
receipt exists. It shows the diff, retirement impact, validation reasons,
required owner approval and cost ceiling before separate explicit actions.
Draft intent/reason survives failed refresh; stale authorization disables
commands. Switching organization clears scoped state. SDK examples must show
discovery and exact plan flow without hidden POST retry or elevated DB access.

Acceptance evidence includes cross-tenant/service-viewer/replay denial,
attenuation/shared-cap races, current authority after lock waits, plan CAS and
approval staleness, no I/O during static commands, exact-key/destination pairing,
revoked-purpose future model admission denial, model reserve-before-send,
credential echo scrubbing, physical MCP no-resend after crash/revocation, late
safe evidence, bounded concurrency, SDK/UI/CLI actual-server flows and populated
018-to019 preservation. These do not by themselves close the full original
W01/W05/W06/W11/W12/W17 work orders or establish production readiness.

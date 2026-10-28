# Signed task-event webhooks: future public implementation boundary

Status: under development; the public webhook surfaces remain unmounted.
API0.14 is assigned to explicit local model accounting, not webhook acceptance.
Historical private webhook checkpoints retain their0.14 filenames/labels.
This document freezes the webhook implementation boundary, not release
acceptance. Original requirements: ADR-0021-T02 and W01. The
independent design review
records the safety interpretation and required adversarial evidence.

Implementation checkpoint2026-10-26: signing/profile primitives and subscription/
atomic-fanout storage are verified in disposable PostgreSQL schemas. SQL021 was
first applied successfully and is immutable; the saved main profile remains020.
See storage evidence (internal historical reference omitted). The subsequent
private runtime checkpoint (internal historical reference omitted) implements
HTTPS sending, claims/recovery, short fenced permits, exact late observations
and bounded explicit retry. The worker switch defaults off until the complete
public webhook feature. Dedicated public permissions, new HTTP/CLI/SDK/MCP/Workspace
surfaces, operator/key commands and Apache receiver helpers remain unimplemented.

## Delivery semantics

A human organization owner/admin with a dedicated management permission can
subscribe one task to one exact trusted installed receiver/version. Subscription
creation explicitly acknowledges export of task/event metadata and names1..16
registered event types, a lifetime of1..86400 seconds and1..1024 notifications.
At most8 accepting subscriptions per task and32 per organization are admitted.
Service identities have no implicit webhook-management permission.

Creation serializes with committed organization event allocations and records
`start_after_sequence`. Historical events are not exported by creation. Each
matching new committed event freezes one delivery and its first physical attempt
in the same transaction as state/event/outbox publication. A composite unique key
prevents duplicate logical deliveries. Subscription matching does not acquire
inverted IAM locks inside event publication. Actual dispatch rechecks current
authority before external I/O.

The notification count is a declared lifetime admission bound. Reaching it
durably marks the subscription `exhausted` and records the exact last admitted
sequence. No further event is eligible under that exhausted subscription; its
already admitted deliveries may finish before expiry unless separately revoked.
The status/cutoff remains inspectable. This is a visible bounded subscription,
not silent queue eviction or unbounded promised delivery. Notification lifecycle
audit does not re-enter the subscribed task-event loop.

Every logical delivery has immutable exact envelope bytes/digest and a stable
delivery ID. Append-only attempts have distinct IDs and lifetime sequence
numbers. Up to8 attempts may be allocated, including explicit redelivery; retry
does not reset age, counters, source identity, destination/key version or old
unknown outcomes. Maximum delivery age is24hours, capped by subscription expiry.
Claim recovery is separately bounded; only a durable proven-unsent attempt can
be reclaimed automatically.

The worker commits a fenced sent marker and short dispatch permit before making
one physical HTTPS request outside the transaction. Lost marker acknowledgment,
process death after that marker, timeout, partial transport failure and unknown
response outcomes are never automatically sent again. Non2xx/429/5xx does not
prove absence of a receiver effect and does not authorize automatic retry.

An explicit retry command names exact delivery version, current attempt ID and
envelope digest, supplies a reason and acknowledges possible duplicate receipt.
It requires fresh management authority and a still-valid exact subscription/
destination. It allocates a new attempt with the **same delivery ID and body**.
Late observations remain recordable after revocation/lease loss; they do not
restore dispatch authority. Current aggregate status never hides old attempts:
`unknown_attempts` (currently unresolved), `ever_unknown_attempts` (historical)
and `acknowledged_attempts` remain distinct facts. An exact late acknowledgment
can resolve that attempt's current uncertainty while retaining its immutable
`unknown_at` and the historical count; retry cannot reset either history. No retry is
admitted once receipt is already acknowledged. No financial Action, source Task
acceptance or model continuation is inferred from receipt of a notification.

## Current authorization and endpoint trust

Subscriptions freeze creator and source task-owner identities and immutable
membership/security/policy consent epochs. Public reads/mutations use ordinary
current organization-scoped authorization before idempotency replay. Claim and
final locked admission recheck active organization/identities, current human
management role, exact source owner, consent epochs, subscription state/expiry,
destination version/digest and lifetime bounds. Role/account restore cannot
revive old consent. Logout/key expiry is not automatic cancellation of previously
admitted durable work; explicit account/policy/subscription/endpoint retirement
stops future admissions. A short already-issued send permit may be in flight.
Terminal/cancelled Tasks remain legitimate subjects for their completion events.

Only the trusted installation operator supplies endpoint URL, CA, explicit
private-network approval and dedicated signing-key environment reference.
Public descriptors contain ID/version/name/public signing-key ID and the
unmetered assertion; never URL, CA, environment name or key bytes. A new profile
version never silently retargets old deliveries; retiring an old version stops
its future sends. Reinstalling that immutable version cannot reactivate it.

Receiver notification delivery requires explicit operator approval as unmetered.
That assertion is not proof of an arbitrary receiver's tariff, infrastructure
egress costs or business effect. No paid model/tool action or trusted cost
settlement is introduced by webhook delivery. Metered receiver profiles are
unsupported until their exposure and reconciliation contract exists.

Transport requires fixed HTTPS without userinfo/query/fragment. Each connection
resolves and checks every answer, then dials only a checked IP with the original
hostname/SNI/certificate verification. No ambient proxy, redirect, reusable
connection, automatic HTTP resend, HTTP2 or GetBody replay. Private network
opt-in does not admit link-local/metadata/multicast/unspecified destinations and
is not an OS egress firewall. Response prose/headers are discarded; safe status
and fixed reason codes cannot echo signing material or payload content.

## Signed wire format version1

The body is at most4096 raw bytes, UTF-8 JSON containing only:

```json
{
  "envelope_version": 1,
  "delivery_id": "whd_example",
  "organization_id": "org_example",
  "subscription_id": "whs_example",
  "event": {
    "id": "evt_example",
    "task_id": "tsk_example",
    "sequence": "9007199254740993",
    "type": "task.created",
    "payload_version": 1,
    "created_at": "2026-10-26T06:00:00Z"
  }
}
```

No Event.data, legacy JSONB, task goal/result, material/artifact content,
arguments, reason or provider response is sent. Metadata still reveals IDs,
event kind and timing; explicit subscription consent authorizes this export.
Any full data fetch uses an independently authorized ordinary API client.

Headers are exactly one each:

- `Webhook-Key-ID`: opaque public key identity1..128 ASCII identifier characters;
- `Webhook-ID`: stable logical delivery ID;
- `Webhook-Attempt-ID`: physical attempt ID;
- `Webhook-Timestamp`: canonical nonnegative decimal Unix seconds;
- `Webhook-Signature`: `v1=` followed by64 lowercase hexadecimal characters.

The HMAC-SHA256 input is the concatenation of these exact bytes:

```text
agent-platform-webhook-v1\n
KEY_ID\n
TIMESTAMP\n
DELIVERY_ID\n
ATTEMPT_ID\n
RAW_BODY_BYTES
```

Here `\n` means one byte0x0a, not a backslash and letter. The raw body is not
parsed/reformatted before verification. Key identity/timestamp/delivery/attempt
IDs are bound by the MAC; newline/space/comma ambiguity in those fields is
rejected. Receivers check exactly one raw header value case-insensitively before
calling Header.Get or another lossy extraction, and reject joined values.
Signing material is32..64 bytes generated by a trusted cryptographic
random generator. The worker receives only canonical unpadded base64url material
through `PLATFORM_WEBHOOK_KEY_*`; raw keys are never stored in PostgreSQL.
Verification uses constant-time MAC comparison and a configurable inclusive
timestamp tolerance of1..3600 whole seconds, normally300. Duplicate headers are
rejected before calling the verifier.

An authenticated in-window replay remains a valid signature. Receivers must
durably deduplicate by organization/delivery ID and reject a changed body digest
for the same logical ID. They acknowledge recording the notification, not
exactly-once arbitrary business effects. Signature verification grants no access
to the platform and is not durable replay protection. After MAC verification,
receivers additionally validate the complete metadata-only envelope and equality
of header/body delivery IDs before accepting it into their inbox. The MAC alone
does not parse JSON or establish these semantic bindings. A late explicit retry gets
a fresh timestamp/attempt ID but the same logical identity/body.

## Public surfaces and acceptance

The intended future webhook API exposes safe destinations, task subscriptions, exact
revocation, paginated deliveries/attempts and explicit retry. CLI/SDK/MCP and
Workspace use those same routes and current authorization. Mutation tools remain
operator opt-in; the capability catalogue describes this bounded profile only
after implementation. Client SDK receiver verification/examples remain
Apache-2.0; server/storage/runtime and web remain ELv2.

Required evidence includes actual TLS/PG atomic fanout/rollback, cross-tenant and
role/epoch tests, exact signature vectors and cross-language receivers, timeout
after receipt with no automatic resend, duplicate-ID receiver suppression on
explicit retry, marker commit-ack loss, process kill before/after admission,
revocation/retirement lock waits and late receipt, bounded expiry/exhaustion,
network/redirect/key-echo cases, migration preservation and all consumer surfaces.
Neither this draft nor the existing pure signing primitive closes W01 or release.

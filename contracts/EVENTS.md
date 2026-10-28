# Events and API compatibility — revision0.11

This contract describes implemented source behavior. Evidence for a release is
separate; changing a source version does not upgrade the running local service.

## Explicit API revision

All authenticated `/api/v1` requests require exactly one
`X-Platform-API-Version: 0.11.0` header. The server authenticates first, then
checks the revision before command decoding, idempotency replay or business
changes. Missing, duplicate or unsupported revisions return409
`incompatible_client`, with `retryable:false`. Responses include the server
revision. SDKs validate it and never automatically repeat a command because a
response was incompatible or lost. Existing durable idempotency receipts are
preserved; an explicitly compatible request can retrieve the original receipt.

`GET /api/v1/compatibility` discovers the sole supported revision, header name,
event versions and retry policy without credentials. The revision header may
be absent there; if supplied, it must be valid. Login/session endpoints and
the separate MCP transport retain their own protocols. The MCP backend itself
selects the current ordinary API revision; tool arguments cannot override it.

## Event data

Event envelopes contain `id`, `organization_id`, `task_id`, `sequence`, `type`,
`payload_version` and `created_at`. Sequence is a canonical positive decimal
string through9223372036854775807. Global sequence gaps are expected and do not
establish loss. Committed event allocations remain ordered within an organization.

Version1 has a required `data` object whose complete fields, primitives,
constants and discriminated variants are registered in
[event-payloads.json](event-payloads.json). Every production emitter validates
the registered payload before the state/event/outbox transaction commits.
Money and fencing positions are exact decimal strings. Unknown kinds or fields,
missing fields, invalid Unicode, nulls and oversized new payloads fail closed.
An absent material merge base is omitted from new payloads.

Historical rows retain version0 and their original JSONB. They expose exact
stored JSONB text as `legacy_data_json`, with no `data` field. Clients preserve
that string as opaque history: they do not reinterpret large old numbers as
typed facts or execute its contents. Unknown future versions/types are explicit
protocol errors for version1 consumers. Old version0 types remain readable.
The shared [conformance corpus](event-conformance.json) covers every registered
kind, large integers, legacy records and invalid wires. These two generated
protocol artifacts use Apache-2.0; Go server/generator code remains ELv2.

## Consistent snapshot and bounded replay

`GET /tasks/{id}/event-snapshot` returns a fresh authorized `Task`, the current
per-task replay window, its head cursor, the window-floor `replay_cursor`, up
to50 recent events and `observed_at`, all from one RepeatableRead snapshot.
The preview has a256KiB event budget and may omit older/oversized entries;
`has_earlier_events` makes that omission explicit. It does not claim to include
artifact bodies, full costs, materials or every descendant state.

`GET /tasks/{id}/event-feed?cursor=...&limit=...` replays up to100 events (default50)
within a32KiB event budget. A successful cursor advances only through delivered
events and is returned even for an empty page; `has_more` signals another page.
Absent cursor starts at zero. Explicit empty, repeated, unknown or malformed
query fields are rejected. Future coordinates or wrong organization/task
coordinates are rejected after current authorization and task scope checks.

Cursors are canonical opaque base64url read coordinates containing format,
organization, task and an exact decimal position. They are neither credentials
nor delegated capabilities; every read authenticates again. Clients keep them
opaque. The `window.version` is a positive mutable policy revision; it is not
the cursor/payload format version. Head and floor belong to this exact task,
so unrelated organizations/tasks cannot create a quiet-task reset loop.

A coordinate below the logical floor returns a successful batch with
`reset_required:true`, `reset_reason:"history_expired"`, `events:[]`, `cursor:""`
and `has_more:false`. Clients explicitly request a fresh snapshot; they never
silently skip missing history. A single oversized legacy event that cannot fit
stream replay returns409 `invalid_transition` without cursor advancement; a
snapshot remains the explicit recovery path. `/tasks/{id}/events?after=...`
continues to expose immutable full history, independent of the logical floor.

Only the trusted-host `advance-event-replay --organization ... --task ...
--after ... --reason ...` command advances the logical floor. Advances are
monotonic, task-head bounded and audited atomically; equality is a no-op.
They notify subscribers after commit. No API/MCP retention mutation exists.
Nothing is physically deleted: event/outbox history still protects financial
facts and tool-conflict deduplication. Disk retention/archive remains W15 work.

## Bounded SSE connection

`GET /tasks/{id}/event-stream?cursor=...` uses ordinary authenticated fetch/SDK
headers. `Last-Event-ID` may supply the cursor when the query omits it; if both
are present they must match exactly. Repeated, empty or conflicting IDs fail.

Each complete SSE data frame contains one `TaskEventStreamFrame`:

| SSE event | Frame kind | Resumable id |
|---|---|---|
| `platform.events` | `batch` | Exactly the successful batch cursor |
| `platform.reset` | `reset_required` | Absent |
| `platform.error` | `error` | Absent |

Heartbeat comments carry no cursor. The bounded protocol accepts data/event/id
and comments, with CR/LF/CRLF lines; it rejects unknown fields, duplicate event
or id fields, invalid UTF-8 and oversized64KiB frames. A truncated final frame
cannot advance a cursor. A reset/error frame closes the connection.

The current local server admits at most32 streams and4 per principal. Each
connection has a20-second lifetime, two-second heartbeats and a three-second
deadline for each write. Go TLS may synchronously spend up to five additional
seconds sending `close_notify` after a failed write; physical admission stays
held until the handler returns. Thus three seconds is the I/O deadline, not an
end-to-end cleanup promise. Slow-client tests must observe actual blocked I/O
and this additional cleanup bound. These defaults are not a provider scale SLO.

Every batch and heartbeat rechecks current authorization after its short read
transaction and before output. Revocation linearizes at that check; bytes
already accepted by a socket cannot be recalled. No database transaction or
pool connection is held while waiting or sending. One shared PostgreSQL LISTEN
connection per Store provides bounded, coalesced wake hints; committed replay
is authoritative and repairs notifications lost during reconnection.

SDK stream iterators open one connection and close/cancel their reader. They
perform no hidden reconnect or mutation retries. The UI reconnects read-only
with bounded delay from its last fully processed batch, keeps at most300 journal
events and requires an explicit reset acknowledgment before replacing the
snapshot. Saved drafts and pending command identities survive interruption.
A task-only feed does not contain every descendant cost change: the UI still
refreshes authoritative task/cost/child resources periodically.

MCP exposes `platform_api_compatibility`, `platform_event_snapshot` and
`platform_event_feed` through the same API. It does not expose an SSE tool or
confuse API events with MCP transport messages. The catalog now has40 readonly
tools plus23 explicitly enabled mutation tools.

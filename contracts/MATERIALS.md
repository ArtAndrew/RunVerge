# Collaborative materials contract 0.5.0

A Material is a logical document owned by one organization and Task. Its head can advance; each MaterialRevision is immutable. An Artifact is a separately immutable result snapshot. A draft is neither the Task's current candidate nor an accepted result. All routes are under `/api/v1`; ordinary current authorization and scoped durable idempotency apply.

| Method and route | Result |
|---|---|
| POST `/tasks/{id}/materials` | 201 Material head |
| GET `/tasks/{id}/materials` | Paginated Material metadata |
| GET `/tasks/{id}/materials/{material_id}` | Material head |
| POST `/tasks/{id}/materials/{material_id}/revisions` | 201 updated Material head |
| GET `/tasks/{id}/materials/{material_id}/revisions` | Paginated revision metadata |
| GET `/tasks/{id}/materials/{material_id}/revisions/{version}` | MaterialContent: exact revision metadata and content |
| POST `/tasks/{id}/materials/{material_id}/publish` | 200 Task with a new current candidate |

GET `/tasks/{id}/artifacts/{artifact_id}?version=1` retrieves the exact immutable Artifact; version defaults to1. The artifact list is now paginated, default20/max100. Material and revision lists default50/max100. Lists use opaque `cursor` and descending `created_at,id`; the caller must follow `next_cursor` rather than assume one page contains the current candidate or review basis. Exact head/content routes accept no query parameters. Exact artifact reads accept only a positive version. Unknown, repeated or malformed query fields are rejected.

## Editing and conflicts

Create input: `{name,media_type,content,base_candidate_id?,reason?}`. `content` must be present; empty is a valid draft. `media_type` is `text/plain`, `text/markdown` or `application/json`. The optional base candidate must belong to the same organization/Task. It records the document's origin and is not publication authority.

Revise input: `{expected_version,content,reason,merge_base_version?}`. It requires the exact current head version, a present content field and a nonblank reason. A stale version returns409 and appends nothing. There is no automatic last-write-wins overwrite or automatic retry with changed content. Read the new head, compare both texts, explicitly merge, then submit a new command with the current `expected_version`, a fresh idempotency key and optional `merge_base_version` recording the older revision used by the editor. Merge base must be an existing revision1..expected_version. This is manual merge provenance, not CRDT/OT or a claim of semantic conflict resolution.

Material metadata contains stable identity, name/type, `head_version`, `head_content_digest`, optional origin, creator and timestamps. Revision metadata contains immutable identity, `version`, `base_version` (=version−1), optional `merge_base_version`, content SHA-256, authenticated principal, optional reason and timestamp. Authorship is server-derived. Exact content is `{revision,content}`; the digest covers the original UTF-8 content bytes. Invalid UTF-8, NUL and unpaired escaped Unicode surrogates are rejected rather than repaired before hashing. Draft JSON may be temporarily invalid while editing.

Draft writes do not change Task version/status/candidate/result, create a Run, dispatch a model or alter reserves/spend/financial holds. A draft can still be edited after the Task is terminal. Those edits cannot reopen that Task or change its accepted result.

## Publication and separate acceptance

Publish input: `{expected_task_version,expected_material_version,content_digest,parent_candidate_id}`. It binds the exact current Task version, current document head/digest and predecessor candidate. Publication requires the Task to be waiting for an undecided current model/team/material candidate in its current Run, with the corresponding operation blocked and no active operations in the subtree. A managed child's candidate cannot be replaced once its parent has consumed it. A terminal, cancelled, changed, reviewed or consumed snapshot is rejected.

Publication requires nonblank text; an `application/json` document must now contain valid JSON. It creates a fresh physical Artifact and a new TaskCandidate with source `material`, advances Task version once and leaves it waiting for a decision. It does not create another Run, call a model, acknowledge a new paid attempt, settle money or remove existing financial liabilities. Accept/reject/request_changes remains a separate exact snapshot command under [COLLABORATION.md](COLLABORATION.md). An actor allowed to publish need not be allowed to accept.

The candidate carries `parent_candidate_id`, `review_basis_candidate_id`, `material_id`, `material_version` and `published_by`. Parent is the previous candidate in this same Task/Run/Operation; review basis is the original generated candidate, never another material candidate. Composite foreign keys and immutable rows bind these facts. Repeated edits cannot create an unbounded basis traversal.

For an edited team result, the reviewed text is the newly published exact text. Inherited child receipts and execution closure are still taken from the original immutable team manifest identified by the review basis. Editing prose or supplying JSON cannot invent, omit or swap the original selected children. A separately accepted child keeps its own decision. Human edits are attributed; neither publication nor acceptance proves factual truth.

## Permissions and bounds

Reads require current `tasks.read` and an authorized same-organization Task. Each multi-query content read uses a single repeatable-read, read-only transaction whose first snapshot checks current credentials and membership. A caller-supplied stale Principal role is not read authority.

Member can write/publish materials on owned Tasks (`materials.write_own`, `materials.publish_own`). Admin/owner additionally have the corresponding `_any` permissions. Viewer has reads only. `tasks.review` remains separate. Mutations recheck current authority before a cached idempotency replay; revoked credentials do not recover old response contents. Browser mutations keep ordinary CSRF protection. MCP invokes these same routes; a transport allowlist does not create additional permissions.

| Bound | Value |
|---|---:|
| Documents per Task | 32 |
| Documents per organization | 10,000 |
| Revisions per document | 128 |
| Material publications per Task, across attempts | 64 |
| Content per revision | 262,144 UTF-8 bytes |
| Reason per revision | 4,096 UTF-8 bytes |
| Name | 120 UTF-8 bytes, nonblank |
| Organization material bytes | 268,435,456 |

The monotonic material byte quota counts saved content/reasons and copied material publication content, not all PostgreSQL disk usage or a billing amount. Metadata, indexes, generated artifacts and other product storage need the separate full storage/retention design. Quota admission and append/publication/event state commit atomically. Failed conflicts and rejected quota writes consume no revision or publication count. Counters cannot reset when a new Run starts.

## Workspace recovery boundary

The Workspace offers conflict comparison and explicit manual merge, exact historical content, frozen publication inputs and a separate result decision. An unsaved buffer is volatile RAM, bounded to8 entries of at most262KiB each, keyed by normalized API endpoint/principal/organization/Task/material (new draft separately). It is never automatically posted, logged or written to browser persistent storage. There is no cross-device/offline guarantee; reloading or closing the page loses this RAM.

401/403/404 removes visible server data. Recovery is offered only after fresh `/me` confirms the same principal/organization and a fresh authorized material read (or Task read for an unsaved new document) succeeds. A different principal or endpoint cannot recover the buffer. Explicit restore is separate from save; a changed head still requires a manual merge. Intentional logout/discard uses an unsaved guard and clears the buffers; exceeding the bound does not silently evict a dirty document. This is bounded local recovery, not completed realtime collaborative editing or the full W08 usability acceptance.

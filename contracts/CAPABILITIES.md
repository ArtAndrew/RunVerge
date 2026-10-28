# Product capability catalogue API0.14

Implementation boundary under development. Canonical DTOs:
`internal/domain/capability_types.go`. `GET /api/v1/capabilities` accepts no query
or body and uses ordinary current authentication, organization selection and
the exact API0.14.0 header. It performs no external probe or mutation.

The result is `{data: ProductCapabilities}`. Fixed fields:
`catalog_version="1"`, `api_contract="0.14.0"`,
`assessment="support_and_role_observation"`, `read_only=true`.
`observed_at` is RFC3339 UTC; `identity` is the currently authenticated identity,
with effective permissions from the existing shared permission implementation.

This describes support and role prerequisites. It is not an authorization
decision, dispatch permit, proof that a resource exists, or reusable grant.
Permission alternatives are descriptive OR-of-AND groups; an empty list means
that the access model and runtime guards must be consulted, never universal
permission. Actual routes continue to enforce their existing authorization.
Configuration delegation is separate from role permissions.

## Frozen wire enums and bounds

| Field | Values |
|---|---|
| operation response_kind | `object`, `list`, `sse` |
| operation access_model | `public`, `current_membership`, `role_and_resource`, `scoped_configuration`, `human_owner` |
| operation effect / step effect | `read_only`, `platform_write`, `queues_external_work`, `external_dispatch`, `records_observation` |
| operation cost_semantics | `none`, `may_reserve_and_queue`, `reserves_upper_bound`, `possible_charge_existing_reservation`, `observation_no_settlement` |
| operation retry_policy | `read_with_current_authorization`, `explicit_same_key_after_inspection`, `scoped_stream_resume` |
| binding surface | `http`, `cli`, `python`, `typescript`, `mcp`, `workspace` |
| binding status | `available`, `operator_opt_in`, `not_exposed` |
| profile status | `implemented`, `bounded`, `not_implemented` |
| onboarding status | `available`, `partial`, `not_implemented` |
| role principal_kind | `human`, `service` |
| role role | human: `viewer`, `member`, `admin`, `owner`; service: `viewer`, `member` |

Arrays are bounded: operations1..128; profiles1..32; roles exactly6;
onboarding exactly6; authorization_rules1..32; remediation_command1..20;
permission_alternatives0..4 with1..4 permissions per group;
runtime_guards1..32; bindings exactly6, one per surface; documentation1..8;
profile constraints1..32; onboarding prerequisites1..32 and steps0..12;
step operation_ids0..16; commands0..4, each1..20 arguments;
role permissions0..64. Identifiers and schema names are at most128 UTF-8 bytes;
paths/binding names/command arguments at most512; explanatory text at most4096;
documentation references at most256. All strings are valid non-NUL Unicode.
`input_schema` may be empty; an unavailable binding has an empty `name`.
Titles, instructions and actor names are display-only bounded text, not code,
HTML, tool calls or authority. Command templates use `platform` as their first
argument; `<private-plan.json>`-style placeholders are argv data, never shell.
Documentation paths are repository-relative
`docs/` or `contracts/` references, with no URL, absolute path or parent traversal.

Operation IDs equal their canonical OpenAPI operationId. Method is uppercase;
path includes `/api/v1`. IDs for operations/profiles/onboarding are unique.
Each role kind/role pair is unique. `identity` must belong to the selected
organization; clients knowing the active principal must reject another identity.
No private endpoint, credential reference/value, peer-supplied prose or tenant
resource inventory is included. This catalogue never executes step commands.

## Sources and completeness

The operation list is generated from the same endpoint definitions as OpenAPI.
MCP bindings come from the registered tool catalogue; mutation tools remain
operator opt-in. SDK/Workspace bindings identify real implemented surfaces;
missing bindings are explicitly `not_exposed`. SSE remains a separate transport,
not a JSON MCP tool. Installed profiles and connection availability are read
through their ordinary scoped APIs, not inferred from the static support list.

Profiles explicitly distinguish bounded local support from absent enterprise,
OAuth, sandbox, provider lifecycle and operational features. Six onboarding
personas are guidance paths, not new IAM roles. Their privileged prerequisites
remain explicit. Provider onboarding cannot be advertised as available before
the provider lifecycle exists.

Effects, financial obligations and result acceptance remain separate facts.
Unknown external outcomes are never automatically resent; successful connectivity
does not settle an invoice or accept model output. Credential expiry blocks new
admissions; execution consent, immutable counters and prior liabilities follow
the existing security/configuration/automation contracts.

The offline product remediation catalogue remains
`platform doctor --catalog --json`. It does not execute repairs. Full usability,
all original W11 dependencies and the complete product require separate evidence.

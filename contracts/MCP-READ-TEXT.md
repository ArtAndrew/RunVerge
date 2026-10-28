# Registered standard MCP read-text execution

Implementation contract for the bounded read portion of ADR-0022/W06. Introduced
under API0.13; the current public contract is0.14. This is an additional trusted-host ToolConnection
profile, not a new route or a promise of complete MCP interoperability. Status:
bounded implementation verified in the current checkpoint (internal historical reference omitted);
broader peer acceptance and alpha release remain incomplete.

## Registration and admission

`result_contract: "mcp_read_text_v1"` is allowed only with `transport: "mcp_https"`
and a host-installed `tool.effect: "read"`. The operator registers the exact
endpoint/remote tool, argument schema/digest, credential purpose, network/CA
policy, price revision and charge bound. Peer descriptions/readOnlyHint/output
annotations cannot create or change this authority. Misclassified or malicious
servers are not made read-only by this label; OS isolation remains separate.

Current identity, pinned tool/profile/argument bytes, task/root budget, approval
or explicit automation grant, lifetime counters, fencing and final dispatch
permit all remain authoritative. External I/O stays outside the transaction.
No public credential/endpoint selection or self-registration is added.

## Standard request and untrusted response

Send the approved argument JSON object as `tools/call.params.arguments`, without
the existing custom execution/action envelope. Keep precise integer tokens;
do not decode arguments through float64. JSON-RPC IDs correlate transport only;
the existing action identity and short permit still control one physical call.

Validate the matching original response before the SDK can lose duplicate keys,
invalid Unicode, missing text or null-vs-empty distinctions. Support finite JSON
and SSE responses under existing byte/time bounds. Refuse multiple matching
results, malformed errors, interactive input requests and continuations.

Accept1..16 `content` blocks, each with exact `type: "text"` and a present string
`text`. Explicit empty text represents an empty read and is valid. Store the
ordered text values as one canonical JSON array of strings, including for a
single block, so block boundaries are lossless rather than guessed from inserted
delimiters. Bound both aggregate decoded UTF-8 text and the complete encoded
artifact by the registered output limit; NUL is forbidden. Image/audio/resource/tool-use
blocks and structured-only results are outside this profile. Additional metadata
and structured content are not interpreted as instructions, authority or proof.
Preserve a digest of the original matched result; do not fetch links/resources.

For this read profile, `succeeded` means the selected server supplied bounded
data; it does not verify that the data is true, that the user's task is complete
or that a business write occurred. A structurally valid `isError:true` is a
registered read-error observation; its `rejected` state is not evidence of zero
charge. The existing write/destructive custom result contract is unchanged.
Missing/malformed response, transport loss or input-required outcome remains
unknown and cannot trigger an automatic resend of the same occurrence.

The receipt reference is constructed by the kernel from the immutable action
identity. It is a local read-observation identity, not an external business ID.
Data remains an untrusted artifact; acceptance and model continuation keep their
existing separate authorization and bounds. Transport success/error never
settles or releases a financial reservation. Max charge0, if explicitly installed,
is the operator's tariff assumption, not something inferred from readOnlyHint.

## Verification required

- Actual TLS JSON/SSE: ordinary raw arguments, precise large integers, explicit
  empty strings, multiple text blocks and original-result digest.
- Reject wrong effect/transport, schema mismatch, missing/null/duplicate/wrongcase
  fields, bad Unicode/NUL, nontext/oversized content, mismatched RPC IDs,
  multiple matching results and interactive continuations.
- Permit/expiry/current-authority failures cause no physical call. A lost reply
  is unknown, retains accounting and is not resent. Legacy write tests still pass.
- Real PostgreSQL/profile/receipt/tenant checks and an installed task using one
  independently defined standard MCP server; do not substitute discovery alone.
- Independent safety review. Full OAuth/peer matrix and other media remain open.

Primary protocol references, checked2026-10-26:
[Tools](https://modelcontextprotocol.io/specification/2026-07-28/server/tools),
[Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http).
The implementation uses the already pinned Go SDK1.8.0.

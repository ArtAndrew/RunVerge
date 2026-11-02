# OpenRouter usage observations

`NewOpenRouterUsageReader` constructs a metadata reader without connecting.
`Observe(ctx, generationID)` makes one GET to
`https://openrouter.ai/api/v1/generation?id=...` using the explicitly supplied
credential. It does not generate model output. This internal component is not
connected to the Workspace, a CLI command or the accounting ledger.

The reader uses a fixed HTTPS origin, normal certificate/hostname verification,
no environment proxy, no redirects and no application retry. A caller chooses
a positive timeout of at most 30 seconds. Response headers are bounded to 16 KiB
and the uncompressed response body to 256 KiB. Unexpected encoding, malformed
JSON, duplicate keys, invalid Unicode, excessive nesting or a mismatched
generation ID return a fixed error with no partial observation. Non-200 errors
include only their status code, not the provider body or credential.

## What an observation means

The response reports a generation's cost at the time it was read. It is not proof
of a final invoice, ownership by a particular RunVerge organization, complete
coverage of a recovery window, successful task execution or permission to resume.
`settlement_state` is always `unverified`.

`Record()` returns metadata by value. `RawResponse()` returns a copy of the exact
received bytes; `response_sha256` binds those bytes. Keep this data private.
Direct and JSON-escaped echoes of the supplied credential are rejected before
retention; this is not detection of every possible encoding of a secret.

The cost is retained as its exact JSON number lexeme in `total_cost_credits`.
There is no float conversion. Under the USD-based credit unit, `microusd_status`
reports whether the value is exactly representable in a signed 64-bit integer
number of micro-USD:

- `exact`: `exact_microusd` contains that integer, including explicit `"0"`.
- `fractional`: a sub-micro fraction exists; no rounding is applied.
- `out_of_range`: the integer does not fit; no amount is substituted.

Decimal lexemes are bounded to 128 bytes and exponents to ±1000. Unsupported or
negative values, missing/null cost, and missing/non-boolean `is_byok` are refused.
An upstream inference cost is never substituted for the OpenRouter account cost.
For BYOK, this observation does not establish the separate upstream provider bill.

The API requires a previously known generation ID. If the original response was
lost, the absence of an ID or an HTTP 404 is not proof of no charge. A later
integration must independently bind the generation and current provider account
to the exact archived execution, retain authenticated acquisition provenance and
define monetary finality/correction handling. A caller-edited `Record()` value
is not an authorization or verification token.

## Validation and references

The package tests use owned local HTTPS peers and synthetic credentials. They
exercise the production transport with test-only DNS routing and certificate
roots, including proxy/redirect refusal, TLS failure, cancellation/timeouts,
response bounds, exact decimals, secret echoes and immutable response copies.
They establish neither live provider compatibility nor recovery settlement.

Official documentation consulted on 2 November 2026:

- [Generation metadata](https://openrouter.ai/docs/api/api-reference/generations/get-request-&-usage-metadata-for-a-generation)
- [Usage accounting](https://openrouter.ai/docs/cookbook/administration/usage-accounting)
- [Credit and billing systems](https://openrouter.ai/docs/faq)

Those references describe usage and account charges. This reader does not infer
immutable invoice finality, a provider correction sequence or a retention
guarantee from the presence of a cost field.

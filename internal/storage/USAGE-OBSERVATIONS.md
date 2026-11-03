# Retaining recovery usage observations

This internal storage component retains a provider metadata response together
with the archived request identity and the current host policy that allowed its
acquisition. It is not a supported recovery command or a final billing decision.

`AcquireRecoveryUsage` derives the request from authenticated original/captured
archives and the held target, then performs one bounded read through the current
host usage authority. It holds no database gate during the HTTP request.

`RecordRecoveryUsage` accepts that opaque acquisition and a caller-retained
operation ID. It does not contact the provider. It rechecks the archives, target,
accounting baseline and current host policy before inserting an immutable row.
The host policy lock is acquired before the database recovery gate and remains
held through commit. Policy rotation, revocation, expiry or a changed accounting
head prevents a new insertion from an old acquisition.

The row retains exact response bytes, decimal representation and byte hashes,
plus separate source-plan, acquisition and admission commitments. The database
checks required identities and cross-references, assigns the recorded time and
refuses updates, deletion and truncation. The coordinator additionally checks
canonical representation, acquisition metadata and archive provenance. Database
owners and the host operator remain trusted; a digest is not a provider signature.

## Duplicate and uncertain writes

Retain the operation ID and acquisition receipt digest before calling record.
An exact repeated acquisition returns `already_recorded` and the original
operation ID, even if the caller proposes a different ID. Reusing an operation
for a different acquisition is refused. A failed cleanup returns no success.
Duplicate recording still requires the current acquisition policy to be valid;
use historical inspection when that policy has expired or been revoked.

A lost commit acknowledgement returns `ErrRecoveryUsageUnknown` with no success
record. Call `InspectRecoveryUsage` with the original operation ID, acquisition
digest and exact archive selection. Inspection reports `recorded` or
`not_recorded` after validating the held target and rolling back its read
transaction. It performs no HTTP request and requires no current provider key.
Historical inspection remains possible after revocation and subsequent valid
accounting changes. An inspection error is not evidence that no row exists.

## Financial and release limits

Every stored observation remains `observed_unverified`. Recording changes no
business rows, ledger entries, reservations or execution permissions. Fractional
micro-USD values and BYOK remain explicit; no rounding, settlement or upstream
invoice is inferred. Missing request IDs and missing provider records do not
prove absence of a charge. This component provides neither recovery-window
coverage nor permission to resume execution.

Tests use disposable PostgreSQL and an owned HTTPS peer inside the CI container.
The container maps the fixed provider hostname to loopback and uses a test CA;
production endpoint selection and TLS verification are unchanged. The checks
cover exact persistence, duplicate/conflicting operations, lost commit
acknowledgements and historical inspection through a newly opened connection
pool. A new pool is not a database restart. These tests do not establish live
OpenRouter compatibility, full restore, upgrade acceptance or release readiness.

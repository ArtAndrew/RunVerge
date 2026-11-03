# Current authority for provider usage reads

This internal component permits a bounded OpenRouter metadata read under an
explicit current host policy. It is separate from recovery-read bearer tokens.
The development [host commands](../../docs/RECOVERY-USAGE.md) expose this policy
without a Workspace or HTTP entrypoint. It does not settle costs or resume tasks.

## Policy and credential

`InitUsageHost` creates a new private authority directory. Its policy binds the
target installation, restore and original snapshot to one source installation,
organization and exact archived model connection. It includes an explicit
credential reference, fingerprint, expiry and enabled status. The fingerprint
is computed from the supplied current credential; restored credentials are not
loaded from the database or environment.

The mapping is a **current host operator attestation**. Neither a local account
label nor a successful metadata read proves independent provider account
ownership. Provision the directory outside archives and the restored database,
under the same trusted owner-only filesystem conditions as recovery-read
authority. The durable implementation supports macOS and Linux; other platforms
refuse these operations.

The private state contains either a read-authority signing seed or a usage
credential, never both. Purpose changes and mixed states are refused. Existing
read-authority state retains its original JSON encoding. A usage policy cannot
issue a bearer token or enter an ordinary recovery-read callback.

## Rotation, revocation and interruption

`ReplaceUsageHost` requires the expected current epoch. It takes the same
exclusive operating-system lock as other authority changes, records a durable
pending intent, and replaces state. Acquisitions hold a shared lock throughout
their bounded request. Replacement waits for them; a timeout or other failure
does not prove that revocation happened.

Disabling a policy removes its credential while retaining its public reference
and fingerprint. Current and previous keys are rejected if accidentally included
in public metadata labels. Inspection returns metadata, not the stored key.
This protects against accidental echoes, not arbitrary secret encodings or a
hostile trusted operator.

Use `InspectHost` after an uncertain outcome. A pending change blocks acquisition;
`ResolveHost` completes only the exact pending operation. The inspection state
`active` means the configuration has no pending change; permission also requires
an enabled policy and an unexpired timestamp at acquisition time.

## Acquisition and its limits

`Store.AcquireRecoveryUsage` first derives the request from authenticated original
and captured history using `PlanRecoveryUsageReadback`. The caller supplies local
organization/reservation IDs, not a provider generation ID, model or amount.
Only a request with an available generation identity reaches current authority.

`ObserveHostUsage` reloads current private policy, verifies scope and expiry, and
uses the fixed OpenRouter reader. It exposes no credential callback or endpoint
override. Cancellation and expiry are checked again before returning success.
The returned generation and model must match exactly; model aliases are not
assumed equivalent.

The opaque acquisition retains copied response bytes and a receipt containing
the source-plan digest, request binding, policy epoch/hash/operation, operator
attestation and observation metadata. Accessors return copies. An editable
receipt is not authority. The lower-level `ObserveHostUsage` accepts a trusted
caller-derived request: its opaque result alone does not prove the request came
from an archive.

Network I/O holds no database recovery lock. The checked target accounting head
may change during the read. Durable ingestion must independently rederive the
source binding and revalidate the target and current policy. The separate
[storage coordinator](../storage/USAGE-OBSERVATIONS.md) does that while
`WithUsageAdmission` holds the shared policy lock through commit. It persists
unverified observations; neither component establishes invoice finality or
complete recovery-window coverage.

## Checks

Tests use synthetic credentials and observations to exercise purpose separation,
legacy state decoding, scope denial, key rotation in fresh processes, revocation,
pending changes, lock draining, cancellation, expiry and exact response identity.
The gateway tests exercise the production HTTP transport against owned local
HTTPS peers. The storage integration checks cover both disabled-authority
refusal and successful owned-HTTPS acquisition through durable storage,
including duplicate/conflicting writes and inspection after commit loss.
Admission tests also check revocation, callback cancellation and lock draining.
The storage drill also performs a normal restart of the owned PostgreSQL service
and inspects the retained observation without another HTTP request. These checks
do not establish live provider compatibility, crash durability, financial
settlement or full recovery.

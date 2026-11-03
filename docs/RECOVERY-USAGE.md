# Inspect provider usage during recovery

Development component, separate from the supported alpha.6 download. These host
commands help an operator retain a provider's usage response for one archived
model request. They do not run a model, settle a bill, release a reservation, or
enable execution on the restored installation. Full restoration is still pending.

Use this after importing into a held target and ingesting the authenticated
capture pair described in [recovery evidence](RECOVERY-EVIDENCE.md). The database
operator and private host directories are trusted. The current provider key must
come from the operator, separately from the archives.

## Prepare and review

Use a private directory owned by your OS account with mode `0700`. Files must have
mode `0600`, have one link, and remain outside both archive directories. Substitute
your own absolute paths and original organization/reservation IDs:

```sh
platform recovery usage-plan \
  --source-intent-file /private/runverge/capture-plan.json \
  --target-config /private/runverge/target.json \
  --organization-id ORIGINAL_ORGANIZATION \
  --reservation-id ORIGINAL_RESERVATION \
  --out /private/runverge/usage-plan.json
```

The plan identifies the original request from verified history. You cannot supply
a replacement provider request ID, model, account or cost. Only
`request_identity_available` can proceed. Missing IDs and unsupported provider
profiles remain unresolved. Saving a plan does not contact the provider.

## Configure current access

Check that the current key belongs to the account corresponding to the archived
connection. This is your attestation; RunVerge does not turn it into independent
proof of provider account ownership. Put the key in a private file; never pass its
value on the command line. Choose a future UTC expiry and non-secret references.

```sh
platform recovery usage-access-init \
  --intent-file /private/runverge/usage-plan.json \
  --authority-dir /private/runverge/usage-authority \
  --key-file /private/runverge/current-provider-key \
  --credential-reference CURRENT_KEY_LABEL \
  --operator-reference ACCOUNT_CHECK_REFERENCE \
  --expires-at FUTURE_UTC_TIMESTAMP \
  --confirm-current-account
```

The authority directory must be a new child of an existing private directory.
RunVerge derives the connection binding from the verified plan and stores the key
only in this private authority. `usage-access-inspect --authority-dir ...` reports
the current epoch and policy without returning the key. To rotate access, use
`usage-access-replace` with the same options plus `--expected-epoch N` from that
inspection. A changed plan requires a new review.

## Read once and retain the response

```sh
platform recovery usage-acquire \
  --intent-file /private/runverge/usage-plan.json \
  --authority-dir /private/runverge/usage-authority \
  --confirm-read-provider-usage
```

The command writes `usage-plan.json.attempt` before a provider request. It then
rebuilds the plan and requires the reviewed digest to match. The fixed OpenRouter
generation endpoint receives one GET, with normal TLS verification and no redirect
or automatic retry. Current access must still permit storage after the response.

Before the database write, `usage-plan.json.acquisition` retains the response bytes
and inspection identity privately. A failure to save that file prevents admission
to the database. Both files are private recovery material: do not publish them or
include them in issue reports.

The saved observation stays `observed_unverified`. Fractional values are retained
exactly; they are not rounded into a charge. BYOK information is retained, not
interpreted as a complete invoice. The target remains held.

## Inspect instead of repeating

```sh
platform recovery usage-inspect \
  --intent-file /private/runverge/usage-plan.json
```

Inspection reopens the archives and exact target, checks the retained operation
identity, and reads the database without contacting the provider. It needs no
current provider key and remains available after revocation. `not_recorded` means
that this observation is absent from the selected target; it does **not** mean the
original model request was free or never executed.

If the attempt or acquisition file is missing, partial or inconsistent, the
command refuses to infer an outcome. Do not delete markers to retry. Review the
failure and explicitly prepare a new plan if another provider read is needed.
Saved response JSON cannot be used to reconstruct an acquisition or admit it later.

Revoke access with `usage-access-revoke --authority-dir ... --expected-epoch N
--operator-reference REF --confirm-revoke`. If an authority update reports an
uncertain outcome, first inspect it. `usage-access-resolve --authority-dir ...
--operation-id ID` resolves only the exact pending operation reported by inspection;
it does not start another usage read.

## Verification limits

The storage component has checks using a disposable PostgreSQL database and a
local HTTPS peer with a synthetic credential. The
[host command check](https://github.com/ArtAndrew/RunVerge/actions/runs/41594113551)
passed at `c4363375f19c381d5bcebdd9924fb9d2494686b1`: initial authority creation,
stale-plan refusal before HTTP, failed response retention without a database write,
lost stdout after recording, repeated-attempt refusal, and inspection in a fresh
process after revocation and deletion of the input key. It also refused a changed
target selector. Child processes dispatch the production handlers from the test
binary; this is not an installed-package walkthrough. The storage check separately
performed a normal PostgreSQL restart. A live OpenRouter account, final billing,
full restore and safe resumption of new work remain separate requirements.

# Inspect provider usage during recovery

Development component, separate from the supported alpha.6 download. These host
commands help an operator retain a provider's usage response for one archived
model request. They do not run a model, settle a bill, release a reservation, or
enable execution on the restored installation. Full restoration is still pending.

For a held target imported from the earlier archive, first ingest the
authenticated capture pair described in [recovery evidence](RECOVERY-EVIDENCE.md).
For an activated handoff, use the saved source intent and exact target configuration
from that handoff. The late archive already imported on the target is the baseline.
Run these commands on the target's maintenance host with access to both archives
and the signed retirement files at their saved paths. Live source observation
takes place during handoff review and admission.

The database operator and private host directories are trusted. The current
provider key must come from the operator, separately from the archives.

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

The saved observation stays `observed_unverified`. The response retains fractional
values exactly and records BYOK information for separate billing review.
Historical execution remains held.

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

## Post a reviewed final statement

On an activated handoff, a finance reviewer can confirm an exact final USD
line from the provider's statement. Use a separate Ed25519 key for this
review. Pin its public key in a private file with `version`, `scope` and
`public_key` in that order. The public key is the 32 raw bytes in base64.
The scope fields are `target_id`, `restore_id`, `activation_id`,
`organization_id` and `provider_account`. Select these from the saved usage
plan and verified account records. Set the version to 1.

Sign the domain bytes `RunVerge final finance statement v1` followed by a NUL
byte and the compact UTF-8 JSON statement. Use fields in this order:
`version`, `scope`, `observation_id`, `acquisition_sha256`, `invoice_line`,
`document_sha256`, `amount_microusd`, `expected_revision` and, for
corrections, `correction_reason`. Use the saved usage operation ID and
acquisition digest, a stable account-wide provider line reference, the SHA-256
of the exact private bill, and a decimal string for integer micro-USD. Use
the current reservation revision from the trusted maintenance records.
Keep the original bill privately. The signed file wraps the compact statement
as `statement` and the base64 Ed25519 signature as `signature`.

```sh
platform recovery usage-invoice-post --intent-file /private/runverge/usage-plan.json \
  --statement-file /private/runverge/final-line.json \
  --finance-trust /private/runverge/finance-key.json \
  --statement-document /private/runverge/final-bill.pdf \
  --confirm-final-usd-statement
```

Use `usage-invoice-inspect` with the same files and omit the confirmation flag
to inspect an interrupted posting. An exact repeated line returns its recorded
operation. A correction requires a distinct provider line, an explicit reason
and the current reservation revision. The database retains the signed
confirmation and reviewer public key together with the accounting receipt.
The reviewer verifies the provider bill outside RunVerge.

If posting reports `quarantined`, retain the original signed files, the
quarantine ID and the reservation revision shown in that result. Once the
accounting totals have capacity, use the same four files with
`usage-invoice-resolve --quarantine-id ID --expected-revision N
--confirm-quarantine-resolution`. This applies the original signed amount.
If capacity is still insufficient, `quarantine_retained` confirms that no
resolution was recorded. The financial hold remains in effect.

For an interrupted resolution, run `usage-invoice-resolution-inspect` with
the same files, ID and revision. Omit the confirmation flag. The original
revision identifies the resolution even after a later correction.

## Verification limits

The storage component has checks using a disposable PostgreSQL database and a
local HTTPS peer with a synthetic credential. The
[host command check](https://github.com/ArtAndrew/RunVerge/actions/runs/41594113551)
passed at `c4363375f19c381d5bcebdd9924fb9d2494686b1`: initial authority creation,
stale-plan refusal before HTTP, failed response retention without a database write,
lost stdout after recording, repeated-attempt refusal, and inspection in a fresh
process after revocation and deletion of the input key. It also refused a changed
target selector. Child processes dispatch the production handlers from the test
binary. The storage check separately performed a normal PostgreSQL restart.

The later [installed Linux check](https://github.com/ArtAndrew/RunVerge/actions/runs/41599089951)
at `d52d3ef1cd8d32841746b69ca28cbd86e6dc8594` passed on amd64 and arm64. It used
`/app/bin/platform` from the same installed runtime image for plan, authority,
acquisition and inspection. A separate test-only peer answered one HTTPS request;
the runtime's endpoint and TLS checks were unchanged. Hostname mapping and the
test CA were confined to the disposable command container, without editing host
DNS or trust settings.

The observation survived lost stdout and a normal restart of the same PostgreSQL
cluster. After revocation and deletion of the input key, inspection returned the
same record. Reusing the attempt was refused and the peer saw no second request.
Independent database reads checked the exact observation row and every other
target table remained unchanged. Both temporary environments were removed.

A later local Linux ARM64 candidate used the installed commands after an
activated handoff. It retained one loopback HTTPS response, posted a separately
signed synthetic final invoice once, and verified the same record on replay and
after a second import. The provider response and invoice were both synthetic.

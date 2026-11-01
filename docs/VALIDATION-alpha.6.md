# Alpha.6 validation

The runtime and installed browser workflow were checked at revision
`34e04b448c42cd44e507b517fff014dafd4bc808` in [PR #16](https://github.com/ArtAndrew/RunVerge/pull/16).
Later changes update documentation, diagram labels and the release helper’s exact
allowlist for the two bundled team guides. The allowlist change received independent
review; existing metadata integrity/private-payload checks passed. Final package
source bindings and checksums are included with the release assets.

## Installed team workflow

[Linux amd64 model/browser run](https://github.com/ArtAndrew/RunVerge/actions/runs/41099518582)
passed with a newly installed native bundle, fresh PostgreSQL, an owned local
Ollama 0.40.11 server and pinned Qwen2.5:7b weights. No paid model API was used.

- The member created the team task through Workspace. Two model stages produced
  a draft and review. The owner opened and copied an exact saved MCP document.
- Requesting changes saved a decision without a model run or accounting change.
  The revision required explicit cost consent and made exactly one new root
  attempt with two stages. The earlier drafts and exact feedback remained saved.
- The owner accepted an exact aggregate through the browser; a separate member
  browser context saw the decision. Fresh contexts reconnected after a full
  PostgreSQL/API/worker restart and inspected the retained result and decision.
- Four real model results and one MCP read left five visible unresolved zero-value
  reservations. Zero is the declared local API tariff; hardware is not metered,
  and a response does not establish final provider billing.
- Three targeted storage regressions passed in fresh schemas, covering nested
  inputs, uncertain exposure and unchanged earlier inputs after new feedback.

The MCP peer used the upstream Go SDK 1.8.0 over TLS/SSE. Read preparation,
approval and dispatch used the API; browser coverage was viewing and copying the
saved receipt. This does not test arbitrary MCP servers, autonomous tool choice
or a browser approval flow for that read. Browsers used separate owner/member
identities and Russian labels. The actions were automated, not performed by an
independent new user. Optional editing of final prose was not part of this pilot.

## Installation, permissions and interface

[Linux amd64 and arm64 checks](https://github.com/ArtAndrew/RunVerge/actions/runs/41099454550)
passed on both native architectures: 16 native installation/runtime/restart steps
and five Compose checks per architecture. The native two-person fixture covered
owner/member permissions, unauthorized review rejection, pending team cancellation
and credential revocation across restart. These fixture checks establish protocol
behavior, not intelligent review. Owned resources were cleaned up.

The checked native source SHA-256 was
`9fdfe82d356e17249e847f00e2b6e70d2ec3420e80d8b216b4c3486ec4a668b0`.
The macOS arm64 rc.1 package built, installed into a new temporary directory and
passed offline integrity verification with both team guides included. No new
macOS PostgreSQL/runtime check is claimed for this candidate. The final package
receives a separate offline installation/integrity check and a documented source
delta from this runtime revision.

TypeScript and production Workspace builds passed during implementation, along
with the targeted existing tool/API/language checks. Earlier isolated starter
browser checks covered all seven languages, editable fields and one explicit
agent-draft request. The installed real-model browser pilot described above is
a separate, Russian-language check. Updated README diagrams were rendered in
all seven languages and checked for text outside their assigned bounds.

## Quality and deployment limits

The final writer included the supplied MCP requirements but invented Nina's email
address and sign-up promises. The reviewer missed those errors. Acceptance in this
automated test verifies the exact decision record; it does not certify the text.
See the team guide for checking source facts and manually preparing a corrected
final text before acceptance.

No comparative quality, speed or cost advantage is established. Full backup
restoration, upgrades, secure remote team deployment, SSO onboarding, mandatory
independent approval and production capacity are outside this release. Packages
remain unsigned. Internal plans, credentials and raw local reports are excluded.

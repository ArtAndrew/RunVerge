# Alpha.3 validation

The installed macOS arm64 candidate `0.14.0-alpha.3-rc.3` completed an
operator-run automated check on a fresh disposable PostgreSQL database. Its bundle
manifest SHA-256 was
`9bc66e66aa46f81f9fbbef231dbcdc32eb6c3f02c8c5147579586d1da3dc1754`.
The report was checked against the candidate's source inventory and the pinned
public catalog snapshot. Private configuration and raw reports are not published.

The check established:

- Installation, file verification, migration and Workspace assets work in that
  profile; the existing synthetic worker scenario remains functional.
- Two real `nvidia/nemotron-3.5-lightning:free` requests produced bounded text with
  exact model identity, generation IDs, usage and matching artifact digests.
- Feedback closed the old run without creating another. A new attempt without
  cost consent was refused; explicit consent admitted the second attempt.
- The first candidate remained unchanged. Acceptance referred to the exact second
  artifact. Tasks, both results, decisions, model configuration and accounting
  remained after PostgreSQL/API/worker restart. Disposable resources were removed.

The test used public synthetic text and explicitly allowed provider data
collection. All provider price filters were zero; fallbacks were disabled.
Recorded spent amount was zero, with two unresolved one-micro-USD internal holds.
This does not verify the provider's final invoice. The catalog check used a pinned
snapshot; zero-price filters remained part of each actual inference request.

Targeted gateway/runtime/CLI, credential privacy, release-payload and complete
probe-lifecycle checks passed. Independent security reviews found no blocker.
[Linux CI](https://github.com/ArtAndrew/RunVerge/actions/runs/40691352079) checked
installation, a fresh fixture database, isolated Compose services and retained
results/accounting after restart; it installed a provider profile offline and
received no model key. This does not establish real-model behavior on Linux.

The final download is separately built, installed and file-verified. Its release
summary records the exact source difference from the runtime candidate: operator
instructions and release metadata only. Execution code, migrations, model
parameters and test logic remain unchanged. See `source-binding.json` and
`verification-summary.json` in the release assets for the final hashes.

These checks do not establish answer quality, independent new-user usability,
comparative speed or cost, every OpenRouter model, paid-provider settlement,
arbitrary MCP/tool conversations, full backup/restore, upgrades, ARM/Windows,
multi-user isolation or production readiness. Restarting the same test database
is not a backup restoration test.

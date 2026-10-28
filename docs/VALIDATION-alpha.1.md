# Alpha.1 verification

This experimental release was checked by an agent. Independent human usability testing has not yet been completed. It is a local, single-owner, test-data-only profile.

## Verification scope

The PostgreSQL-backed baseline passed all11 groups, including671 Go test cases and1703 nested cases with6 expected skips. The final delta adds Russian/English UI and removes private documentation. Every runtime and SQL input was compared with that baseline; the only runtime differences redirect documentation references to distributed user guides. No authorization, accounting, dispatch or migration logic changed.

The final delta passed Go vet, the capability catalogue race checks, contracts, Python SDK, development helpers, synthetic peers, TypeScript SDK and HTTP tests, web typecheck/build and web tests. Inputs remained stable during those checks. A fresh full PostgreSQL rerun was attempted but the execution environment denied shared-memory creation; this is recorded as a verification limit, not a new full database pass.

The final native package was installed into a fresh directory and all files verified. A previously verified installed core completed real local-model attempts, explicit revision, bounded standard MCP reading and restart with retained history. The final compiled UI was checked against that disposable core.

## Actual browser workflow

An agent created and published a local-model agent, submitted a task, requested correction, checked that a new attempt required separate consent, and retained both model outputs. The small model repeated an unsupported policy addition, so the reviewer created an editable copy, corrected it against supplied facts, published it as a proposed result and accepted that exact material. This demonstrates the review workflow; it does not establish automatic factual accuracy.

Russian and English UI were checked, including translated task/review screens, localized amounts/dates, preservation of connection on language switching, preservation of unsaved Russian input, and English selection after reload. User/model/server content remains verbatim. Only these two locales are distributed.

## Release integrity

The release assets provide exact source identity, file inventory, license notices and SHA-256 values. The package is unsigned; hashes do not authenticate the publisher. Internal plans, ADRs, progress files and private development history are excluded from the public snapshot. Raw private test logs are not distributed.

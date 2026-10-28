# RunVerge alpha.3

Use a local model or connect one explicitly selected OpenRouter text model. The
native macOS arm64 package includes the setup guide and a private-key helper.
OpenRouter runs the model remotely, so this option needs no local model server
or GPU. PostgreSQL 18 remains a separate prerequisite.

- Prepare a private model profile without key access, database access or inference.
- Keep the key in the worker process. New profiles allow 2,048 output tokens,
  including reasoning; configured prices must fit the explicit call budget.
- Prepare TLS before obtaining fresh dispatch authority. The existing short
  inference-start permit, access checks and one-attempt guard remain.
- Distinguish provider shared-pool limits and exhausted output budgets with fixed
  diagnostic codes. Provider messages and credentials are not copied into reports.

The installed candidate completed two real free-model attempts, feedback, consent,
exact-artifact acceptance and a full PostgreSQL/API/worker restart. See
[validation](VALIDATION-alpha.3.md). Free-model availability can change. No paid
fallback or automatic inference retry is enabled; an uncertain outcome retains its
reservation and does not establish a final bill.

Start with [the quickstart](QUICKSTART-ALPHA.md), then [OpenRouter setup](OPENROUTER.md).
The interface remains English/Russian; API contracts remain at 0.14.0. SDKs retain
their Apache-2.0 license; server, web and original documentation retain ELv2.

This is an experimental one-owner/test-data release. Full restore, upgrades,
enterprise SSO lifecycle, provider operation and production deployment are not
accepted. The Linux amd64 source/Compose preview uses separate fixture checks;
ARM and Windows acceptance remain open. Packages are unsigned.

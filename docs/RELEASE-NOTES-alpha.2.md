# RunVerge 0.14.0-alpha.2

Package version: `0.14.0-alpha.2`. API contract remains 0.14.0. The supported profile remains macOS arm64, one local owner and test data only.

## Easier first and second tasks

- **Suggest a configured local model.** The agent form prefers a valid, explicitly configured local model with a declared zero API tariff. Metered profiles and synthetic `fixture:` or `team:` models are never selected automatically. An explicit user choice survives later catalogue pages and language changes.
- **Start from an editable task example.** An empty task form offers a summary, short instructions or a comparison of supplied facts, in Russian or English. Choosing an example fills the text and focuses the editor. It does not create a task or call a model. Existing text is preserved, including when the interface language changes.
- **Copy the exact result text.** Copy the current candidate or any retained artifact directly. Plain text, spaces, line breaks and Unicode are preserved; JSON content is copied as its original text. The action does not accept a result, create a new attempt or change the task. If clipboard access fails, the interface suggests manual selection or download.

The project overview also has three infographics and README translations in English, Russian, Spanish, Brazilian Portuguese, German, French and Simplified Chinese. The application interface still supports only Russian and English; the shared diagrams use English labels with translated descriptions in the READMEs.

## Verification scope

Type checking, the production web build and all 99 existing web unit tests passed. Agent-led browser checks covered local-model selection and preservation of explicit choices, editable examples without task submission, language-switch preservation, and clipboard contents matching the current and previous synthetic artifacts exactly. The clipboard check used a read-only synthetic API fixture, not a new PostgreSQL or real-model run.

The native RC1 passed all 15 package checks when run by the owner outside the restricted tool environment: fresh installation, new disposable PostgreSQL database, migrations, fixture execution, server/worker/database restart, retained result and ledger, and final file verification. Cleanup completed. Earlier attempts inside the tool environment failed during database initialization and are not counted as passes.

The final package uses the same source inventory and web files as RC1; only the package version label changes. It is separately inspected, installed and file-verified. Server, API, migrations and SDK source are unchanged from alpha.1. This release does not add a new real-model quality benchmark or independent new-user acceptance; the runtime check uses synthetic execution.

## Installation and limits

Use a new installation and a new dedicated test database following [the quickstart](QUICKSTART-ALPHA.md). This release does not introduce a supported upgrade of an existing database. PostgreSQL 18, the local model server and model weights remain separate prerequisites.

The package is unsigned. Published checksums verify consistency with their values, not publisher identity. Keep important results outside the platform.

No new production, multi-user, SSO, provider, recovery or general MCP support is claimed. Budgets and explicit retry consent remain in force. Unknown sent outcomes are not automatically resent. A declared zero local API tariff does not measure hardware or electricity costs. Model output still requires factual review. See [the supported profile](SUPPORT.md) and [alpha.1 verification](VALIDATION-alpha.1.md) for the retained baseline and its limits.

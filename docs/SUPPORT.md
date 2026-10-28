# Supported alpha profile

The alpha.5 package targets macOS arm64, one owner and test data. Start with QUICKSTART-ALPHA.md. Choose a local model or the explicit OPENROUTER.md text profile: create a task, review its draft, approve a revision separately and keep both results.

Application deployment and model hosting are separate choices. OpenRouter's fixed `nvidia/nemotron-3.5-lightning:free` model completed a two-attempt review/revision workflow through an installed alpha.3 macOS candidate; results and accounting survived a full PostgreSQL/API/worker restart. This is a bounded text profile, not verification of every OpenRouter model, tool conversation, factual answer or final bill. Direct DeepSeek, Nebius and Z.ai configurations and native Windows/Linux packages are not part of this release. The optional private-key setup helper needs Python 3; the application itself does not.

There is no supported full backup/restore or upgrade path. Organization-wide deployment, provider lifecycle, production load guarantees and the complete enterprise login lifecycle have not been accepted for this alpha. These limits are also shown by the read-only capabilities catalogue. Use CAPABILITIES.md and the public API contracts to inspect supported operations.

Internal plans, ADRs, development progress and private review records are not part of this public repository. Report installation or workflow problems through the GitHub issue templates without credentials or private documents.

A separate [Linux amd64/arm64 server source-build preview](LINUX-SERVER.md) is available. Its native installation/fixture/restart and Compose separate-worker/result-retention checks passed on both architectures in [CI](https://github.com/ArtAndrew/RunVerge/actions/runs/40731840153). CI also installs the OpenRouter profile offline; it receives no real model key. Use a fresh test-data installation. Windows, real-provider acceptance on Linux, upgrades and backup/restore are not covered by that check. See [alpha.5 validation](VALIDATION-alpha.5.md) and the release verification summary for source bindings and limits.

Alpha.5 adds [reusing completed tasks and comparing saved results](REPEAT-WORK.md). These are Workspace actions using the existing task/artifact API. They do not add team access, automatic fact checking or a new model provider.

# Supported alpha profile

The published alpha.2 package is checked for macOS arm64, one owner and test data. Start with QUICKSTART-ALPHA.md. The guide uses a local model: create a task, review its draft, approve a revision separately and keep both results.

Application deployment and model hosting are separate choices. The engine includes a configurable HTTPS Chat Completions adapter. Named-provider configurations have not been accepted for this release; a compatible-looking API is not a provider interoperability test. Windows and Linux packages are not distributed as supported installations yet. These are the current release boundaries, not a requirement that all future installations or models run locally on ARM.

There is no supported full backup/restore or upgrade path. Organization-wide deployment, provider lifecycle, production load guarantees and the complete enterprise login lifecycle have not been accepted for this alpha. These limits are also shown by the read-only capabilities catalogue. Use CAPABILITIES.md and the public API contracts to inspect supported operations.

Internal plans, ADRs, development progress and private review records are not part of this public repository. Report installation or workflow problems through the GitHub issue templates without credentials or private documents.

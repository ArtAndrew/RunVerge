# Supported alpha profile

Alpha.6 is an experimental test-data release. The native download targets macOS arm64; Linux amd64 and arm64 have a separate source/Compose preview. PostgreSQL 18 and a model service are configured separately.

Start with one owner and the [quickstart](QUICKSTART-ALPHA.md). You can also try a [local two-person workflow](TWO-PERSON-PILOT.md): an operator issues a separate credential for a member, the member starts a task, and the owner reviews the exact result. Use separate browser profiles on the same machine. This is not a secure remote team deployment, SSO onboarding or an email invitation flow. Members can read organization tasks and accept their own work; independent approval is not mandatory.

The [team starter](TEAM-REVIEW.md) creates an editable draft-and-review workflow using explicitly chosen published agents. Both results remain available. Current revision feedback reaches each new stage; a new attempt needs explicit cost consent. An agent review can miss invented facts and is not human acceptance.

Local models and the existing [OpenRouter text profile](OPENROUTER.md) are available. OpenRouter's fixed `nvidia/nemotron-3.5-lightning:free` model completed a two-attempt workflow through an installed alpha.3 macOS candidate. Alpha.6's installed Linux amd64 browser workflow used Qwen2.5:7b with no paid API; its draft still contained unsupported claims. These are bounded checks, not verification of every model, factual quality, final billing or OpenRouter on Linux. Direct DeepSeek, Nebius and Z.ai connections and native Windows/Linux download packages remain outside this release.

A trusted [MCP text-reading example](MCP-READ.md) supports explicit preparation, approval and dispatch. Its saved text can be copied into an explicitly authorized revision. Arbitrary MCP servers, OAuth and autonomous tool selection are not covered by this example. A read approval does not authorize disclosure to a remote model.

There is no supported full backup/restore or upgrade path. Use new installations and synthetic data; keep needed results separately. Organization-wide production deployment, provider operations, enterprise login lifecycle and production load guarantees are not accepted. The API's read-only capabilities catalogue describes protocol capabilities, not the full release support matrix.

See [alpha.6 validation](VALIDATION-alpha.6.md) for exact evidence and limits. The [Linux source-build preview](LINUX-SERVER.md) passed native installation, separate-worker execution, two-person fixture checks and restart on amd64 and arm64. Packages are unsigned; checksums establish consistency with the published values, not publisher identity.

Internal plans, ADRs, private settings and raw local test reports are excluded from the public repository. Report a small sanitized example through the issue templates without credentials, database URLs or private documents.

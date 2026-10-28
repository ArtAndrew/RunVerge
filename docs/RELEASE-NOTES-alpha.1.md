# RunVerge 0.14.0-alpha.1

Experimental local preview for one owner on macOS arm64, using test data. The main workflow is supplied text → draft → human feedback → explicitly approved new attempt → retained artifacts. API contract version: 0.14.0.

## Interface languages

Russian and English UI, browser-language detection, a persistent explicit choice and locale-aware dates/amounts. Language switching preserves the connection and ongoing work. User/model/server content is retained verbatim. Additional locale dictionaries are not yet distributed.

## Getting started

PostgreSQL 18 and Ollama must already be installed and managed by the operator. Download the native package from the release page, check its archive SHA-256, then follow [the quickstart](QUICKSTART-ALPHA.md) and [the short example](../examples/alpha-first-task.md). The package includes the server/worker/CLI, compiled Workspace, schema identity, component inventory and license notices. PostgreSQL and model weights are not included.

The alpha is unsigned. Published SHA-256 values establish consistency with those values, not publisher identity. The inventory is not a standard SPDX/CycloneDX SBOM.

## Known limitations

- Only macOS arm64, a single local owner and test data are in the supported alpha profile. Do not expose the listener as a public multi-user service.
- No supported backup/restore, database upgrade/downgrade or automatic service lifecycle. Export important results yourself.
- Local model answers may omit facts or hit the configured output limit. Human review is required.
- Each draft is a separate artifact; repeated filenames and artifact version 1 are expected. Use IDs and times, or rename downloaded copies. No side-by-side comparison is provided.
- The optional MCP example is a bounded text-reading integration. General server compatibility, OAuth/media and autonomous tool selection by the real model are not claimed.
- Enterprise SSO lifecycle, team workflow acceptance, provider operations, complete webhook surfaces and production load guarantees remain future work.
- A declared local zero API tariff does not measure hardware or energy costs. Unknown operation count remains visible even when its amount is zero.

Release acceptance must be recorded against the final public source revision and exact archive. Historical test reports in the source tree are not a substitute for that record or for an independent human walkthrough.

Please [report feedback](https://github.com/ArtAndrew/runverge/issues/new) using only sanitized examples. The supported profile is described in SUPPORT.md.

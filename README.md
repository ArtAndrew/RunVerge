# RunVerge

**A local workspace for reviewing and revising AI-generated drafts.**

RunVerge connects a task, its generated result, human feedback, a separately approved new attempt and the retained history. The first alpha is for developers and technical leads who want to try that workflow with a local model.

**Status: experimental alpha.1.** The release workflow is checked by an agent; independent user testing is still to follow.

**0.14.0-alpha.1 is experimental: macOS arm64, one local owner, test data only.** PostgreSQL 18 and Ollama are separate prerequisites. Keep important results outside the platform. This release has no supported backup/restore or upgrade path, and must not be exposed as a public multi-user service.

## Try one useful task

1. Follow the [quickstart](docs/QUICKSTART-ALPHA.md) to install the native package and configure a local model.
2. Use the [small first task](examples/alpha-first-task.md): summarize supplied facts, review the output and request one correction.
3. Keep both attempts, then decide whether the result is useful. The model's response is a candidate for human review.

Download candidates from [GitHub Releases](https://github.com/ArtAndrew/runverge/releases). Check the release's exact platform, limitations and archive SHA-256 before running it. The alpha has no publisher signature; a checksum verifies consistency with the published value, not publisher identity.

## What this alpha focuses on

- A browser Workspace with tasks, generated artifacts, explicit feedback and a new attempt with separate cost consent.
- Durable PostgreSQL state and retained attempts across ordinary restarts.
- Budget reservations and separate tracking of uncertain outcomes, including a declared local zero API tariff. Hardware cost is not measured.
- API, CLI and Python/TypeScript SDK interfaces using the same contracts.
- An optional, narrowly configured [MCP text-reading example](docs/MCP-READ.md).

RunVerge packages the task-and-review workflow as an application with persistent state and operator controls. The broader organizational platform is a roadmap: this alpha does not establish better model quality, speed, security or cost than other frameworks. There are no comparative benchmarks.

## How it relates to other tools

| Project | Main focus | RunVerge alpha focus |
|---|---|---|
| [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) | Extensible plugin-based harness and Web UI | A packaged task, review and retained-result workflow |
| [Deep Agents](https://docs.langchain.com/oss/python/deepagents/overview) | Planning, subagents and context management | Human feedback, explicit new attempts and operator controls |
| [LangGraph](https://docs.langchain.com/oss/python/langgraph/overview) | Stateful orchestration runtime | An application with a Workspace, API and durable task records |

These are overlapping layers, not interchangeable products. The LangChain ecosystem also offers managed platform products. RunVerge does not claim feature, quality, speed or security superiority over those ecosystems.

## Interface languages

Russian and English are available from the top-bar language selector. The choice persists locally without signing out; user and model content remains in its original language. See [language behavior](docs/LANGUAGES.md).

## Current limits

The supported first scenario is one owner reviewing a document draft. A small local model can omit facts or truncate an answer. Full recovery, upgrades, team usability, enterprise SSO lifecycle, provider deployment, arbitrary MCP servers/OAuth, and production performance are outside alpha.1 acceptance. Existing internal components for later features do not make those features supported.

Two generated attempts are separate artifacts. They can both be named `model-candidate.txt` and each have artifact version 1; use their IDs and creation times to distinguish them. There is no side-by-side comparison view.

## Feedback

[Open an issue](https://github.com/ArtAndrew/runverge/issues/new) with your operating system, release version, the step that failed and a small sanitized example. Never attach tokens, private configuration, database URLs or private documents. Tell us whether you reached a useful result, what you had to correct and whether you would use it for a second task.

## Documentation

- [Quickstart](docs/QUICKSTART-ALPHA.md) · [first task](examples/alpha-first-task.md)
- [Native installation](docs/NATIVE-BUNDLE.md) · [local model configuration](docs/LOCAL-MODELS.md)
- [Release notes](docs/RELEASE-NOTES-alpha.1.md) · [supported profile](docs/SUPPORT.md)
- [Development](docs/DEVELOPMENT.md) · [API contracts](contracts/README.md)
- [Public source contents](docs/PUBLIC-SOURCE.md) · [license boundaries](docs/LICENSING.md)

## Technology and licenses

Go server and workers, PostgreSQL durable state, React/TypeScript Workspace, Python and TypeScript SDKs. The command remains `platform` and internal module names remain `agent-platform` for compatibility.

Original server, CLI, web application and repository documentation use [Elastic License 2.0](LICENSE). Client SDKs and examples use Apache-2.0 under their included license files. Dependencies retain their own licenses. ELv2 is source-available and is not presented as an OSI open-source license.

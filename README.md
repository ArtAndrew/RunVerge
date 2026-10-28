# RunVerge

**Give agents tasks, review their answers and keep the revision history.**

[English](README.md) · [Русский](README.ru.md) · [Español](README.es.md) · [Português (Brasil)](README.pt-BR.md) · [Deutsch](README.de.md) · [Français](README.fr.md) · [简体中文](README.zh-CN.md)

![Describe a task, get a draft, check it and accept it or approve a revision. Original results and feedback stay in the task history.](docs/images/workflow.svg)

RunVerge is an application for working with AI agents. Describe a task and its spending limit, get a draft, leave comments and decide whether to accept the answer or start another attempt. The original answer stays available after a revision.

[**Try alpha.5 →**](docs/QUICKSTART-ALPHA.md) · [**Download →**](https://github.com/ArtAndrew/RunVerge/releases/tag/0.14.0-alpha.5) · [**First task →**](examples/alpha-first-task.md) · [**Give feedback →**](https://github.com/ArtAndrew/RunVerge/issues/new/choose)

> **Experimental alpha.5:** macOS arm64 · one local owner · test data only. PostgreSQL 18 is configured separately. Use a local model or OpenRouter. Full restore, upgrades and production organization-wide deployment are not supported by this release. [Supported scope](docs/SUPPORT.md).

## Where RunVerge runs and which model it uses

RunVerge and the model are separate services. RunVerge keeps tasks and results; the model receives a request and generates an answer. The engine includes an administrator-configured HTTPS Chat Completions connection.

The alpha.5 package runs on macOS arm64 and includes local-model setup plus an explicit [OpenRouter text profile](docs/OPENROUTER.md). Choose one model, configure its key privately and review the returned draft. OpenRouter models do not run on your computer. DeepSeek, Nebius Token Factory, Z.ai and native Windows/Linux packages are outside this release. A single checked model does not establish compatibility with every provider.

[Try OpenRouter](https://openrouter.ai/) · [Connect it to RunVerge](docs/OPENROUTER.md).

A [Linux server source-build preview](docs/LINUX-SERVER.md) is now available on `main` for fresh installations with test data. It has passed Linux amd64 and arm64 installation, separate-worker execution and restart checks. It is separate from the alpha.5 download. The preview guide is currently in English.

The application host and model provider are separate choices. The current macOS package does not mean that the product must only use local models.

## Who should try it?

| You are… | You want… | What you can try today |
|---|---|---|
| A developer prototyping an AI application | A review workflow around generated work | Create an agent and task; inspect its draft, feedback and attempts through the UI, API or SDKs |
| A technical lead evaluating an agent workflow | A clear record of what was produced and decided | Follow the task, original artifacts, human decisions and accounting state in one local workspace |
| An early user working with a developer | Help drafting short documents from supplied facts | Summarize a text, draft an instruction or compare two supplied options, then review and revise |

The supported alpha installation has **one owner**. The audience above does not imply a shared team service or a production deployment.

## What you get out of the box

| A common problem | RunVerge includes | Practical benefit |
|---|---|---|
| A model replies, but you still need an application for reviewing its work | Task page, candidate result, feedback and accept/reject actions | Try the complete draft-and-review loop in one application |
| A correction loses the connection to the original answer | Separate attempts and retained artifacts | Inspect both results and keep the original |
| Feedback accidentally becomes another model run | Feedback pauses the task; a new attempt needs explicit consent | Decide when to spend another attempt |
| A failed or uncertain request makes the cost hard to interpret | Reservations, confirmed costs and unresolved outcomes are separate | See what needs reconciliation instead of treating an uncertain result as free |
| You want to start with your own local model | A configured local-model path and a native application package | Try the workflow without a paid model API; hardware costs still apply |
| The UI and your integration need the same behavior | Public contracts, CLI, Python SDK and TypeScript SDK | Use the same task-and-review operations from an application or script |

Use RunVerge when you want task pages with saved drafts, review decisions and cost records. A general agent harness is a different starting point: it gives you more control over how the agent itself works.

## How it compares with other agent tools

![LangChain, LangGraph, Deep Agents and DeepSeek Harness help build or customize agents. RunVerge provides task pages, review decisions, saved results and budgets. The capabilities overlap.](docs/images/starting-points.svg)

| Project | Its main starting point | Choose it when… | RunVerge's different emphasis |
|---|---|---|---|
| [LangChain](https://docs.langchain.com/oss/python/langchain/overview) | Configurable agent loops, model/tool integrations and middleware | You want to compose agent behavior inside your application | A supplied local application for the task → draft → review → decision loop |
| [LangGraph](https://docs.langchain.com/oss/python/langgraph/overview) | Stateful orchestration, durable execution and human intervention | You need custom graphs mixing deterministic and model-driven steps | An assembled task model, Workspace and review/accounting records |
| [Deep Agents](https://docs.langchain.com/oss/python/deepagents/overview) | A harness for planning, subagents, filesystem context and memory | You need those capabilities as agent-building primitives | The user's review and revision workflow around a retained result |
| [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) | Extensible plugin-based harness with a Web UI | You want to customize the harness through its plugin architecture | An opinionated task, human decision, attempt and budget workflow |
| **RunVerge alpha.5** | **A local task-and-review application** | **You want to evaluate that workflow with one owner and test data** | **UI + durable records + explicit revision consent + accounting controls** |

Comparison describes the linked projects' public overviews, reviewed on 28 October 2026. It compares starting points, not features other projects lack. LangGraph and Deep Agents also support persistence and human intervention; the LangChain ecosystem offers managed deployment products. RunVerge has no comparative evidence of better model quality, speed, security or total cost.

## How work stays under control

![Before a run, check permissions, budget and limits. Reserve budget using configured prices. Confirm the recorded cost or keep the reservation when the request outcome is unknown. A reservation is an internal task-budget record, not a bank hold.](docs/images/execution-controls.svg)

- **Review and execution are separate.** Requesting changes records feedback; starting another attempt is a separate action with cost consent.
- **Attempts have bounds.** Task budgets, attempt counts and deadlines constrain admission to more work. These controls do not guarantee factual accuracy or a provider's final bill.
- **Uncertain outcomes stay visible.** An uncertain sent operation keeps its reservation and is not blindly resent.
- **Local zero API tariff is explicit.** It does not meter CPU/GPU, electricity or hardware depreciation, and does not make paid tools free.
- **MCP is included in a bounded form.** The alpha offers a trusted, explicitly configured [text-reading example](docs/MCP-READ.md), with separate permissions and execution approval. Arbitrary servers, OAuth and a general connector marketplace are outside this release.

## Try one useful task

1. Follow the [quickstart](docs/QUICKSTART-ALPHA.md): install the native package and configure PostgreSQL and a local model.
2. Use the [first task](examples/alpha-first-task.md): summarize five supplied facts into a short draft.
3. Check the facts. Record a correction, then explicitly approve a new attempt.
4. Inspect both artifacts and choose what to accept. Tell us whether you would return with a second task.

**Alpha.5 supports seven interface languages:** English, Russian, Spanish, Brazilian Portuguese, German, French and Simplified Chinese.

In alpha.5, the first-task guide walks through **save an agent draft → publish a version → create a task and review its answer**. Optional source-text instructions and task examples fill editable fields only after you choose them. They do not select a model, publish an agent or start work automatically.

Choose the interface language in the top bar. The choice is stored on this device; switching keeps the connection and current work. Existing task text, model output and materials stay unchanged. Amounts remain exact USD values, not converted currencies. [Language selection, local assets and fallback behavior](docs/LANGUAGES.md).

Downloads and exact SHA-256 values are on [GitHub Releases](https://github.com/ArtAndrew/RunVerge/releases). The alpha has no publisher signature; a checksum checks consistency with the published value, not publisher identity.

## Know the current limits

Alpha.3's release workflow was checked by an agent; independent new-user testing is still to follow. A small local model can omit facts or truncate an answer. Keep important results outside the platform.

Full recovery, upgrades, team usability, enterprise SSO lifecycle, provider deployment, arbitrary MCP integrations and production performance are outside alpha.5 acceptance. Do not expose this installation as a public multi-user service. Source components for additional features do not make them supported.

Two attempts are separate artifacts and may share the name `model-candidate.txt` and artifact version 1. Use their IDs and creation times to distinguish them. Use **Compare results** to view two exact versions and their feedback. Use **Use for a new task** on a completed task to repeat the work with new inputs. [Workflow guide](docs/REPEAT-WORK.md).

## Feedback and documentation

[Open a feedback or bug report](https://github.com/ArtAndrew/RunVerge/issues/new/choose) with your OS, release version, the difficult step and a small sanitized example. Do not attach tokens, private configuration, database URLs or private documents.

- [Quickstart](docs/QUICKSTART-ALPHA.md) · [first task](examples/alpha-first-task.md)
- [Native installation](docs/NATIVE-BUNDLE.md) · [local models](docs/LOCAL-MODELS.md) · [MCP reading](docs/MCP-READ.md)
- [Release notes](docs/RELEASE-NOTES-alpha.5.md) · [supported profile](docs/SUPPORT.md) · [validation](docs/VALIDATION-alpha.5.md)
- [Development](docs/DEVELOPMENT.md) · [API contracts](contracts/README.md) · [source contents](docs/PUBLIC-SOURCE.md)

## Technology and licenses

Go server and workers, PostgreSQL durable state, React/TypeScript Workspace, Python and TypeScript SDKs. The CLI command remains `platform` and internal module names remain `agent-platform` for compatibility.

Original server, CLI, web application, documentation and diagrams use [Elastic License 2.0](LICENSE). Client SDKs and examples use Apache-2.0 under their included license files. Dependencies retain their own licenses. ELv2 is source-available, not an OSI open-source license. [License boundaries](docs/LICENSING.md).

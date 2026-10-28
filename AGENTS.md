# Contributor instructions

RunVerge alpha.3 serves one local owner on macOS arm64 with test data. Read README.md and docs/SUPPORT.md before expanding scope. A separate Linux amd64/arm64 source-build preview is documented in docs/LINUX-SERVER.md; it requires an explicit container listener and test data. Full restore and enterprise/provider features are deferred; do not describe unfinished components as supported features.

- Preserve existing access checks, tenant boundaries, budget reservations, uncertain outcomes, explicit consent for a new attempt and immutable result history.
- Never replay an uncertain external effect automatically. Model output, business outcome and financial evidence are separate facts.
- Applied SQL migrations are immutable. Use a forward migration for schema changes, and coordinate public contract changes across API, CLI, SDKs and Workspace.
- Use targeted existing checks during development and one final candidate gate. Security and accounting changes require independent review.
- Use newly initialized disposable PostgreSQL instances for tests. Never read a developer's saved database configuration implicitly for a destructive test or point tests at a working installation.
- Keep secrets, private configuration, model weights, dependencies, compiled artifacts and raw local test output out of source commits and issue reports.
- Keep internal plans, ADRs, work orders and progress reports local. Before publication, review the source tree, release archives and every commit being pushed; ignore rules do not remove already tracked files or history. Publish only reviewed branches and tags, never private development history through bulk pushes.
- Original server/web code and documentation use ELv2. Client SDKs and examples use Apache-2.0; preserve the included license boundaries and upstream notices.
- Document what was actually checked and its limits. Do not equate the number of tests, lines of code or source availability with production readiness.

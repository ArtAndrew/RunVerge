# RunVerge alpha.7 — local team and recovery preview

Use RunVerge with test data to give agents work, review their drafts and keep the decisions and costs attached to each task.

- The Workspace decision queue brings review, approval and reconciliation items together.
- The local Ollama setup prepares a model profile offline. The two-person workflow supports a contributor, a reviewer and two agents for drafting and checking.
- Operators can create an encrypted backup and restore it to a separate held database to inspect saved results and costs. The recovery drill exercises that path after restart.
- The experimental Linux ARM64 provider tools can provision, suspend, resume, export and delete isolated test installations. A local operator API records requests and their outcomes over a private Unix socket.

## Download and install

Download the macOS arm64 archive and check it against SHA256SUMS. Install on a fresh PostgreSQL 18 test instance. The package is unsigned. Configure your model separately. Linux ARM64 is a source-build preview using Docker Compose.

The bundle-manifest SHA-256 for bundle inspect and bundle install is **35351bc3e9bfbee4b318e486afbb329d3a42c900799615f2c87d3958d59a7ca8**.

[Native installation](https://github.com/ArtAndrew/RunVerge/blob/0.14.0-alpha.7/docs/NATIVE-BUNDLE.md) · [Local models](https://github.com/ArtAndrew/RunVerge/blob/0.14.0-alpha.7/docs/LOCAL-MODELS.md) · [Recovery drill](https://github.com/ArtAndrew/RunVerge/blob/0.14.0-alpha.7/docs/RECOVERY-OPERATOR.md) · [Linux preview](https://github.com/ArtAndrew/RunVerge/blob/0.14.0-alpha.7/docs/LINUX-SERVER.md) · [Provider API](https://github.com/ArtAndrew/RunVerge/blob/0.14.0-alpha.7/docs/PROVIDER-API.md)

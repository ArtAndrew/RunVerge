# Alpha.4 candidate validation

Status: source candidate validation, not a published release. The checked
feature revision is `8a3a7e7bf4989179fbf0c627e2b01ec51b4ac51b` in
[PR #4](https://github.com/ArtAndrew/RunVerge/pull/4).

- TypeScript, the production Workspace build and 14 targeted language and
  accounting checks passed. All five additional dictionaries have the same
  keys, placeholders and text boundaries as the English reference.
- Interactive browser checks covered seven translated agent forms, preserved
  mixed-language input and connection state, a saved choice after reload,
  320px and 1280px layouts, and a deliberately delayed language asset.
  The standalone browser smoke script could not run in the local environment;
  its syntax was checked, and interactive checks were used instead.
- Independent review checked exact micro-USD display and language-loading
  errors, timeouts and stale responses. Two issues found during review were
  fixed and checked again.
- The macOS arm64 `0.14.0-alpha.4-rc.1` bundle built, installed into a new
  disposable directory and passed offline integrity verification. All five
  separate language assets were present. The directory was removed afterward.
- [Linux CI](https://github.com/ArtAndrew/RunVerge/actions/runs/40714381696)
  passed on native amd64 and arm64 hosts: 15 installation/runtime/restart
  checks and five separate-worker Compose checks per architecture. Both
  runs confirmed cleanup and used no real model key.

The native macOS candidate and both Linux reports bind to source digest
`c80343165fd6e1236d14dd3a8d5646fb7f42af3a775d95dce8e404ca0920292c`.
The macOS candidate bundle manifest digest is
`2251f1c16ac6e6539b0156dbd0422221a9a2834f06c43d6d2c9dc2a298cb0e60`.
A final release build must be separately identified; these hashes identify
this candidate only. Private configuration and raw local reports are excluded.

This UI change did not alter provider execution, migrations or API contracts.
No new real-model call or macOS database restart was performed for alpha.4.
The previous real OpenRouter workflow belongs to [alpha.3 validation](VALIDATION-alpha.3.md).
These checks do not establish translation quality reviewed by native speakers,
independent customer usability, demand, comparative performance, full restore,
upgrades, enterprise isolation or production readiness.

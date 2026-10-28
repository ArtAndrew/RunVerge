# Alpha.5 validation

Feature revision `1bc9479aa85744ded9ed19177e0462da5794c69c` is reviewed in
[PR #7](https://github.com/ArtAndrew/RunVerge/pull/7).

- TypeScript and the production Workspace build passed. All 106 web tests
  passed, including exact historical text binding, mismatched task/organization/
  artifact rejection and feedback association. Dictionaries have matching keys
  and placeholders in all seven supported languages.
- Interactive browser checks used an isolated synthetic API. They covered two
  artifacts sharing the same filename/version, an older artifact outside the
  first materials page, exact feedback association, equal-text reporting,
  English/Russian labels, and a 390px French layout without page overflow.
- Opening the reuse form and comparing versions sent no mutations. Confirming
  the changed source text sent one new-task request with the selected agent,
  budget, attempt limit and fresh duration. The original text and accounting
  stayed unchanged. This establishes UI/request behavior, not real model quality.
- The macOS arm64 candidate built, installed and passed offline file verification.
  Its runtime test stopped while initializing disposable PostgreSQL in the local
  tool environment; cleanup completed. A successful macOS runtime/restart test
  is not claimed for this candidate.
- [Linux CI](https://github.com/ArtAndrew/RunVerge/actions/runs/40731840153)
  passed on native amd64 and arm64: 15 native installation/runtime/restart steps
  and five separate-worker Compose checks per architecture, with cleanup.
  No real model key was supplied.

Both Linux reports and the macOS candidate bind to source SHA-256
`6e44eb5b6906dbfb82dd847dff683b582b87547ba73201b1acd542cb07ee5e55`.
The final bundle changes only two operator guides in the source inventory;
Workspace assets, helpers and notices are identical. It was separately installed
and file-verified. Its source SHA-256 is
`41616a2282ccb522736898fcc404541aaa0171b429bc6e0b8f560631decf0eab`,
and bundle manifest SHA-256 is
`1a9788408dc4e4c67d35185fbfe27dd5571eeb10403517bb516cf66b8b4d8cff`.

No server, API, migration, model gateway or accounting code changed. No new real
model call was made. Backup restoration, upgrades, production use, independent
new-user acceptance and comparative model quality are not established here.
Packages remain unsigned. Internal plans and raw local reports are excluded.

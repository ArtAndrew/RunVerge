# Repeat a task and compare results

Use this workflow when a completed task was useful and you want to do the
same kind of work with different source material. It is also useful when
reviewing what changed after a correction.

## Start with new source material

1. Open a completed top-level task and choose **Use for a new task**.
2. Replace the old source material in **What needs to be done?**. Keep or
   change the instructions to suit the new input.
3. Check the selected agent and its published version, budget, attempt limit
   and deadline. The currently published agent may differ from the version
   used by the old task. If it is unavailable, select a published agent.
4. Confirm that you checked the inputs and limits, then choose **Create task**.

Opening the form makes no model call. Creation starts a separate task through
the normal API, with current permissions and limits. It does not retry the old
task, copy results, reset old reservations or transfer review decisions. The
time limit is new; the form defaults to 24 hours instead of copying an expired
deadline. Agent instructions remain part of the selected published agent.

## Compare two saved results

Open **Compare results** in a task with at least two candidates. Select one
result on each side. Each panel shows its exact saved text, creation time and
matching feedback or decisions. On narrow screens the panels stack vertically.
Expand **Version and checksum** for the full candidate ID, run ID and SHA-256.

Different attempts can both be named `model-candidate.txt`, version 1. The
comparison uses their artifact IDs and versions and checks the saved content
digest. It fetches the selected version even when it is outside the first page
of materials. It refuses a mismatched response. It does not edit, accept or
rerun either result.

“The texts are identical” means exact string equality, including whitespace.
“The texts differ” does not say which version is better. This release presents
the full texts alongside each other; it does not provide a word-level diff,
fact checking, automatic merging or cost attribution to individual sentences.
Decisions remain available in the existing result history; task accounting is
shown separately from the texts.

The workflow supports all seven interface languages. Your inputs, outputs and
feedback are not translated when you change the interface language.

Use test data. This workflow does not add backup restoration, upgrades or
production multi-user support. See [supported scope](SUPPORT.md).

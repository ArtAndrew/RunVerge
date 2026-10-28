<!-- SPDX-License-Identifier: Apache-2.0 -->

# First task: summarize supplied facts

Use this short example after the [quickstart](../docs/QUICKSTART-ALPHA.md). It needs no browsing, private data or external tool. Small prompts reduce the chance of hitting the local model's output limit.

In source versions after `0.14.0-alpha.1`, **New task → Start with an example** offers three editable prompts: summarize text, draft instructions and compare options. They appear only while the task text is empty. Choosing one fills the text in the current interface language; it does not create a task or call the model. Edit it, select your published agent and review the limits before choosing **Create task**. Switching the interface language preserves text already entered. The released alpha.1 package uses the manual example below.

## First request

> Summarize these facts in exactly three bullets, at most 60 words total: The library pilot starts on 12 November. Nina owns the pilot. Ten volunteers will test it. Registration closes on 8 November. The budget is 200 euros. Do not invent details.

Expected facts: 12 November start; Nina; ten volunteers; 8 November registration deadline; 200-euro budget. Check that all five facts survive and that no new claim appears. Accept a concise arrangement of these facts; formatting alone does not prove correctness.

## Feedback for a new attempt

> Revise the draft: put the registration deadline first. Keep all five facts, exactly three bullets and at most 40 words. Add no new facts.

Submit this as feedback on the exact candidate, and explicitly acknowledge the new attempt. Check the new artifact and keep the original for comparison.

In **Materials / Материалы**, both artifacts may be named `model-candidate.txt` and each have version 1. Their IDs and creation times differ. Download and rename them `draft-1.txt` and `draft-2.txt` if you want to compare locally; the application has no side-by-side comparison view.

Tell us whether the model retained all five facts, whether the correction helped and what setup step was hardest. Use only this synthetic example in public feedback.

This example uses [Apache-2.0](python/LICENSE).

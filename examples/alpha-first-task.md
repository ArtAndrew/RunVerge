<!-- SPDX-License-Identifier: Apache-2.0 -->

# First task: summarize supplied facts

Use this short example after the [quickstart](../docs/QUICKSTART-ALPHA.md). It needs no browsing, private data or external tool. Small prompts reduce the chance of hitting the local model's output limit.

## From an agent draft to a task

The **published alpha.3 package** can use the manual example below. The **current alpha.4 source candidate** adds a first-task guide and seven interface languages; it is not yet a published or accepted release.

1. **Create an agent draft.** For a single agent with empty name and instructions, you can choose **Use suggested agent instructions**. This fills editable instructions for working only from supplied text: preserve facts, flag missing information and produce a draft for review. It is optional. Choose a configured model and a budget yourself, then save.
2. **Publish a version.** Check the agent card before publishing. Publication makes that version available for tasks; it does not call the model.
3. **Create a task.** In **New task → Start with an example**, choose a summary, instructions or comparison prompt, or paste the request below. Examples appear only while the task text is empty. Choosing one fills the form without starting work. Review the text, published agent and limits, then explicitly choose **Create task**.

The guide does not configure a model or grant permissions. If you cannot create or publish an agent, ask an authorized administrator to prepare one. A test executor only checks the platform; use a configured model for a generated answer.

Suggested instructions and examples use the selected interface language when inserted. Switching languages afterward preserves entered text and existing model answers. See [language behavior](../docs/LANGUAGES.md).

## First request

> Summarize these facts in exactly three bullets, at most 60 words total: The library pilot starts on 12 November. Nina owns the pilot. Ten volunteers will test it. Registration closes on 8 November. The budget is 200 euros. Do not invent details.

Expected facts: 12 November start; Nina; ten volunteers; 8 November registration deadline; 200-euro budget. Check that all five facts survive and that no new claim appears. Accept a concise arrangement of these facts; formatting alone does not prove correctness.

## Feedback for a new attempt

> Revise the draft: put the registration deadline first. Keep all five facts, exactly three bullets and at most 40 words. Add no new facts.

Submit this as feedback on the exact candidate, and explicitly acknowledge the new attempt. Check the new artifact and keep the original for comparison.

In **Materials / Материалы**, both artifacts may be named `model-candidate.txt` and each have version 1. Their IDs and creation times differ. Download and rename them `draft-1.txt` and `draft-2.txt` if you want to compare locally; the application has no side-by-side comparison view.

Tell us whether the model retained all five facts, whether the correction helped and what setup step was hardest. Use only this synthetic example in public feedback.

This example uses [Apache-2.0](python/LICENSE).

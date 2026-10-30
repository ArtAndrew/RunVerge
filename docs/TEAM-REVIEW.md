# Draft, review, then decide

This guide describes the team starter in the development branch. It is not included in the alpha.5 download and does not establish support for a shared production service. Use synthetic materials on a disposable installation. [Current release limits](SUPPORT.md).

A review team has two stages. The first agent prepares a draft. The second compares the saved draft with the original task and its supplied materials. RunVerge keeps both outputs for a person to review. Another model response is not proof that the draft is correct.

## Create the team

1. Create and publish the agents you want to use for drafting and review. Choose their models and instructions explicitly. The `fixture:echo` executor can exercise the interface but does not write or review intelligently.
2. Open **Agents → Create agent**, select **Agent team**, then **Fill in a draft-and-review workflow**. This fills editable instructions and two member tasks. It does not create, publish or run anything.
3. Choose a published agent version for each stage. Check that each agent's instructions fit its job. Both stages may use the same model; this does not make the review independent.
4. Check the shared task limit. The starter requires both results, runs the stages in order, and allows two immediate subtasks at one level. Leave nested teams out of your first example.
5. Save the draft. Review the saved configuration, then publish it separately. Creating a task is a further, explicit action.

Changing the interface language does not translate or replace filled instructions. The starter uses the language selected when you fill it. It is offered only on an untouched team form, so applying it cannot overwrite your existing configuration.

## Try a concrete task

Use this synthetic input as the team task:

> Write a short announcement of the Atlas trial. It starts on 12 November, lasts 10 days, has 6 participants and has a budget of $40. Nina collects feedback. Include these facts and list unanswered questions. No other facts have been supplied.

Check the saved draft against those five facts. Then inspect the reviewer's comments: do they identify specific passages, and are their proposed corrections supported by the input? The reviewer can also be wrong. Neither agent should invent an outcome of the trial or claim to have checked external sources.

The result is a collection of the draft and review, not an automatically corrected third document. The final decision applies to the exact collected result. Record your decision or feedback through the existing result-review actions; a new attempt requires separate consent. Do not treat the second agent's output as human acceptance.

## Budget and stopping

The task's shared budget covers its subtasks. Each agent's configured limits and the organization's limits still apply. Two model stages can cost more and take longer than one; the starter makes no savings or quality guarantee.

Use the root task's stop action to stop the team. A request already sent to a provider cannot be assumed cancelled or free. Unresolved costs must remain visible until reconciled. [Accounting contract](../contracts/COSTS.md).

## Agent teams and human teams are separate

This starter composes agents. It does not invite colleagues or grant access. Human participants need their own identity and permissions configured through the supported administration path; never share an owner's token. End-to-end acceptance of the two-person installation and review workflow is a separate release requirement. [Membership contract](../contracts/MEMBERSHIPS.md).

For separate human logins, see the development [two-person walkthrough](TWO-PERSON-PILOT.md).

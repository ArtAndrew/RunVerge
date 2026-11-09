# CI triage pilot

These three prompts contain complete synthetic logs. The example links use a
reserved test domain. Copy one complete text file into a new task or session.
The agent can cite evidence from the prompt without opening the link.

In RunVerge, open the draft from **Needs attention**. The CI log references
panel shows which full run links point to lines supplied in the task. Expand
the panel to read those lines beside the draft. Compare them with the finding
before deciding whether to accept or request changes.

To correct the text yourself, create an editable copy and use the reference
check in the editor. If a bare line reference matches the supplied log, expand
the suggested corrections to review and add the full link. Save the corrected
text and propose it as the result.
The original stays in the task history. Accept the new version only after
reviewing its evidence. Accepting a result keeps it in RunVerge.

For a comparison, use fresh runs with the same model revision, output allowance
and task input in both products. Record the app version, configuration, effective
model settings, operator actions, elapsed time, tokens and known cost. Keep
publishing outside this trial. Use a throwaway workspace with no repository
credentials or enabled write tools. If a platform adds its own system prompt,
record that fact as part of the platform configuration.

Shuffle the three cases and alternate which product the operator uses first.
Count time spent entering inputs, inspecting the draft, correcting it and
making the decision. Track that time separately from waiting for the model.
Grade the first draft before editing and record whether the revised draft was
accepted. Keep draft text and individual scores so another reviewer can check
the result.

| Input | Accept if |
| --- | --- |
| 01-auth-regression | Identifies the observed 401 on the named test. Treats the issuer change as a hypothesis, cites the error lines and asks to compare the configured and token issuers. |
| 02-runner-disk | Identifies the full runner disk during cache restore, cites the disk or cache error lines and says tests did not run. |
| 03-ambiguous-timeout | Identifies the observed timeout, cites its line, leaves the underlying cause open and requests specific missing evidence such as process progress or timings. |

A draft fails if its cited lines do not support the finding or it invents an
observed cause. The release team can use the three samples to learn where the
workflow breaks. Three synthetic cases alone cannot establish a product-wide
advantage.

# RunVerge alpha.6 — draft and review with a small team

Create two agents: one writes a draft and the other reviews it. The new Workspace starter fills an editable two-stage workflow; you choose the published agent versions and explicitly publish the team before starting a task. The starter is available in all seven interface languages.

A contributor can start the task using their own credential. The owner can inspect the draft and review, request changes, explicitly authorize a new attempt and accept a specific saved result. Both people see the same decision history. This local walkthrough uses separate browser profiles; it does not provide SSO, email invitations or secure remote deployment. Members can read organization tasks and accept their own work: independent approval is not mandatory.

For a revision based on a document, an operator configures a trusted MCP reader. Prepare, approve and start the read explicitly. Open the returned text, copy it, and include the relevant material in the separately approved revision instructions. Copying or requesting changes does not start another model call. The agent does not automatically choose this MCP tool.

The current revision feedback now reaches each newly created team stage directly, alongside its saved parent context. Earlier inputs, drafts, reviews and accounting records stay unchanged.

Native bundles include TEAM-REVIEW.md and TWO-PERSON-PILOT.md. The application does not bundle an inference engine or model weights. The bounded Linux example uses a separately downloaded Qwen2.5:7b model; a smaller model may complete the workflow but produce poor drafts. Model review is not factual certification.

Use a fresh installation and synthetic data. Full backup restoration, upgrades, production team deployment and provider operations remain outside this release. Local zero API tariff does not measure hardware costs; unresolved cost records remain visible. Existing OpenRouter support is retained, with no new provider compatibility or billing claim.

[Team guide](TEAM-REVIEW.md) · [Separate logins](TWO-PERSON-PILOT.md) · [Validation and limits](VALIDATION-alpha.6.md)

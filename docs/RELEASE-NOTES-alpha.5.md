# RunVerge alpha.5 — repeat useful work

Alpha.5 adds two Workspace actions for work you want to repeat or review:

- **Use for a new task:** take a completed top-level task, replace its source text, check the current published agent and limits, then explicitly create a separate task. Opening the form does not run the model. Old results, decisions, reservations and costs remain with the original task.
- **Compare results:** choose two saved candidates, read their exact texts side by side and see the feedback attached to each version. The view checks artifact identity and SHA-256, including for historical artifacts outside the first page. On narrow screens the results stack vertically.

Both actions are available in English, Russian, Spanish, Brazilian Portuguese, German, French and Simplified Chinese. Your text is not translated. Comparison is not fact checking, a word-level diff or automatic approval.

[Usage guide](REPEAT-WORK.md) · [Русская инструкция](REPEAT-WORK.ru.md)

This remains an experimental, single-owner, test-data release. Full backup restoration, upgrades and production team deployment are not supported. Local models and the existing OpenRouter text profile remain available; no new model-provider compatibility is claimed. API version and database migrations are unchanged.

# Interface languages

## README translations

The project overview and all three diagrams are available in [English](../README.md), [Russian](../README.ru.md), [Spanish](../README.es.md), [Brazilian Portuguese](../README.pt-BR.md), [German](../README.de.md), [French](../README.fr.md) and [Simplified Chinese](../README.zh-CN.md). Each README uses diagrams with labels and descriptions in its language. Brand names and technical identifiers stay unchanged. These are documentation translations; the Workspace interface still supports Russian and English. Linked detailed guides may be in English or Russian. See [diagram editing instructions](images/README.md).

## Workspace interface

Workspace supports Russian and English. Use the **Interface language** selector in the top bar. The first visit follows the browser language when supported, otherwise English. Your choice is saved locally and remains after a reload. Switching language keeps the active connection, current task and unsaved form state.

Only interface labels, local validation messages and locale formatting change. Task text, model output, tool results, server messages, names and identifiers keep their original text. Ask the agent explicitly for an answer in your preferred language. A language switch does not translate existing artifacts.

Amounts retain exact integer micro-USD arithmetic; the display follows the chosen locale. Budget input continues to use the documented decimal syntax.

Additional languages use local dictionaries in `web/src/i18n/` and the supported locale registry in `web/src/i18n.ts`. A language should be offered only after its dictionary, placeholders and principal workflows have been checked. Spanish, Portuguese, German, French and Chinese are not yet shipped translations in this alpha.

# Interface languages

Alpha.4 supports English, Russian, Spanish, Brazilian Portuguese, German, French and Simplified Chinese. The older alpha.3 download includes English and Russian only.

## Choosing a language

Use **Interface language** in the top bar. A supported saved choice takes precedence; otherwise Workspace checks browser language preferences and falls back to English. Portuguese preferences use Brazilian Portuguese (`pt-BR`). Chinese support is **Simplified Chinese only**: generic Chinese and `zh-CN`, `zh-SG` or `zh-Hans` preferences select it. Traditional Chinese preferences are skipped in favor of another supported browser preference, or English.

The choice is saved in this browser’s local storage (`runverge.locale`), not in your account. If storage is unavailable, changing language still works for the current page. Switching keeps the connection, current task and unsaved form state.

English and Russian are included in the initial application bundle. The other dictionaries load on demand as local assets served with RunVerge; no external translation service receives your text. If a language asset cannot load, Workspace keeps the current language and offers an error message; a failed initial load leaves English available. Check the connection and select the language again.

## What changes

Interface labels, local validation messages, dates and number formatting follow the selected language. Task text, model output, tool results, server messages, names and identifiers remain unchanged. Existing artifacts are not translated. Ask the agent explicitly for an answer in your preferred language.

Amounts stay in **USD**, using exact integer micro-USD arithmetic (1 USD = 1,000,000 micro-USD); a locale changes presentation, not currency or recorded value. Budget input still accepts the documented decimal syntax with up to six fractional digits.

In the first-task guide, explicitly choosing suggested agent instructions or a task example fills an editable form in the selected language. This does not run a model or translate text already entered. See the [first-task example](../examples/alpha-first-task.md).

## Documentation and adding a locale

The overview and diagrams are available in [English](../README.md), [Russian](../README.ru.md), [Spanish](../README.es.md), [Brazilian Portuguese](../README.pt-BR.md), [German](../README.de.md), [French](../README.fr.md) and [Simplified Chinese](../README.zh-CN.md). Detailed linked guides may be in English or Russian. See [diagram editing instructions](images/README.md).

Dictionaries live in `web/src/i18n/`; the registry and loading rules are in `web/src/i18n.ts`. Offer another language only after checking complete keys, placeholders, spacing and principal workflows. Preserve the distinction between a reservation and a confirmed cost, approval and execution, and an unknown outcome and permission to retry. User/model content must never become translation input.

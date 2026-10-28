# RunVerge alpha.4 candidate

Status: prepared in source; no alpha.4 release has been published.
The latest download remains [alpha.3](https://github.com/ArtAndrew/RunVerge/releases/tag/0.14.0-alpha.3).

Workspace guides a new user through saving an agent draft, publishing its
version and preparing the first task. Suggested instructions help an agent
work with supplied text; examples remain editable. Choosing instructions or
an example does not call a model. If no model is configured, the agent form
links to local-model and OpenRouter setup.

The interface adds Spanish, Brazilian Portuguese, German, French and
Simplified Chinese alongside Russian and English. Additional dictionaries
load when selected. Switching preserves entered text and the current
connection. A failed or stalled translation download leaves the interface
usable. Dates and number formatting follow the language; amounts remain
exact USD values. Task content, model output and server details retain their
original text. See [language behavior](LANGUAGES.md).

[Candidate validation](VALIDATION-alpha.4.md) covers the source and build
currently checked. The API remains 0.14.0. SDKs and examples retain Apache-2.0;
server, Workspace and original documentation retain ELv2.

Use a fresh installation and test data. The alpha still targets one owner;
full backup restoration, upgrades, organization-wide production use and
provider operation have not been accepted. The native macOS candidate is
unsigned. Linux remains a separate source-build preview.

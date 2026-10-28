# RunVerge

**Ein lokaler Arbeitsbereich für KI-Aufgaben, Entwürfe, menschliche Entscheidungen und die Historie der Ausführungsversuche.**

[English](README.md) · [Русский](README.ru.md) · [Español](README.es.md) · [Português (Brasil)](README.pt-BR.md) · [Deutsch](README.de.md) · [Français](README.fr.md) · [简体中文](README.zh-CN.md)

![Aufgabe und Grenzen → Modellentwurf → menschliche Prüfung → optionaler, separat genehmigter Versuch → Entscheidung.](docs/images/workflow.svg)

**Beauftragen. Entwurf prüfen. Korrektur anfordern. Beide Versionen behalten. Über die Annahme entscheiden.** RunVerge bündelt diesen Ablauf in einer Anwendung mit Browseroberfläche, dauerhaften Aufgabenaufzeichnungen und Ausführungskontrollen. So lässt sich der Ablauf erproben, ohne zuerst eine eigene Anwendung zur Ergebnisprüfung zu entwickeln.

[**Loslegen →**](docs/QUICKSTART-ALPHA.md) · [**Alpha.1 herunterladen →**](https://github.com/ArtAndrew/RunVerge/releases/tag/0.14.0-alpha.1) · [**Erste Aufgabe →**](examples/alpha-first-task.md) · [**Feedback →**](https://github.com/ArtAndrew/RunVerge/issues/new/choose)

> **Experimentelle Alpha.1:** macOS arm64, ein lokaler Eigentümer und ausschließlich Testdaten. PostgreSQL 18 und ein lokaler Modellserver wie Ollama werden separat eingerichtet. Vollständige Wiederherstellung, Updates bestehender Installationen und organisationsweiter Produktivbetrieb werden in dieser Version nicht unterstützt. [Unterstützter Umfang](docs/SUPPORT.md).

## Für wen ist RunVerge gedacht?

| Rolle | Bedarf | Heute ausprobieren |
|---|---|---|
| Entwickler einer KI-Anwendung | Ein Prüfablauf rund um die Agentenarbeit | Agenten und Aufgaben anlegen; Entwürfe und Versuche über Oberfläche, API oder SDK prüfen |
| Technischer Leiter | Nachvollziehen, was erstellt und entschieden wurde | Originalmaterialien, Feedback, Entscheidungen und Kostenstatus verfolgen |
| Früher Anwender mit Entwicklerunterstützung | Kurze Dokumente aus vorgegebenen Fakten erstellen | Text zusammenfassen, Anweisungen entwerfen oder Optionen vergleichen und das Ergebnis prüfen |

Die Alpha-Installation unterstützt **einen Eigentümer**. Diese Zielgruppen bedeuten keinen gemeinsam genutzten Mehrbenutzerdienst.

## Praktische Vorteile

- **Zusammenhängender Ablauf:** Aufgabe, Entwurf, Feedback sowie Annahme oder Ablehnung in einer Anwendung.
- **Erhaltene Historie:** Getrennte Versuche behalten ihre Materialien; eine Korrektur ersetzt nicht das Original.
- **Bewusste Überarbeitung:** Änderungswünsche pausieren die Aufgabe. Ein weiterer Versuch erfordert eine separate Kostenzustimmung.
- **Klare Kostenaufzeichnungen:** Reservierungen, bestätigte Kosten und ungewisse Ergebnisse werden getrennt behandelt.
- **Lokales Modell:** Der Ablauf lässt sich ohne kostenpflichtige Modell-API erproben; Hardware und Strom bleiben Kostenfaktoren.
- **Integration:** API, CLI sowie Python- und TypeScript-SDKs verwenden gemeinsame öffentliche Verträge.

Der Vorteil liegt in der zusammengestellten Lösung für diesen Anwendungsfall. Der unterstützte Umfang ist noch enger als bei etablierten Agentenökosystemen.

## Vergleich mit anderen Werkzeugen

![Unterschiedliche Ausgangspunkte: Agentenverhalten zusammenstellen oder den Aufgaben- und Prüfablauf von RunVerge verwenden. Fähigkeiten überschneiden sich.](docs/images/starting-points.svg)

| Projekt | Schwerpunkt | Wann es passt |
|---|---|---|
| [LangChain](https://docs.langchain.com/oss/python/langchain/overview) | Modelle, Werkzeuge, Middleware und Agentenschleifen | Sie möchten das Agentenverhalten in Ihrer Anwendung zusammenstellen |
| [LangGraph](https://docs.langchain.com/oss/python/langgraph/overview) | Zustandsbehaftete Orchestrierung, dauerhafte Ausführung und menschliche Eingriffe | Sie benötigen eigene Graphen mit programmierten und modellgesteuerten Schritten |
| [Deep Agents](https://docs.langchain.com/oss/python/deepagents/overview) | Planung, Unteragenten, Dateikontext und Gedächtnis | Sie benötigen diese Fähigkeiten als Agentenbausteine |
| [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) | Durch Plugins erweiterbarer Harness mit Weboberfläche | Sie möchten den Harness über Plugins anpassen |
| **RunVerge alpha.1** | **Lokale Anwendung für Aufgaben und Ergebnisprüfung** | **Sie möchten diesen Ablauf mit einem Eigentümer und Testdaten erproben** |

Die verlinkten offiziellen Beschreibungen wurden am 28. Oktober 2026 geprüft. Verglichen werden Ausgangspunkte, keine angeblich fehlenden Funktionen. LangGraph und Deep Agents unterstützen ebenfalls Persistenz und menschliche Eingriffe; das LangChain-Ökosystem bietet verwaltete Bereitstellungsprodukte. Vergleichende Belege für bessere Qualität, Geschwindigkeit, Sicherheit oder geringere Gesamtkosten von RunVerge liegen nicht vor.

## Ausführungs- und Kostenkontrolle

![Vorher: Zugriff und Grenzen prüfen. Währenddessen: konfigurierte Kostenobergrenze reservieren. Danach: bestätigen oder abgleichen. Ungewisse Ergebnisse werden nicht automatisch erneut gesendet.](docs/images/execution-controls.svg)

Budget, Versuchslimit und Frist begrenzen die Zulassung weiterer Arbeit. Sie garantieren weder sachliche Richtigkeit noch die endgültige Anbieterrechnung. Bei ungewissem Ausgang einer gesendeten Operation bleibt die Reservierung erhalten; es erfolgt keine blinde Wiederholung. Ein lokaler API-Tarif von null misst weder CPU/GPU, Strom noch Abschreibung und macht kostenpflichtige Werkzeuge nicht kostenlos.

**MCP:** Ein begrenztes [Beispiel zum Textlesen](docs/MCP-READ.md) bietet eine vertrauenswürdige Konfiguration, Berechtigungen und Ausführungsfreigabe. Beliebige Server, OAuth und ein allgemeiner Konnektorenkatalog gehören nicht zu Alpha.1.

## Ausprobieren

1. Folgen Sie dem [Schnellstart](docs/QUICKSTART-ALPHA.md), um Paket, PostgreSQL und Modell einzurichten.
2. Bearbeiten Sie die [erste Aufgabe](examples/alpha-first-task.md) mit fünf Beispielfakten.
3. Prüfen Sie den Entwurf, erfassen Sie eine Korrektur und genehmigen Sie einen weiteren Versuch separat.
4. Prüfen Sie beide Materialien und entscheiden Sie über die Annahme. Würden Sie mit einer zweiten Aufgabe zurückkommen?

**Die Oberfläche von Alpha.1 unterstützt nur Englisch und Russisch.** Sieben README-Sprachen bedeuten nicht sieben Oberflächensprachen. Ein Sprachwechsel erhält Verbindung und aktuelle Arbeit; Aufgaben und Modellantworten werden nicht übersetzt. [Sprachverhalten](docs/LANGUAGES.md).

## Aktuelle Grenzen und Dokumentation

Ein Agent hat den Veröffentlichungsablauf geprüft; unabhängige Tests durch neue Anwender stehen noch aus. Ein kleines lokales Modell kann Fakten auslassen oder Antworten abschneiden. Bewahren Sie wichtige Ergebnisse außerhalb der Plattform auf.

Vollständige Wiederherstellung, Updates, Teamnutzung, der vollständige Unternehmens-SSO-Lebenszyklus, Cloud-Provider-Betrieb, beliebige MCP-Integrationen und Produktivlast sind nicht für Alpha.1 abgenommen. Stellen Sie die Installation nicht als öffentlichen Mehrbenutzerdienst bereit. Eine Ansicht zweier Versionen nebeneinander fehlt; unterscheiden Sie Artefakte anhand von ID und Erstellungszeit.

Pakete und SHA-256 stehen unter [Releases](https://github.com/ArtAndrew/RunVerge/releases). Eine Herausgebersignatur fehlt; die Prüfsumme bestätigt die Übereinstimmung mit dem veröffentlichten Wert, nicht die Identität des Herausgebers.

- [Installation](docs/NATIVE-BUNDLE.md) · [Modelle](docs/LOCAL-MODELS.md) · [MCP](docs/MCP-READ.md)
- [Versionshinweise](docs/RELEASE-NOTES-alpha.1.md) · [Unterstützung](docs/SUPPORT.md) · [Validierung](docs/VALIDATION-alpha.1.md)
- [Entwicklung](docs/DEVELOPMENT.md) · [API](contracts/README.md) · [öffentliche Inhalte](docs/PUBLIC-SOURCE.md)

[Feedback melden](https://github.com/ArtAndrew/RunVerge/issues/new/choose): Betriebssystem, Version und schwierigen Schritt angeben. Keine Tokens, privaten Einstellungen, Datenbankverbindungszeichenfolgen oder privaten Dokumente anhängen. Verlinkte Detailanleitungen können auf Englisch oder Russisch sein.

## Technik und Lizenzen

Server und Worker in Go, PostgreSQL, React/TypeScript-Workspace, Python- und TypeScript-SDKs. Der CLI-Befehl bleibt `platform`; interne Module heißen weiterhin `agent-platform`.

Server, CLI, Webanwendung, Dokumentation und Diagramme: [Elastic License 2.0](LICENSE). SDKs und Beispiele: Apache-2.0 gemäß den enthaltenen Lizenzdateien. Abhängigkeiten behalten ihre Lizenzen. ELv2 stellt den Quellcode bereit, ist aber keine von der OSI anerkannte Open-Source-Lizenz. [Lizenzgrenzen](docs/LICENSING.md).

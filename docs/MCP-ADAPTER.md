# MCP adapter: implemented foundation 0.14

Текущий wire API —0.14.0. Добавлен читающий `platform_capabilities`: тот же
каталог поддержки, ролей и начала работы, что в HTTP/CLI/SDK/Workspace.
Общее покрытие —81 JSON-операция:51 чтение и30 mutation tools с отдельным
операторским opt-in; SSE остаётся отдельным API transport. Сам каталог не
выдаёт прав и не вызывает providers. Новый профиль проверяется отдельно от
исторической основной установки0.12, описанной ниже. См. [инструкцию](CAPABILITIES.md).


## Версия и источники

На 2026-10-23 официальная ревизия `2026-07-28` опубликована. Закреплён официальный Go SDK `github.com/modelcontextprotocol/go-sdk v1.8.0`; commit тега `3f3b699b2b67e1ed033a63d6651671dab53c2d32`. Этот SDK поддерживает выбранную ревизию. Автоматический переход к legacy `initialize` и другим ревизиям отключён.

Первичные источники, прочитанные перед реализацией:

- [MCP 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28) — ревизия, discovery и возможности.
- [stdio transport](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/stdio) и [cancellation](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/cancellation) — framing, отмена и разрешённая гонка с уже начатым ответом.
- [Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http) — POST, метаданные запроса, JSON/SSE, отмена закрытием потока.
- [Authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization) — обязательная граница будущего OAuth resource-server профиля.
- [Go SDK v1.8.0](https://github.com/modelcontextprotocol/go-sdk/tree/v1.8.0), [Go API](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp) — конкретные API, лимиты и поведение.

Firecrawl documentation search был недоступен по квоте; использованы официальные страницы, исходники закреплённого тега и Go API reference. Протокольные свойства не выведены из сторонних примеров.

## Граница с основной платформой

Пакет `internal/mcpbridge` не использует PostgreSQL и не создаёт собственных правил бюджета или прав. `HandlerBackend(http.Handler)` вызывает обычные фиксированные маршруты `/api/v1/...` внутри процесса. Он передаёт явные Bearer и `X-Organization-ID`, исходное JSON-тело и долговечный `Idempotency-Key`. Произвольные URL, адреса базы и маршруты входа недоступны.

Каждый tools/discovery запрос заново проверяет учётные данные через обычный `/api/v1/me`; исполнение инструмента повторно проходит нормальную авторизацию своего маршрута. Отзыв токена и смена организации не обходятся наличием живого MCP-потока.

Идентификатор JSON-RPC связывает запрос с транспортным ответом. Он не является ключом идемпотентности. Все изменяющие инструменты требуют отдельный `idempotency_key`. Потерянный ответ может означать уже зафиксированную транзакцию: адаптер возвращает неопределённость, не повторяет операцию автоматически. Повтор с тем же исходным ключом проходит дедупликацию основного API. Закрытие HTTP/stdio запроса останавливает обработку запроса, но не отменяет долговечную задачу. Для отмены задачи существует отдельный инструмент с нормальными guards и сохранением финансовых обязательств.

## Каталог

Покрыт81 JSON-маршрут из82 реализованных API-маршрутов. SSE-поток не превращается в MCP-инструмент. По умолчанию объявляется51 инструмент чтения, включая `platform_api_compatibility`, `platform_event_snapshot` и `platform_event_feed`:

- текущий principal/организация/роль/permissions, агенты, доступные модели, список и детали задач;
- события, страницы артефактов, точная версия артефакта, расходы с отдельными курсорами;
- runs, operations, actions, approvals, candidates, decisions, children;
- список материалов, head, страницы метаданных ревизий, точное содержимое версии;
- безопасный каталог инструментов, список внешних occurrences и их точные снимки/согласования/наблюдения.
- метаданные и точное чтение native model tool proposals; они не являются итогом задачи или разрешением на действие.
- разрешение автоматизации и сигналы остановки; возможности управления участниками, список/точный участник и журнал доступа.
- политика входа, служебные аккаунты, безопасные метаданные ключей и журнал безопасности с человеческими правами управления.
- каталог настройки, одобренные назначения и версии ресурсов, списки/точные разрешения, планы и проверки связи, журнал настройки.

Оператор отдельно включает каждый из30 изменяющих инструментов. Существующие23: `platform_agents_create`, `platform_agents_publish`, `platform_tasks_create`, `platform_tasks_cancel`, `platform_tasks_review`, `platform_tasks_retry`, `platform_materials_create`, `platform_materials_revise`, `platform_materials_publish`, `platform_tool_executions_prepare`, `platform_tool_executions_decide`, `platform_tool_executions_dispatch`, `platform_tool_executions_reconcile`, `platform_model_continue`, `platform_automation_create`, `platform_automation_revoke`, `platform_members_create`, `platform_members_update`, `platform_members_revoke_credentials`, `platform_login_policy_update`, `platform_service_accounts_create`, `platform_service_accounts_update`, `platform_service_keys_revoke`.

Настройка добавляет7: `platform_configuration_grants_create`, `platform_configuration_grants_revoke`, `platform_configuration_plans_create`, `platform_configuration_plans_validate`, `platform_configuration_plans_approve`, `platform_configuration_plans_apply`, `platform_configuration_plans_verify`. Не включённый инструмент отсутствует в discovery и не исполняется. Allowlist не выдаёт пользователю прав, которых нет в основной платформе.

Review означает решение аутентифицированного principal; вызов через MCP не доказывает присутствие человека. Публикация материала требует точной версии задачи, ревизии, digest и исходного candidate. Она не принимает результат как истинный и не одобряет внешние действия. Содержимое артефакта, материала, tool description и peer annotations остаётся недоверенным контентом.

Discovery input schemas отражают формы текущих Go DTO, bounds внешних аргументов и `x-agent-platform-contract-revision: 0.14.0`. Они не заявлены полностью идентичными canonical JSON Schema: подробные ограничения вложенных DTO задаются `contracts/domain.schema.json`, а авторитетная проверка выполняется обычным API. Для пагинации limit 1..100; events default50, artifacts default20; material version 1..128, artifact version default1. Непрозрачные курсоры не декодируются адаптером. `events.after` — точная decimal string, а не JavaScript number. Backend самостоятельно передаёт обязательный `X-Platform-API-Version: 0.14.0` и проверяет версию ответа; аргументы инструмента не могут её переопределить. Подробности: [EVENTS.md](../contracts/EVENTS.md).

## Транспорт сервера

`NewServer(ServerConfig)` создаёт каталог; `Handler()` обслуживает современный Streamable HTTP, `RunStdio(ctx, Credentials)` — stdio. `ServeIO` предоставляет то же framing для доверенного embedding. HTTP-обработчик нужно разместить на HTTPS endpoint и использовать серверные header/read/write/idle deadlines. Текущий основной сервер имеет read15s, write30s, headers32KiB. Сам по себе `Handler()` не настраивает TLS listener.

HTTP использует частный профиль явного платформенного Bearer и организации. Cookie-аутентификация запрещена; неоднозначные Bearer/organization/протокольные заголовки отклоняются. Присутствующий Origin должен в точности находиться в операторском allowlist; без Origin допустим небраузерный клиент. SDK сверяет протокольные заголовки с метаданными тела. Внешняя ResponseWriter-обёртка восстанавливает `Cache-Control: no-store` прямо перед WriteHeader/Write/Flush, поскольку SDK устанавливает собственный cache header позднее начальной проверки. GET/DELETE и session-based legacy поток не включены. JSON response выбран сервером; клиент ниже проверен также с SSE peer.

В stdio principal и организация задаются при запуске доверенным оператором, а не в аргументах инструмента. STDOUT содержит только протокол; ошибки и ключи в него не выводятся. При завершении потока его reader/writer закрываются: embedding обязан передать streams, Close которых разблокирует Read/Write.

Лимиты: входящий frame/body 1MiB; глубина JSON32; ответ API1MiB; сериализованный stdio response4MiB; timeout по умолчанию10s, максимум30s; общая серверная concurrency по умолчанию16, максимум64. Имена JSON-RPC envelope точные; duplicate keys, повреждённый UTF-8 и непарные Unicode surrogate escapes отклоняются до декодирования SDK. Вызовы требуют ID; единственный разрешённый notification — cancel без собственного ID. Это не допускает скрытых business calls без ответа и неограниченной очереди notifications внутри SDK. Строковый request ID непустой и ограничен 128 байтами; числовой ID — точное целое в безопасном диапазоне ±(2^53−1). Это явные совместимые границы данного профиля, а не заявление о поддержке любых JSON-RPC IDs.

SDK1.8.0 доставляет cancellation в handler, но его JSON-RPC слой затем пишет завершившийся ответ. Узкая обёртка Connection обеспечивает правило modern stdio: отменённый запрос больше не пишет ответ. Начавшийся до принятия отмены write допустим. Обёртка сопоставляет внешний ID свежей внутренней generation; после cancel тот же внешний ID можно использовать снова, а запоздалый ответ старой generation не подавляет новый. Таблица содержит максимум64 незавершённых generation, включая отменённые до окончания handler; lifetime-лимита на здоровую сессию нет. SDK по-прежнему выполняет routing и cancellation. Заблокированная запись закрывает поток по deadline. При отмене не отправляется бизнес-команда CancelTask.

## Зарегистрированный клиент

`Connect(ClientConfig)` принимает ровно один заранее зарегистрированный HTTPS endpoint либо абсолютный executable. Это конфигурация доверенного оператора, а не URL/команда из ответа модели. HTTPS проверяет сертификат, точный host/path и адреса DNS при соединении; redirect и proxy отключены. По умолчанию запрещены private/loopback адреса; явное `AllowPrivateNetwork` позволяет их для управляемой локальной или корпоративной интеграции. Неопределённые, link-local и multicast адреса запрещены всегда. Проверка DNS не заменяет сетевой egress firewall.

Внешнему endpoint передаются только явно зарегистрированные для него credentials; cookies и входящий платформенный токен автоматически не наследуются. Для процесса environment задаётся полностью явно; shell не используется, stderr не возвращается модели. Доверенный executable имеет права своего OS-процесса — это не sandbox. SDK ограничивает его входящую строку 16MiB, после декодирования результат ограничивается 4MiB. Управление группой дочерних процессов и OS sandbox должны обеспечиваться execution gateway.

У клиента concurrency default4/max16, timeout default10s/max30s, HTTP body/event/result максимум4MiB. Нет автоматических retries, legacy fallback, standalone SSE, MRTR continuation или выполнения ответов elicitation. `AuthorizeCall == nil` запрещает любой tool call; ненулевой callback должен войти в общий gateway хоста. Peer `readOnlyHint` не является разрешением. Текущий пакет не выдаёт разрешения внешним side effects и не добавляет новые расходы в обход основного runtime.

## Проверки и границы приёмки

Авторский `go test -race ./internal/mcpbridge -run 'TestServer|TestClient|TestHandlerBackend|TestMCPRealPG' -count=1` использует независимые stdlib JSON peers по реальным TLS и stdio child process; отдельный тест использует изолированную схему PostgreSQL и настоящий HTTP API handler. Покрыты discovery, read/mutation каталог, exact credentials, metadata rejection, limits, origin/cookie ambiguity, cancellation barrier и ID reuse, заблокированный stdout, no-fallback/no-continuation/no-resend, redirect без утечки credentials, потеря acknowledgement после commit, долговечная дедупликация, смена tenant, отзыв token, exact material revision, version conflict и невозможность опубликовать draft без candidate. Fixture не вызывает платный LLM и не подтверждает корпоративную совместимость с произвольными MCP продуктами.

Точные результаты и хеши: `docs/security/mcp-adapter-author-evidence-2026-10-23.json`. Команда воспроизведения берёт локальную приватную конфигурацию через `scripts/dev.py`, не выводит её:

```python
import sys, subprocess
sys.path.insert(0, 'scripts')
import dev
env = dev.environment()
env['TEST_DATABASE_URL'] = env['DATABASE_URL']
subprocess.run(['go', 'test', '-race', './internal/mcpbridge', '-run',
                'TestServer|TestClient|TestHandlerBackend|TestMCPRealPG', '-count=1'], env=env, check=True)
```

Независимая проверка ограниченного адаптера описана в MCP-TRANSPORT-REVIEW.md, CLI — в MCP-CLI-REVIEW.md. Подтверждённый downgrade запрета кэширования исправлен, исходный отрицательный regression проходит. Гипотеза о case-shadow cancellation не подтвердилась; она не выдана за исправленный дефект.


Пока не выполнены: production onboarding внешних MCP peers; OAuth protected resource metadata, audience-bound access tokens, authorization server integration и другие требования W07; полный production-профиль автономной работы и универсальная сверка внешних эффектов; resources/prompts/progress/elicitation и расширения Tasks/Apps/Skills; принятие произвольных legacy peers. Эти возможности не объявляются сервером. Browser OIDC login и opaque bootstrap Bearer не заменяют MCP OAuth resource-server. Ограниченный явный gateway описан ниже.

## Gateway действий 0.6

[TOOL-EXECUTIONS.md](../contracts/TOOL-EXECUTIONS.md) описывает явную подготовку → отдельное согласование → явную отправку. Registered MCP HTTPS client теперь используется и этим runtime: каждый POST ограничен одним абсолютным допуском, discovery не продлевает право отправить tools/call; keep-alive/HTTP2/retry/GetBody отключены в action mode. Требуется установленный `agent_platform_action_v1` request/result contract с точными execution/action/intent IDs. Обычный successful MCP result не равен подтверждённому эффекту. Произвольный native peer без этого контракта не объявляется совместимым с автоматической effect classification.

Результаты/ошибки/описания peer не могут разрешить следующий вызов. Ручная operator assertion не запускает сеть и не является финансовым фактом. Полный OAuth onboarding и sandbox stdio, полный production-профиль автоматических execution/continuation policies и универсальная provider-specific сверка остаются обязательным незавершённым объёмом.

## Native model tools 0.7

[MODEL-TOOLS.md](../contracts/MODEL-TOOLS.md) добавляет закреплённые схемы и native function calls. MCP даёт чтение промежуточных proposals и отдельную команду продолжения после всех точных исходов. Предложение не выдаёт права подготовить другой инструмент или изменить аргументы; continuation использует current permissions, точные receipts и явное подтверждение новых расходов. Новый модельный шаг сохраняет Run и предыдущие обязательства. Неизвестный эффект блокирует продолжение даже при подтверждённом финансовом факте.

## Ограниченная автоматизация 0.8

`platform_automation_get` и `platform_progress_signals_list` читают разрешения, накопительные лимиты и основания остановки. Изменения `platform_automation_create` и `platform_automation_revoke` требуют отдельного opt-in и обычной авторизации API. Разрешение действует независимо от входной сессии: logout его не отзывает. Режим чтения требует доверенного opt-in установленного инструмента; режим подготовки сохраняет отдельные согласование и отправку человеком. Полная семантика, границы отзыва и сохранение unknown-расходов: [AUTOMATION.md](../contracts/AUTOMATION.md).

Управление участниками0.9 использует обычные API-права и CAS: [контракт](../contracts/MEMBERSHIPS.md). MCP не выдаёт логин или токен и не доставляет приглашение. Отзыв прежних входов действует только на выбранное членство организации; долговечное разрешение задачи отзывается отдельно. Отключение участника лишает старое выполнение права продолжать работу; sent/unknown и поздние факты не подменяются отменой.

Политика и служебные аккаунты0.10 описаны в [контракте безопасности](../contracts/SECURITY.md). Служебный principal не принимает результат как человек. Bearer MCP не переключает SSO: нужен браузер владельца с точным доверенным issuer. Файлы trusted-host `issue-token` и `issue-service-token` напрямую читаются `mcp-stdio --credentials PATH`; их дополнительные метаданные не выдают полномочий. Истечение/отзыв ключа закрывает новые запросы; execution consent отзывается отключением аккаунта или доступа приложений.

## Настройка подключений0.12

Новые17 инструментов `platform_configuration_*` используют тот же [контракт настройки](../contracts/CONFIGURATION.md):10 чтений и7 отдельных opt-in команд. [Руководство](CONFIGURATION.md) объясняет установку доверенных профилей, ограниченное делегирование, точный план/diff, одобрение владельца и атомарное применение. Пользователь/агент не подставляет свой URL, CA или ссылку на ключ. Сервисному member требуется текущий grant с точным scope; service-viewer не обходит запрет сохранённым idempotency-ответом. Требование одобрения владельца остаётся отдельным действием человеческого principal.

Создание, статическая проверка и применение плана не вызывают сеть. Отдельная явно подтверждённая проверка модели создаёт обычную задачу без инструментов и резервирует расходы перед отправкой; `response_received` не принимает кандидата и не подтверждает счёт.

Зарегистрированное MCP-подключение отдельно от исполняемого ToolConnection. Его проверка разрешена только для одобренного оператором бесплатного discovery и читает до16 страниц/128 инструментов/4MiB суммарного каталога. Она не вызывает `tools/call`, не устанавливает инструменты и не повторяет неизвестную отправку. Lease и короткий допуск закрепляют полный профиль, назначение ключа, scope и версию ресурса. Сохраняются только безопасные версия протокола/количество/digests; исходный поздний ответ может быть учтён после отзыва без нового вызова. Полный внешний MCP onboarding и OAuth остаются за пределами этого локального профиля.

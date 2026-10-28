# Операторские файлы настройки 0.12

Эта инструкция описывает файлы доверенного оператора для
`install-credential-purpose` и `install-configuration-destination`. Обычный
пользователь или сервисный агент затем работает через
[configuration API/CLI](CONFIGURATION.md); доступ к базе ему не нужен.

Сначала установите платформу, выполните миграции и создайте организацию по
[DEVELOPMENT.md](DEVELOPMENT.md). `organization_id` берут из её сохранённого
bootstrap-файла. Операторские команды требуют `DATABASE_URL` этой установки.
`platform` ниже означает собранный `bin/platform` из корня репозитория.

Примеры используют вымышленные адреса `.invalid`, модель и тариф. Они не
подключают готовую услугу. Замените организацию, адреса, модель, учётную запись,
тарифные допущения и пределы на одобренные значения. Для интеграционного теста
можно использовать собственные локальные синтетические peers.

Файлы должны быть обычными, приватными, без symlink; допустимы права `0600`
или `0400`. Создавайте их с `umask 077`. Не включайте значения API-ключей,
DSN или bearer-токены: здесь сохраняются только ограниченные имена переменных
окружения worker. Unknown/duplicate/null поля и лишний JSON отвергаются.
`created_at` задаёт сервер, в файлах его опускают.

## Подключение модели

Сохраните как `model-purpose.json`, заменив `org_example` на организацию:

```json
{
  "id": "model_purpose",
  "organization_id": "org_example",
  "version": 1,
  "kind": "model",
  "endpoint_digest": "MODEL_ENDPOINT_SHA256",
  "credential_environment": "PLATFORM_MODEL_KEY_PRIMARY",
  "active": true
}
```

`MODEL_ENDPOINT_SHA256` заменяют на 64 строчных hex-символа SHA-256 **точной
UTF-8 строки** `model.endpoint` из следующего файла, без добавления перевода
строки. Например, безопасно вычислить hash публичного адреса так:

```sh
python3 -c 'import hashlib; print(hashlib.sha256(b"https://model.example.invalid/v1/chat/completions").hexdigest())'
```

Сохраните как `model-destination.json`:

```json
{
  "destination": {
    "id": "primary_model_destination",
    "organization_id": "org_example",
    "version": 1,
    "kind": "model",
    "name": "Approved primary model",
    "target_id": "primary_model",
    "credential_purpose_id": "model_purpose",
    "credential_purpose_version": 1,
    "requires_approval": true,
    "max_verify_charge_microusd": "5000",
    "model": "example-model",
    "discovery_unmetered_approved": false,
    "active": true
  },
  "model": {
    "id": "primary_model",
    "organization_id": "org_example",
    "version": 1,
    "model": "example-model",
    "provider_account": "approved-account",
    "price_version": "illustrative-v1",
    "endpoint": "https://model.example.invalid/v1/chat/completions",
    "credential_environment": "",
    "max_charge_microusd": 5000,
    "max_input_tokens": 4096,
    "input_microusd_per_million": 1000000,
    "output_microusd_per_million": 2000000,
    "fixed_microusd": 0,
    "max_output_tokens": 128,
    "max_input_bytes": 65536,
    "max_output_bytes": 65536,
    "timeout_milliseconds": 10000,
    "bound_assumptions": "Illustrative synthetic tariff only; replace with approved provider billing bounds including all fees and hidden usage",
    "token_limit_field": "max_tokens",
    "allow_insecure_loopback": false,
    "active": false
  }
}
```

Согласуйте следующие связи:

| Поле | Правило |
|---|---|
| `organization_id` | Одна существующая организация во всех частях и purpose |
| `destination.target_id` | Равно `model.id`; слот ресурса постоянный |
| `destination.model` | Равно `model.model`, точное имя разрешённой модели |
| `credential_purpose_id/version` | Точная активная версия ранее установленного purpose с тем же kind и hash адреса |
| `model.credential_environment` | Пустая строка; сервер подставит ссылку только из purpose |
| `destination.max_verify_charge_microusd` | **JSON-строка**; не меньше резерва модели и не больше допустимой денежной границы |
| Числа внутри `model` | **JSON-числа**, включая micro-USD: это частный операторский формат, отличный от денежных строк публичного API |
| `model.version`, `model.active` | Шаблон требует положительную version; при применении фактическая версия определяется `expected_resource_version + 1`, активность — операцией плана |

Резерв модели должен покрывать оценку по максимальным входным и выходным
токенам плюс фиксированную плату. Здесь синтетическая оценка `4096 + 256 = 4352`
micro-USD укладывается в `5000`. Это арифметический пример, не тариф поставщика.
Резерв должен учитывать все реальные платёжные измерения, включая hidden usage
и округление. `max_output_bytes` ограничен 262144; timeout — 1..600000ms.
`token_limit_field` — `max_tokens` либо `max_completion_tokens` по протоколу
поставщика. Профиль поддерживает ограниченный HTTP chat-completions путь, а не
любой модельный API. Подробнее: model-execution.md.

Для локального тестового HTTP peer допускается `allow_insecure_loopback=true`
с явным loopback IP в адресе; для обычной удалённой модели оставляют HTTPS и
`false`. Это не общий доступ к произвольной внутренней сети.

## Подключение MCP

Сохраните как `mcp-purpose.json`:

```json
{
  "id": "mcp_purpose",
  "organization_id": "org_example",
  "version": 1,
  "kind": "mcp",
  "endpoint_digest": "MCP_ENDPOINT_SHA256",
  "credential_environment": "PLATFORM_MCP_KEY_PRIMARY",
  "active": true
}
```

Замените `MCP_ENDPOINT_SHA256` hash точной строки `mcp.endpoint`, тем же способом,
что для модели. Сохраните как `mcp-destination.json`:

```json
{
  "destination": {
    "id": "primary_mcp_destination",
    "organization_id": "org_example",
    "version": 1,
    "kind": "mcp",
    "name": "Approved MCP discovery",
    "target_id": "primary_mcp",
    "credential_purpose_id": "mcp_purpose",
    "credential_purpose_version": 1,
    "requires_approval": true,
    "max_verify_charge_microusd": "0",
    "discovery_unmetered_approved": true,
    "active": true
  },
  "mcp": {
    "endpoint": "https://mcp.example.invalid/mcp",
    "allow_private_network": false,
    "timeout_seconds": 10
  }
}
```

Сеть MCP требует HTTPS без URL-пароля, query или fragment. Для явно одобренного
внутреннего/локального TLS peer оператор может поставить
`allow_private_network=true` и добавить `trusted_ca_pem` с PEM-сертификатом CA
(JSON-строка с `\n`). Проверка TLS остаётся включённой. `timeout_seconds` — 1..30.
Профиль требует подтверждённое бесплатное discovery и нулевой денежный предел;
он не допускает платное discovery. Секрет worker должен соответствовать
разрешённому MCP-серверу. Установка и проверка соединения не устанавливают
исполняемые tools и не выполняют `tools/call`.

## Установка и передача ограниченного доступа

Установите сначала purpose, затем связанное назначение:

```sh
platform install-credential-purpose --file /private/model-purpose.json
platform install-configuration-destination --file /private/model-destination.json
platform install-credential-purpose --file /private/mcp-purpose.json
platform install-configuration-destination --file /private/mcp-destination.json
```

Команды записывают одобренные профили без проверки соединения. Рабочие ключи
`PLATFORM_MODEL_KEY_PRIMARY` и `PLATFORM_MCP_KEY_PRIMARY` передаются в окружение
worker доверенным механизмом установки. Сервисный агент не получает их или DSN.
Как обновлять окружение работающего процесса, определяет система запуска;
изменение shell окружения не меняет уже запущенный worker.

Профили immutable: новая version следует за предыдущей, одинаковый повтор не
активирует ранее отключённый профиль. Изменение адреса требует нового hash и
согласованной новой версии purpose/destination. Старые задачи не переезжают
на новую версию автоматически.

Оператор передаёт владельцу безопасные ID назначений. Владелец читает
`platform config catalog`, `destinations list`, `resources list`, создаёт
ограниченное разрешение сервисному principal по
[публичным JSON-формам](../contracts/CONFIGURATION.md). Сервисная учётная запись
сама по себе не даёт права настройки; нужны подходящая роль и живое разрешение.
Выдача service-token описана в [DEVELOPMENT.md](DEVELOPMENT.md). Передавать
bootstrap владельца сервисному агенту не требуется.

Далее выполняются отдельные plan → validate → требуемое owner approval → apply
→ явная ограниченная verify. `doctor` проверяет сохранённое наблюдаемое состояние
без внешних обращений. Даже успешная model verify сохраняет непроверенный
результат и неизвестный окончательный счёт; новый запуск или новый ключ не
разрешает повторить уже отправленную неопределённую проверку.

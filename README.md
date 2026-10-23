# Multi-account IMAP MCP Server

Read-only MCP-сервер на Go: один `/mcp` и несколько почтовых аккаунтов.
Mail.ru — preset IMAP, MCP и application слой не зависят от провайдера.
Включены presets Mail.ru, Яндекс, Gmail и настройка `custom` с IMAP over TLS.
Пароли приложений шифруются AES-256-GCM с привязкой к UUID аккаунта.

## Возможности

- `mail_accounts`: enabled аккаунты без credentials.
- `mail_folders`: список папок одного аккаунта.
- `mail_recent`: последние поступления в одной папке по всем выбранным аккаунтам.
- `mail_search`: IMAP TEXT (headers + body), From/To/Subject, Date, Seen, attachments и cursor.
- `mail_get`: письмо по UID, MIME text/plain либо безопасный HTML с plain text.
- `mail_thread`: ограниченная цепочка по Message-ID/In-Reply-To/References, в хронологическом порядке.

Нет SMTP и tools отправки, удаления, перемещения, архивирования или изменения Seen.
`mail_thread` включён сразу, поскольку он указан в разделе tools ТЗ, хотя также упомянут во втором milestone.
Бинарные вложения не скачиваются: `include_attachments=true` возвращает **только метаданные** из BODYSTRUCTURE.
Размер attachment — число байтов MIME-part в BODYSTRUCTURE; для base64 это encoded размер, не размер декодированного файла.

## Быстрый запуск с Docker Compose

Требуется работающий Docker Engine с Compose v2. HTTP backend публикуется только на `127.0.0.1:18080`, PostgreSQL не публикует порт.
Если порт занят другим сервисом, задайте свободный `MCP_HOST_PORT` в `.env`. Внутри контейнера MCP продолжает слушать `8080`.

```sh
cp .env.example .env
chmod 600 .env
```

Заполните `.env`: `POSTGRES_PASSWORD` (случайные hex-символы, чтобы не требовалось URL encoding), `MCP_SECRET_KEY`, `MCP_AUTH_TOKEN`.
Генерация значений по отдельности:

```sh
openssl rand -hex 24
openssl rand -base64 32
openssl rand -hex 32
```

Сохраните master key отдельно от базы: при его потере невозможно расшифровать пароли.
Ключ принимает 32 raw байта либо base64 от 32 байт. Настоящие пароли почты в `.env` не помещаются.
`DATABASE_URL` из `.env` нужен для запуска бинарника вне Compose; Compose собирает свой DSN из `POSTGRES_PASSWORD`.

```sh
docker compose up -d --build
docker compose exec mail-mcp /mail-mcp account add
```

Интерактивный CLI: alias `personal`, email, provider `mailru`, пароль **внешнего приложения**, без echo.
Повторите для `university` и `work`. Main account password использовать нельзя.

```sh
docker compose exec mail-mcp /mail-mcp account list
docker compose exec mail-mcp /mail-mcp account test university
docker compose exec mail-mcp /mail-mcp account disable university
docker compose exec mail-mcp /mail-mcp account delete university
curl http://127.0.0.1:18080/health
curl http://127.0.0.1:18080/ready
```

`disable` выключает аккаунт для MCP. `delete` удаляет credentials из базы; сами письма остаются на IMAP-сервере.
Встроенный интерактивный ввод не принимает пароль аргументом командной строки или через pipe.

## Mail.ru

Создайте пароль внешнего приложения: Настройки → Все настройки → Безопасность → Пароли для внешних приложений → Создать. Выберите тип «Полный доступ к Почте». Для создания пароля к аккаунту должен быть привязан телефон.
Официальная инструкция: https://help.mail.ru/mail/login/mailer/
Проверьте доступ IMAP в настройках аккаунта, затем выполните `account add` с preset `mailru`.
Он задаёт `imap.mail.ru:993`, TLS с проверкой сертификата и username=email.
Пароль приложения хранится только в `mail_accounts.encrypted_secret`; он не попадает в ответы MCP или логи.
Для Яндекса также используйте пароль приложения. Для Gmail потребуется разрешённый вашим аккаунтом способ IMAP-аутентификации; этот MVP реализует LOGIN с app password, **не** Gmail XOAUTH2.

## Локальная сборка

Зависимости зафиксированы в `go.mod`/`go.sum`; текущая сборка использует Go 1.27.1 (удовлетворяет требованию Go 1.25+).

```sh
make build
```

Установите `DATABASE_URL`, `MCP_SECRET_KEY`, `MCP_AUTH_TOKEN`, `HTTP_ADDR` и `LOG_LEVEL` через environment/secret manager.
Для новой PostgreSQL базы выполните:

```sh
./bin/mail-mcp migrate
./bin/mail-mcp account add
./bin/mail-mcp account test university
./bin/mail-mcp serve
```

`migrate` явно применяет начальную idempotent схему; автоматической модификации production схемы при старте нет.
Команда применяет также `002_oauth.sql` для встроенной авторизации и `003_mail_dates.sql` для сохраняемого индекса дат; повторное применение безопасно.
Инициализация `/docker-entrypoint-initdb.d` выполняется PostgreSQL только при первом создании volume.
Для уже существующего volume запускайте `docker compose exec mail-mcp /mail-mcp migrate`.
Архитектура использует ручную constructor injection: domain → app ports ← IMAP/PostgreSQL adapters, MCP вызывает app service.
Модуль пока локальный `mail-mcp`: после создания удалённого репозитория замените его module path и imports.

## HTTPS и ChatGPT

Для HTTPS на сервере с публичным DNS и открытыми 80/443 задайте `MCP_DOMAIN=mail-mcp.example.com` и запустите:

```sh
docker compose --profile https up -d --build
```

Caddy получает TLS-сертификат и проксирует `/mcp` и OAuth resource metadata.
`/metrics`, `/health`, `/ready` остаются доступны только через локальный backend; `/metrics` требует `MCP_AUTH_TOKEN`.
Можно использовать свой TLS reverse proxy с поддержкой SSE без буферизации и timeout выше времени tool call.

На VPS с другими сервисами сначала проверьте владельцев портов `80`, `443` и `MCP_HOST_PORT`.
Если `80/443` уже обслуживает общий reverse proxy, запускайте только backend и PostgreSQL:

```sh
docker compose up -d --build postgres mail-mcp
```

Добавьте домен MCP в конфигурацию существующего proxy, сохранив маршруты других сервисов.
Для proxy на хосте upstream — `127.0.0.1:18080` (или выбранный `MCP_HOST_PORT`).
Для proxy в контейнере `127.0.0.1` указывает на сам proxy: подключите его к сети MCP и используйте `mail-mcp:8080` либо отдельный уникальный сетевой alias.
Встроенный Caddy также использует `mail-mcp:8080`, поэтому его Caddyfile при смене `MCP_HOST_PORT` не меняется.

Для существующего proxy в Docker предусмотрен `deploy/compose.shared-proxy.yml`.
Укажите в `.env` `MCP_PROXY_NETWORK=<имя существующей сети proxy>` и, если собственного override ещё нет, скопируйте этот файл:

```sh
cp deploy/compose.shared-proxy.yml docker-compose.override.yml
docker compose config --quiet
docker compose up -d postgres mail-mcp
```

Compose автоматически загружает `docker-compose.override.yml` при следующих обычных запусках.
Если override уже существует, добавьте туда секции из overlay, сохранив прежние настройки.
MCP получает alias `imap-mcp-backend` в сети proxy; PostgreSQL остаётся в исходной сети MCP.
Overlay задаёт базе alias `imap-mcp-db` и использует его в `DATABASE_URL`, чтобы имя `postgres` не конфликтовало с базами других проектов в общей сети.
При первом применении overlay Compose пересоздаёт контейнер PostgreSQL для добавления alias; существующий volume с данными сохраняется.
Добавьте блок из `deploy/Caddyfile.shared.example` в реальный Caddyfile существующего proxy, сохранив остальные домены.
Перед изменением сделайте резервную копию файла, после изменения проверьте его командой `caddy validate` и примените через `caddy reload`.
Сначала проверяйте backend и новый HTTPS endpoint; затем проверьте существующие домены.
При использовании явных `-f` включайте overlay в список файлов: автоматическое чтение override в этом случае не применяется.

**Статический Bearer не является способом подключения приватной почты к ChatGPT UI.** Он подходит SDK-клиентам и MCP Inspector, которые умеют отправить заданный Authorization header.
Для ChatGPT добавлен встроенный режим `AUTH_MODE=oauth` с входом владельца,
PKCE S256, refresh/revoke и сохранением хешей токенов в PostgreSQL.
Настройка `.env`, общего Caddy, GitHub Actions → GHCR → SSH и подключение ChatGPT
описаны в [docs/deployment.md](docs/deployment.md). Готовый prod Compose использует
прежний volume проекта `imap_mcp` и отдельный alias базы `imap-mcp-db`.

Для внешнего authorization server остаётся режим `AUTH_MODE=oidc`:

```dotenv
AUTH_MODE=oidc
OIDC_ISSUER=https://auth.example.com/realms/mail
MCP_PUBLIC_URL=https://mail-mcp.example.com/mcp
OIDC_ALLOWED_SUBJECT=<subject владельца>
```

Внешний authorization server должен:

1. Поддерживать Authorization Code + PKCE S256 и публиковать OAuth/OIDC discovery.
2. Поддерживать подходящую регистрацию ChatGPT клиента: CIMD, DCR или pre-registered client с точным redirect URI из интерфейса подключения.
3. Выдавать **JWT access token** с `iss=OIDC_ISSUER`, `aud=MCP_PUBLIC_URL`, `sub=OIDC_ALLOWED_SUBJECT`, scope `mail.read`, конечным `exp`; поддерживаются RS256, ES256, EdDSA и JWKS rotation.
4. При необходимости длительного доступа поддерживать refresh tokens.

MCP публикует `/.well-known/oauth-protected-resource` и `/.well-known/oauth-protected-resource/mcp`, а при 401 возвращает `WWW-Authenticate` с metadata URL.
Проверяются подпись, issuer, audience, expiry, not-before, subject владельца и `mail.read`.
В OIDC режиме статический `MCP_AUTH_TOKEN` не даёт доступ к `/mcp`; он используется только для локальных метрик.
В режиме `oidc` выдачей токенов и входом управляет внешний провайдер.
Встроенный сервер авторизации и страница входа используются в режиме `oauth`.
Сервис рассчитан на одного владельца и N его ящиков; многопользовательская изоляция ящиков — отдельное расширение.

После настройки issuer и HTTPS добавьте endpoint в ChatGPT через Plugins либо Apps/Create в Developer mode, выберите OAuth, пройдите авторизацию, выполните Scan Tools и создайте подключение. Доступные пункты зависят от плана и workspace permissions.
Проверьте `mail_accounts`, затем запрос «Покажи последние письма из универа».
Наличие файлов и локальные тесты не подтверждают успешное подключение реального ChatGPT.
Официальные инструкции: [подключение MCP](https://developers.openai.com/plugins/quickstart), [OAuth](https://developers.openai.com/plugins/build/auth).

## Примеры tools

`mail_search`:

```json
{
  "accounts": ["university", "personal"],
  "query": "ВКР",
  "from": "teacher@example.com",
  "after": "2026-09-01T00:00:00+03:00",
  "before": "2026-10-01T00:00:00+03:00",
  "seen": false,
  "limit": 20
}
```

`accounts` отсутствует → все enabled аккаунты (до 32). Явный неизвестный/disabled selector — ошибка ввода.
Без `folder` инструменты `mail_search` и `mail_recent` просматривают **все доступные для чтения папки**, включая вложенные, отправленные, архив, спам и корзину. Служебные узлы с `\Noselect` / `\NonExistent` пропускаются. Подписка на папку не требуется.
Чтобы ограничить поиск одной папкой, передайте её точное IMAP-имя: например, `"folder": "INBOX"` или `"folder": "Учёба/Задания"`.
`limit` 1–100, default 20. `after` включительно, `before` исключительно; сравнивается Date письма, при отсутствии Date — InternalDate.
IMAP даты расширяются до дневного диапазона, затем проверяются локально с точными RFC3339 границами.
`query` передаётся как IMAP TEXT; качество Unicode/full-text поиска определяется IMAP-сервером. Локального full-text индекса пока нет.
Результаты — headers/BODYSTRUCTURE и preview до 300 UTF-8 байтов. Для preview читается не более 4 KiB encoded первой text-part; полное тело возвращает `mail_get`. `mail_recent` preview не запрашивает.

`mail_get`:

```json
{
  "account": "university",
  "folder": "INBOX",
  "uid": 18291,
  "uid_validity": 1,
  "include_attachments": true
}
```

Всегда берите `account`, `folder`, `uid` и `uid_validity` из результата поиска: одинаковые UID в разных папках обозначают разные письма. `mail_get` и `mail_thread` без `folder` по-прежнему используют INBOX. UID устойчив внутри текущей UIDVALIDITY папки;
`account:folder:uid` без UIDVALIDITY не гарантирует ту же запись после пересоздания папки.
Если optional `uid_validity` передан, сервер отклоняет обращение к пересозданной папке.
Не подставляйте sequence number вместо UID.

## Pagination, порядок и ограничения

Каждая папка каждого аккаунта просматривается по убывающему UID — порядку поступления на IMAP. Между папками и аккаунтами выбирается максимальная Date среди первых невыданных писем.
Это **не** строгая глобальная сортировка всех исторических писем по Date: при импорте старых писем и нестандартном Date порядок может отличаться. Для строгой сортировки и полнотекстового поиска нужен index из milestone 2.
За вызов скачиваются metadata не более 20 кандидатов на папку (5 для Yandex), в пределах limit+1; `mail_search` дополнительно читает ограниченные preview этих кандидатов, `mail_recent` тела не скачивает. UID SEARCH выполняется на сервере; список найденных UID может быть большим. Yandex игнорирует SENTSINCE/SENTBEFORE. Для поиска с after/before сервер сохраняет Date письма (или INTERNALDATE при некорректном Date) в PostgreSQL `mail_message_dates`, с ключом аккаунт + конфигурация подключения + папка + UIDVALIDITY + UID. Первый проход заполняет индекс ограниченными пакетами до 50 неизвестных UID; повторный запрос читает заголовки Date только новых UID. Уже известные даты проверяются по индексу без пустых страниц на каждые 50 старых писем. Список папок и найденные UID проверяются по IMAP при каждом новом поиске: удалённые письма исключаются, новые вложенные папки и импортированные старые Date учитываются. Индекс переживает рестарт/деплой; новая конфигурация подключения или UIDVALIDITY отделяет старые записи. Тела писем, темы и флаги не сохраняются. Заголовки читаются через BODY.PEEK с потоковым ограничением сохраняемых данных до 16 KiB; metadata/preview загружаются только для подходящих по дате кандидатов. Фильтрация сохраняет Date письма с точными RFC3339-границами, включая письма с датами не по порядку UID; INTERNALDATE используется только при отсутствующем или некорректном Date. LIST ограничен 1000 записями на аккаунт; cursor — 1 MiB. Если папок слишком много, сузьте accounts или укажите folder.

Cursor зашифрован/аутентифицирован AES-GCM, связан с фильтрами, limit, enabled account IDs, UIDVALIDITY и имеет TTL 15 минут.
Он хранит список папок и отдельные границы UID/UIDVALIDITY для каждой папки каждого аккаунта: новые поступления после первого успешного сканирования папки не попадают в продолжение. Папки, созданные после успешного LIST, появятся только в новом поиске.
Cursor предыдущей версии сервера несовместим с поиском по всем папкам: после обновления начните новый поиск без cursor.
Повторный запрос с тем же cursor может измениться, если письма удалены внешним почтовым клиентом; это не database snapshot.
Продолжайте с `next_cursor`, сохраняя все фильтры и limit. Пустая страница с cursor означает, что нужно продолжить сканирование старых UID (например, при фильтре attachments).
Ошибка аккаунта или папки остаётся в `errors` (`folder` указан для ошибки конкретной папки); письма из остальных доступных папок возвращаются, cursor позволяет повторить неудавшиеся операции. Если ошибка постоянна, сузьте accounts/folder и начните новый поиск.

Общий timeout поиска 15 s, timeout одного аккаунта вместе с обходом его папок 10 s, fan-out до 8 аккаунтов одновременно. При большом количестве папок обход может потребовать нескольких страниц; продолжайте с next_cursor, в том числе после частичного timeout. Следующая страница начинает обход после последней проверенной папки, поэтому постоянный таймаут одной папки не блокирует остальные.
Пул: максимум 2 соединения на аккаунт, простой 5 минут с фоновым сборщиком раз в минуту; ошибка/отмена уничтожает соединение.
Read-only обеспечивается EXAMINE, UID SEARCH/FETCH, BODY.PEEK и отсутствием mutating методов в adapter.

`mail_get`: 100 KiB plain text, до 200 KiB HTML. Если есть plain, HTML не возвращается.
Для HTML-only удаляются scripts, images, iframe, styles, event handlers и links; внешние ресурсы не загружаются.
Большой HTML отбрасывается целиком, чтобы не вернуть обрезанный тег. При превышении лимитов возвращается `truncated=true`.
MIME parts читаются отдельно, максимум 1 MiB encoded text-source и 16 text parts на письмо. Attachment-download не входит в этот milestone.
Thread discovery: максимум 5 раундов, 64 reference IDs и 200 кандидатов; общий бюджет текста+HTML цепочки 1 MiB, `truncated` сообщает об ограничении. Subject fallback выключен для предотвращения ложного объединения.

Email — недоверенные данные. Tool descriptions предупреждают модель об инструкциях внутри писем; сервер их не интерпретирует.
Эта маркировка не заменяет защиту MCP-клиента от prompt injection.

## Наблюдаемость

JSON slog: request_id, tool, account, duration_ms, result_count/error без паролей, токенов, тела письма и raw IMAP responses.
Метрики `/metrics`: mcp_requests_total, mcp_request_duration_seconds, imap_requests_total, imap_request_duration_seconds, imap_errors_total.
Labels только tool/operation/result; email, UID, subject и query в labels не используются.
`/health` — liveness. `/ready` проверяет PostgreSQL; config валидируется при старте. IMAP probes readiness не выполняет.

## Проверка

```sh
go test -race ./...
go vet ./...
TEST_DATABASE_URL='postgres://user:password@127.0.0.1:5432/testdb?sslmode=disable' go test -race -tags=integration ./...
```

По умолчанию тесты поднимают TLS IMAP и OIDC/JWKS сервера на loopback и MCP SDK-клиент по Streamable HTTP.
Проверяются шифрование/подмена ciphertext, русский RFC 2047, MIME base64/quoted-printable/KOI8-R, HTML-only, multipart, attachment metadata, read-only команды и Seen, pool, account selection, multi-account partial failures/timeouts, cursor, empty pages, thread grouping, OIDC token checks, tools/list и tools/call.
Для встроенного OAuth проверяются PKCE/CSRF, одноразовость кодов, refresh/revoke,
привязка к клиенту/resource, скрытие секретов и чтение через MCP SDK после OAuth
с реальным временным PostgreSQL и тестовым TLS IMAP-сервером.
PostgreSQL-тест с тегом `integration` требует TEST_DATABASE_URL и создаёт/удаляет только свой случайно названный schema.
Реальные Mail.ru credentials, public DNS/TLS и OAuth-подключение ChatGPT проверяются отдельно на deployment.

## Следующие milestones

Incremental local index/full-text PostgreSQL, IMAP IDLE, mail_attachment_get и REST admin API.
Write tools/SMTP — отдельный milestone с явными annotations и пользовательским подтверждением.

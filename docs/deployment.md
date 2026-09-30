# OAuth и деплой на VPS

Продакшен: `https://imap-mcp.chickenkiller.com/mcp`, проект
`kenniele/imap_mcp`, VPS `fitlog@88.218.68.176`, папка `/home/fitlog/imap_mcp`.
Это отдельный Compose-проект `imap_mcp` рядом с FitLog.

Как в FitLog, GitHub Actions проверяет Go, собирает образ в GHCR и выкладывает
его по SSH. На VPS Go не компилируется. Образ закреплён полным SHA коммита.
Перед заменой приложения выполняются `check-config` и `migrate`, после —
проверки базы, публичного OAuth discovery и защиты `/mcp`.
Общий `fitlog-caddy` управляется отдельно: workflow его не пересоздаёт.

## 1. Подготовить OAuth на VPS один раз

С локального компьютера, из репозитория, скопируй только помощник настройки:

```bash
scp deploy/configure-oauth.py fitlog@88.218.68.176:/home/fitlog/imap_mcp/configure-oauth.py
```

На VPS:

```bash
cd /home/fitlog/imap_mcp
python3 configure-oauth.py
```

Помощник создаёт закрытую резервную копию `.env` и два случайных OAuth-секрета.
Повторный запуск сохраняет эти секреты. `POSTGRES_PASSWORD`, `MCP_SECRET_KEY`,
`MCP_AUTH_TOKEN` и остальные настройки сохраняются.
Он записывает:

```dotenv
AUTH_MODE=oauth
MCP_PUBLIC_URL=https://imap-mcp.chickenkiller.com/mcp
MCP_OAUTH_CLIENT_ID=mail-mcp-chatgpt
MCP_OAUTH_CLIENT_SECRET=<отдельный случайный секрет клиента>
MCP_OAUTH_LOGIN_TOKEN=<отдельный случайный ключ входа владельца>
MCP_OAUTH_REDIRECT_URIS=https://chatgpt.com/connector_platform_oauth_redirect
MCP_PROXY_NETWORK=deployments_default
```

`OIDC_ISSUER` и `OIDC_ALLOWED_SUBJECT` для встроенного OAuth не нужны.
Пароли приложений Mail.ru/Яндекса/Gmail уже находятся в зашифрованной базе.
Ключ `MCP_SECRET_KEY` должен остаться прежним, иначе их нельзя будет расшифровать.

## 2. Обновить только блок IMAP в общем Caddyfile

На VPS сделай копию и открой реальный файл:

```bash
cp -p /home/fitlog/fitlog/deployments/Caddyfile \
  "/home/fitlog/fitlog/deployments/Caddyfile.bak.$(date +%Y%m%d-%H%M%S)"
nano /home/fitlog/fitlog/deployments/Caddyfile
```

В существующем блоке `imap-mcp.chickenkiller.com` замени строку `@public`:

```caddyfile
    @public path /mcp /oauth/mcp/* /.well-known/oauth-protected-resource /.well-known/oauth-protected-resource/mcp /.well-known/oauth-authorization-server
```

Остальные строки этого блока и домены FitLog/Event Connect сохраняются.
Не добавляй второй блок с тем же доменом. Применение:

```bash
docker exec fitlog-caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile &&
docker exec fitlog-caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
```

## 3. Настроить GitHub Actions secrets

Выполняй на своём компьютере, где установлен `gh` и есть доступ к репозиторию.
Вместо нового ключа можно использовать уже настроенный ключ деплоя FitLog.
Показанные команды создают отдельный ключ для этого проекта.

```bash
gh auth login

ssh-keygen -t ed25519 -f "$HOME/.ssh/imap_mcp_deploy" -N '' -C imap-mcp-deploy

cat "$HOME/.ssh/imap_mcp_deploy.pub" | ssh fitlog@88.218.68.176 \
  'umask 077; mkdir -p ~/.ssh; cat >> ~/.ssh/authorized_keys'

ssh -i "$HOME/.ssh/imap_mcp_deploy" fitlog@88.218.68.176 true

gh secret set DEPLOY_HOST --repo kenniele/imap_mcp --body '88.218.68.176'
gh secret set DEPLOY_USER --repo kenniele/imap_mcp --body 'fitlog'
gh secret set DEPLOY_PORT --repo kenniele/imap_mcp --body '22'
gh secret set DEPLOY_SSH_KEY --repo kenniele/imap_mcp < "$HOME/.ssh/imap_mcp_deploy"

ssh-keygen -F 88.218.68.176 > /tmp/imap-mcp-known-hosts
test -s /tmp/imap-mcp-known-hosts &&
gh secret set DEPLOY_KNOWN_HOSTS --repo kenniele/imap_mcp < /tmp/imap-mcp-known-hosts

gh variable set DEPLOY_PATH --repo kenniele/imap_mcp --body '/home/fitlog/imap_mcp'
```

При первом SSH-подключении сверь fingerprint сервера с данными провайдера;
в Actions используется уже доверенный ключ хоста с `StrictHostKeyChecking=yes`.
Приватный SSH-ключ передаётся напрямую в GitHub secret и не выводится в терминал.
OAuth-секреты и пароли почты в GitHub не нужны: они остаются в `.env`/PostgreSQL на VPS.

Для приватного GHCR-пакета пользователь `fitlog` должен иметь доступ к образу.
Если Docker уже авторизован для FitLog с подходящими правами на новый пакет,
этого достаточно. Иначе на VPS:

```bash
docker login ghcr.io -u kenniele
```

В приглашении пароля введи GitHub token с правом `read:packages` и доступом
к пакету `imap_mcp`. Не записывай его в `.env` и не отправляй в чат.
`GITHUB_TOKEN` для сборки/публикации Actions предоставляет автоматически.

## 4. Выпустить версию

После попадания изменений в `main` запускается `test → build → deploy`.
Повторный запуск текущего `main`:

```bash
gh workflow run deploy.yml --repo kenniele/imap_mcp --ref main
gh run list --repo kenniele/imap_mcp --workflow deploy.yml --limit 5
gh run watch --repo kenniele/imap_mcp
```

Workflow доставляет только `docker-compose.prod.yml` и `release.sh` в
`deploy/releases/<SHA>`. `.env`, root override и исходники на VPS он не заменяет.
Продакшен запускается с отдельным самостоятельным compose-файлом; root override
в этом режиме не используется. Сети, alias и переменные OAuth уже включены в prod.
Используется прежний `imap_mcp_postgres_data`, поэтому аккаунты сохраняются.
Первый переход может пересоздать контейнер PostgreSQL из-за смены конфигурации
томов/alias; содержимое volume остаётся прежним.

Проверки после выкладки:

```bash
curl -i --max-time 15 https://imap-mcp.chickenkiller.com/mcp
# Ожидается 401 с WWW-Authenticate и resource_metadata.
curl -fsS https://imap-mcp.chickenkiller.com/.well-known/oauth-protected-resource/mcp
curl -fsS https://imap-mcp.chickenkiller.com/.well-known/oauth-authorization-server
curl -I --max-time 15 https://fitlog.chickenkiller.com
curl -I --max-time 15 https://event-connect.duckdns.org
```

Для CLI после такого деплоя используй prod-конфигурацию, чтобы обычный `up`
случайно не переключил приложение обратно на локальный образ:

```bash
cd /home/fitlog/imap_mcp
export MCP_IMAGE=$(docker inspect imap_mcp-mail-mcp-1 --format '{{.Config.Image}}')
docker compose --project-name imap_mcp --project-directory "$PWD" --env-file .env \
  -f deploy/current/docker-compose.prod.yml exec mail-mcp /mail-mcp account list
```

## 5. Подключить ChatGPT

Создай MCP-подключение с URL `https://imap-mcp.chickenkiller.com/mcp` и OAuth.
Для веб-версии укажи заранее зарегистрированные client credentials:

- Client ID: значение `MCP_OAUTH_CLIENT_ID`.
- Client secret: значение `MCP_OAUTH_CLIENT_SECRET`.

Сверь точный callback в интерфейсе с `MCP_OAUTH_REDIRECT_URIS`.
Сервер публикует RFC 9207 issuer identification и возвращает `iss` в редиректе,
поэтому стандартный callback — `https://chatgpt.com/connector_platform_oauth_redirect`.
Если интерфейс показывает другой адрес, добавь именно его в allowlist и
пересоздай только `mail-mcp` с прежним образом через prod compose.

Посмотреть значения можно только на своём VPS:

```bash
cd /home/fitlog/imap_mcp
grep '^MCP_OAUTH_' .env
```

После начала подключения откроется страница Mail MCP. В неё введи
`MCP_OAUTH_LOGIN_TOKEN` и разреши доступ. Этот ключ не является client secret
или паролем какого-либо почтового ящика. Не отправляй вывод `.env` в чат.
В настольном клиенте с DCR обычно достаточно URL и прохождения той же страницы.
DCR ограничена native loopback callback; веб-клиент использует заданные credentials.

Разрешён только `mail.read`; `offline_access` сохраняет подключение до 30 дней.
Access token действует час, refresh ротируется. Смена login/client secret или
списка callback отзывает прежние grants; для сохранения подключения не меняй их
при каждом деплое. Коды/токены хранятся только в виде SHA-256-хешей.

Официальные требования: [OAuth в ChatGPT MCP](https://developers.openai.com/plugins/build/auth).

## Откат приложения

Бандлы прежних успешных версий сохраняются. Чтобы вернуться к проверенной версии,
выполни на VPS, подставив её полный SHA:

```bash
bash /home/fitlog/imap_mcp/deploy/releases/<FULL_SHA>/release.sh \
  /home/fitlog/imap_mcp ghcr.io/kenniele/imap_mcp:<FULL_SHA>
```

Миграции добавляют только отдельную OAuth-таблицу и не меняют сохранённые аккаунты.
Откат приложения не откатывает базу. Workflow не запускает `down`,
`--remove-orphans`, удаление volumes или глобальную чистку Docker.
Успешные проверки деплоя подтверждают HTTP/OAuth discovery и связь с PostgreSQL;
вход в реальном ChatGPT и IMAP-доступ к каждому ящику проверяются отдельно.

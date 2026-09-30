# Проверка реализации — 30 сентября 2026

- Go 1.27.1, macOS arm64.
- `go test -race ./...`: успешно, включая локальный TLS IMAP, OAuth/JWKS и MCP Streamable HTTP через официальный SDK-клиент.
- `go test -race -tags=integration ./...` с временным PostgreSQL 18 на loopback: успешно; schema создаётся/удаляется тестом. Временная БД остановлена после проверки.
- `go vet ./...`: успешно.
- `govulncheck ./...` (пересобран Go 1.27): No vulnerabilities found.
- `make build`: macOS binary `bin/mail-mcp` собран.
- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ... ./cmd/server`: Linux binary собран.
- `docker compose --profile https config --quiet` с тестовыми env: конфигурация валидна.
- Реальный бинарник проверен через PTY и временную БД: добавление 3 тестовых аккаунтов, отсутствие echo пароля, list/disable/delete, HTTP readiness, 401 без Bearer, graceful shutdown.

PTY-проверка обнаружила гонку при выводе приглашения до отключения echo. Исправлено: терминал переключается до показа приглашения. Повторная проверка прошла.

## Границы проверки

Docker daemon был недоступен: сборка Docker image, запуск Compose-сервисов и получение сертификата Caddy не проверены.
Реальные Mail.ru credentials не предоставлялись; никакие настоящие почтовые ящики не подключались.
Публичный DNS/HTTPS, внешний OAuth issuer и подключение ChatGPT требуют deployment и отдельной проверки.

Порядок массовой выдачи — по убывающему UID внутри аккаунта, с выбором очередного аккаунта по Date головы. Это не строгая глобальная сортировка всей истории по Date; подробнее в README.

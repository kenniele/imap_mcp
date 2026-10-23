package migrations

import _ "embed"

//go:embed 001_accounts.sql
var AccountsSQL string

//go:embed 002_oauth.sql
var OAuthSQL string

//go:embed 003_mail_dates.sql
var MailDatesSQL string

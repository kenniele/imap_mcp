package migrations

import _ "embed"

//go:embed 001_accounts.sql
var AccountsSQL string

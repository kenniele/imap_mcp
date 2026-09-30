package domain

import "time"

type Account struct {
	ID        string    `json:"id"`
	Alias     string    `json:"alias"`
	Email     string    `json:"email"`
	Provider  string    `json:"provider"`
	IMAPHost  string    `json:"-"`
	IMAPPort  int       `json:"-"`
	IMAPTLS   bool      `json:"-"`
	Username  string    `json:"-"`
	Secret    []byte    `json:"-"`
	Enabled   bool      `json:"-"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}
type ProviderPreset struct {
	IMAPHost string
	IMAPPort int
	TLS      bool
}

var Presets = map[string]ProviderPreset{
	"mailru": {"imap.mail.ru", 993, true},
	"yandex": {"imap.yandex.ru", 993, true},
	"gmail":  {"imap.gmail.com", 993, true},
}

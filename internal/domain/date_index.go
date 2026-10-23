package domain

// DateIndexKey isolates immutable UID dates by account connection and mailbox
// generation. Folder names and UIDVALIDITY alone are not unique across accounts.
type DateIndexKey struct {
	AccountID   string
	MailboxKey  string
	Folder      string
	UIDValidity uint32
}

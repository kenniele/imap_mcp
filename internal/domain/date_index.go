package domain

import "time"

// DateIndexKey isolates immutable UID dates by account connection and mailbox
// generation. Folder names and UIDVALIDITY alone are not unique across accounts.
type DateIndexKey struct {
	AccountID   string
	MailboxKey  string
	Folder      string
	UIDValidity uint32
}

// IndexedDate distinguishes a verified Date from an unreadable message body.
// For unreadable entries Date is INTERNALDATE and is never used to hide the
// missing Date: searches return an explicit warning and recheck ENVELOPE.
type IndexedDate struct {
	Date        time.Time
	Unavailable bool
}

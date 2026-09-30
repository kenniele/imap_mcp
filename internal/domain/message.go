package domain

import "time"

type Address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        uint32 `json:"size"`
	ID          string `json:"attachment_id"`
}
type Folder struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}
type Message struct {
	Account        string    `json:"account"`
	ID             string    `json:"id"`
	Folder         string    `json:"folder"`
	UID            uint32    `json:"uid"`
	UIDValidity    uint32    `json:"uid_validity"`
	MessageID      string    `json:"message_id"`
	Subject        string    `json:"subject"`
	From           []Address `json:"from"`
	To             []Address `json:"to"`
	CC             []Address `json:"cc,omitempty"`
	Date           time.Time `json:"date"`
	HasAttachments bool      `json:"has_attachments"`
	Seen           bool      `json:"seen"`
	Preview        string    `json:"preview,omitempty"`
}
type Content struct {
	Message
	Text        string       `json:"text"`
	HTML        *string      `json:"html"`
	Attachments []Attachment `json:"attachments"`
	Truncated   bool         `json:"truncated"`
	InReplyTo   []string     `json:"in_reply_to,omitempty"`
	References  []string     `json:"references,omitempty"`
}
type SearchQuery struct {
	Preview        bool     `json:"-"`
	Accounts       []string `json:"accounts,omitempty" jsonschema:"Account aliases or IDs; omit to query all enabled accounts"`
	Query          string   `json:"query,omitempty"`
	From           string   `json:"from,omitempty"`
	To             string   `json:"to,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	After          string   `json:"after,omitempty" jsonschema:"Inclusive RFC3339 message Date lower bound"`
	Before         string   `json:"before,omitempty" jsonschema:"Exclusive RFC3339 message Date upper bound"`
	Folder         string   `json:"folder,omitempty"`
	Seen           *bool    `json:"seen,omitempty"`
	HasAttachments *bool    `json:"has_attachments,omitempty"`
	Limit          int      `json:"limit,omitempty" jsonschema:"1 to 100; default 20"`
	Cursor         string   `json:"cursor,omitempty" jsonschema:"Opaque pagination token; keep all filters unchanged"`
}
type Position struct {
	BeforeUID   uint32 `json:"before_uid"`
	UIDValidity uint32 `json:"uid_validity"`
	Done        bool   `json:"done"`
}
type Page struct {
	Messages []Message
	Start    Position
	Next     Position
}
type AccountError struct {
	Account string `json:"account"`
	Error   string `json:"error"`
}
type SearchResult struct {
	Messages   []Message      `json:"messages"`
	Errors     []AccountError `json:"errors"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

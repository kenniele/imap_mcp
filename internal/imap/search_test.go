package imap

import (
	"testing"
	"time"

	"mail-mcp/internal/domain"

	imaplib "github.com/emersion/go-imap/v2"
)

func TestSearchCriteria(t *testing.T) {
	seen := false
	q := domain.SearchQuery{Query: "ВКР", From: "teacher@example.com", To: "student@example.com", Subject: "ВКР", After: "2026-09-01T01:30:00+03:00", Before: "2026-10-01T00:00:00+03:00", Seen: &seen}
	c := SearchCriteria(q)
	if len(c.Text) != 1 || c.Text[0] != "ВКР" || len(c.Header) != 3 || len(c.NotFlag) != 1 || c.NotFlag[0] != imaplib.FlagSeen {
		t.Fatal("query conversion failed")
	}
	after, _ := time.Parse(time.RFC3339, q.After)
	before, _ := time.Parse(time.RFC3339, q.Before)
	if matches(domain.Message{Date: after.Add(-time.Nanosecond)}, q) || !matches(domain.Message{Date: after}, q) || matches(domain.Message{Date: before}, q) {
		t.Fatal("exact RFC3339 boundaries failed")
	}
}

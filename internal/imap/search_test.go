package imap

import (
	"bufio"
	"context"
	"fmt"
	"github.com/emersion/go-imap/v2/imapclient"
	"net"
	"strings"
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

func TestFetchDatesUsesNonPartialReadOnlyHeaders(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	_ = clientConn.SetDeadline(time.Now().Add(3 * time.Second))
	_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { _ = client.Close(); _ = serverConn.Close() })
	done := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			done <- err
			return
		}
		line, err := bufio.NewReader(serverConn).ReadString('\n')
		if err != nil {
			done <- err
			return
		}
		if !strings.Contains(line, "UID FETCH") || !strings.Contains(line, "BODY.PEEK[HEADER.FIELDS") || strings.Contains(line, "<") || strings.Contains(line, "ENVELOPE") || strings.Contains(line, "BODYSTRUCTURE") {
			done <- fmt.Errorf("unexpected date fetch command")
			return
		}
		tag := strings.Fields(line)[0]
		headers := []string{"Date: Wed, 30 Sep 2026 08:42:00 +0300\r\n\r\n", "Date: invalid\r\n\r\n", "\r\n", "Date: " + strings.Repeat("x", 32*1024) + "\r\n\r\n"}
		for i, header := range headers {
			_, err = fmt.Fprintf(serverConn, "* %d FETCH (UID %d BODY[HEADER.FIELDS (Date)] {%d}\r\n%s INTERNALDATE \"01-Sep-2026 00:00:00 +0000\")\r\n", i+1, i+1, len(header), header)
			if err != nil {
				done <- err
				return
			}
		}
		_, err = fmt.Fprintf(serverConn, "%s OK fetched\r\n", tag)
		done <- err
	}()
	if err := client.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	dates, err := fetchDates(context.Background(), &connection{client: client, conn: clientConn}, []imaplib.UID{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	expected := time.Date(2026, 9, 30, 5, 42, 0, 0, time.UTC)
	fallback := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if len(dates) != 4 || !dates[1].Equal(expected) || !dates[2].Equal(fallback) || !dates[3].Equal(fallback) || !dates[4].Equal(fallback) {
		t.Fatalf("incorrect Date parsing/fallback: %v", dates)
	}
}

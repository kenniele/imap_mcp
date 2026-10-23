package imap

import (
	"bytes"
	"context"
	"net/mail"
	"sort"
	"time"

	"mail-mcp/internal/domain"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const MaxScan = 200

func SearchCriteria(q domain.SearchQuery) *imaplib.SearchCriteria {
	c := &imaplib.SearchCriteria{}
	for _, h := range []struct{ key, value string }{{"From", q.From}, {"To", q.To}, {"Subject", q.Subject}} {
		if h.value != "" {
			c.Header = append(c.Header, imaplib.SearchCriteriaHeaderField{Key: h.key, Value: h.value})
		}
	}
	if q.Query != "" {
		c.Text = []string{q.Query}
	} // IMAP TEXT searches headers and MIME body.
	if q.Seen != nil {
		if *q.Seen {
			c.Flag = []imaplib.Flag{imaplib.FlagSeen}
		} else {
			c.NotFlag = []imaplib.Flag{imaplib.FlagSeen}
		}
	}
	// IMAP compares dates without time zones. Widen UTC dates, then filter envelope Date exactly.
	if q.After != "" {
		t, _ := time.Parse(time.RFC3339, q.After)
		c.SentSince = t.UTC().Add(-24 * time.Hour)
	}
	if q.Before != "" {
		t, _ := time.Parse(time.RFC3339, q.Before)
		c.SentBefore = t.UTC().Add(48 * time.Hour)
	}
	return c
}
func (p *Provider) Search(ctx context.Context, a domain.Account, q domain.SearchQuery, pos domain.Position) (out domain.Page, err error) {
	out.Messages = []domain.Message{}
	err = p.with(ctx, a, "search", func(ctx context.Context, c *connection) error {
		selected, e := selectFolder(ctx, c, q.Folder, pos.UIDValidity)
		if e != nil {
			return e
		}
		if pos.BeforeUID == 0 {
			pos.BeforeUID = uint32(selected.UIDNext)
			pos.UIDValidity = selected.UIDValidity
		}
		out.Start = pos
		out.Next = pos
		if selected.NumMessages == 0 || pos.BeforeUID <= 1 {
			out.Next.Done = true
			return nil
		}
		criteria := SearchCriteria(q)
		localDates := a.Provider == "yandex" && (q.After != "" || q.Before != "")
		if localDates {
			// Yandex silently ignores SENT date search keys. Filter Date headers
			// locally without replacing message Date with INTERNALDATE.
			criteria.SentSince = time.Time{}
			criteria.SentBefore = time.Time{}
		}
		criteria.UID = []imaplib.UIDSet{{{Start: 1, Stop: imaplib.UID(pos.BeforeUID - 1)}}}
		data, e := step(ctx, "uid_search", func() (*imaplib.SearchData, error) { return c.client.UIDSearch(criteria, nil).Wait() }, "text_filter", q.Query != "", "limit", q.Limit, "preview", q.Preview)
		if e != nil {
			return e
		}
		uids := data.AllUIDs()
		sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
		if len(uids) == 0 {
			out.Next.Done = true
			return nil
		}
		// Bound metadata work per cursor page; BODYSTRUCTURE can be slow on large mailboxes.
		batchSize := 20
		if a.Provider == "yandex" {
			batchSize = 5
		}
		count := min(MaxScan, batchSize, q.Limit+1, len(uids))
		candidates := uids[:count]
		metadataUIDs := candidates
		if localDates {
			candidates = uids[:min(MaxScan, len(uids))]
			dates, err := fetchDates(ctx, c, candidates)
			if err != nil {
				return err
			}
			metadataUIDs = nil
			for i, uid := range candidates {
				date, ok := dates[uint32(uid)]
				if ok && matchesDate(date, q) {
					metadataUIDs = append(metadataUIDs, uid)
					if len(metadataUIDs) == count {
						candidates = candidates[:i+1]
						break
					}
				}
			}
		}
		buffers, e := fetchMetadata(ctx, c, metadataUIDs)
		if e != nil {
			return e
		}
		byUID := map[uint32]domain.Message{}
		for _, buf := range buffers {
			m := summary(a, q.Folder, selected.UIDValidity, buf)
			byUID[m.UID] = m
		}
		processed := 0
		for _, uid := range candidates {
			processed++
			out.Next.BeforeUID = uint32(uid)
			m, ok := byUID[uint32(uid)]
			if !ok {
				continue
			}
			if !matches(m, q) {
				continue
			}
			if q.Preview {
				m.Preview, e = fetchPreview(ctx, c, uint32(uid), buffersForUID(buffers, uid))
				if e != nil {
					return e
				}
			}
			out.Messages = append(out.Messages, m)
			if len(out.Messages) >= q.Limit+1 {
				break
			}
		}
		out.Next.Done = processed == len(uids)
		return nil
	})
	return out, err
}

// fetchDates scans only bounded Date headers, avoiding expensive ENVELOPE and
// BODYSTRUCTURE for thousands of messages excluded by the exact time window.
func fetchDates(ctx context.Context, c *connection, uids []imaplib.UID) (map[uint32]time.Time, error) {
	section := &imaplib.FetchItemBodySection{Specifier: imaplib.PartSpecifierHeader, HeaderFields: []string{"Date"}, Peek: true, Partial: &imaplib.SectionPartial{Size: 16 * 1024}}
	buffers, err := step(ctx, "uid_fetch_dates", func() ([]*imapclient.FetchMessageBuffer, error) {
		return c.client.Fetch(imaplib.UIDSetNum(uids...), &imaplib.FetchOptions{UID: true, InternalDate: true, BodySection: []*imaplib.FetchItemBodySection{section}}).Collect()
	}, "requested_count", len(uids), "peek", true)
	if err != nil {
		return nil, err
	}
	dates := make(map[uint32]time.Time, len(buffers))
	for _, buf := range buffers {
		date := buf.InternalDate
		if msg, err := mail.ReadMessage(bytes.NewReader(buf.FindBodySection(section))); err == nil {
			if parsed, err := msg.Header.Date(); err == nil {
				date = parsed
			}
		}
		dates[uint32(buf.UID)] = date
	}
	return dates, nil
}

func matchesDate(date time.Time, q domain.SearchQuery) bool {
	if q.After != "" {
		t, _ := time.Parse(time.RFC3339, q.After)
		if date.Before(t) {
			return false
		}
	}
	if q.Before != "" {
		t, _ := time.Parse(time.RFC3339, q.Before)
		if !date.Before(t) {
			return false
		}
	}
	return true
}

func matches(m domain.Message, q domain.SearchQuery) bool {
	if !matchesDate(m.Date, q) {
		return false
	}
	if q.HasAttachments != nil && m.HasAttachments != *q.HasAttachments {
		return false
	}
	return true
}

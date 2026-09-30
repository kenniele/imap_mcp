package imap

import (
	"context"
	"errors"
	"sort"

	"mail-mcp/internal/app"
	"mail-mcp/internal/domain"

	imaplib "github.com/emersion/go-imap/v2"
)

func threadCriteria(ids []string) *imaplib.SearchCriteria {
	clauses := []imaplib.SearchCriteria{}
	for _, id := range ids {
		for _, field := range []string{"Message-ID", "In-Reply-To", "References"} {
			clauses = append(clauses, imaplib.SearchCriteria{Header: []imaplib.SearchCriteriaHeaderField{{Key: field, Value: id}}})
		}
	}
	if len(clauses) == 0 {
		return &imaplib.SearchCriteria{}
	}
	var or func([]imaplib.SearchCriteria) imaplib.SearchCriteria
	or = func(c []imaplib.SearchCriteria) imaplib.SearchCriteria {
		if len(c) == 1 {
			return c[0]
		}
		n := len(c) / 2
		return imaplib.SearchCriteria{Or: [][2]imaplib.SearchCriteria{{or(c[:n]), or(c[n:])}}}
	}
	result := or(clauses)
	return &result
}
func (p *Provider) Thread(ctx context.Context, a domain.Account, folder string, uid, validity uint32, limit int) (out []domain.Content, truncated bool, err error) {
	out = []domain.Content{}
	err = p.with(ctx, a, "thread", func(c *connection) error {
		selected, e := selectFolder(c, folder, validity)
		if e != nil {
			return e
		}
		anchor, e := getContent(c, a, folder, uid, selected.UIDValidity, true)
		if e != nil {
			return e
		}
		if anchor.MessageID == "" {
			out = append(out, anchor)
			return nil
		}
		known := map[string]bool{}
		add := func(m domain.Content) {
			for _, id := range append(append([]string{m.MessageID}, m.InReplyTo...), m.References...) {
				if id != "" {
					known[id] = true
				}
			}
		}
		add(anchor)
		candidates := []domain.Content{anchor}
		seen := map[uint32]bool{uid: true}
		for round := 0; round < 5; round++ {
			ids := []string{}
			for id := range known {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			if len(ids) > 64 {
				ids = ids[:64]
				truncated = true
			}
			data, e := c.client.UIDSearch(threadCriteria(ids), nil).Wait()
			if e != nil {
				return errors.New("imap operation failed")
			}
			uids := data.AllUIDs()
			if len(uids) > MaxScan {
				uids = uids[:MaxScan]
				truncated = true
			}
			newUIDs := []imaplib.UID{}
			for _, u := range uids {
				if !seen[uint32(u)] {
					newUIDs = append(newUIDs, u)
				}
			}
			if len(newUIDs) == 0 {
				break
			}
			bufs, e := fetchMetadata(c, newUIDs)
			if e != nil {
				return e
			}
			for _, buf := range bufs {
				if len(candidates) >= MaxScan {
					truncated = true
					break
				}
				m := domain.Content{Message: summary(a, folder, selected.UIDValidity, buf)}
				h, e := fetchSection(c, m.UID, &imaplib.FetchItemBodySection{Specifier: imaplib.PartSpecifierHeader, Peek: true, Partial: &imaplib.SectionPartial{Size: 64 * 1024}})
				if e != nil {
					return e
				}
				m.InReplyTo, m.References = parseReferences(h)
				candidates = append(candidates, m)
				seen[m.UID] = true
				add(m)
			}
			if round == 4 {
				truncated = true
			}
		}
		grouped := app.GroupThread(anchor.MessageID, candidates)
		if len(grouped) > limit {
			grouped = grouped[:limit]
			truncated = true
		}
		for _, m := range grouped {
			if m.UID == uid {
				out = append(out, anchor)
				continue
			}
			content, e := getContent(c, a, folder, m.UID, selected.UIDValidity, true)
			if e != nil {
				return e
			}
			out = append(out, content)
		}
		return nil
	})
	return out, truncated, err
}

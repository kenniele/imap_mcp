package app

import (
	"context"
	"errors"
	"sort"

	"mail-mcp/internal/domain"
)

type ThreadResult struct {
	Messages  []domain.Content `json:"messages"`
	Truncated bool             `json:"truncated"`
}

func (s *Service) Thread(ctx context.Context, account, folder string, uid, validity uint32, limit int) (ThreadResult, error) {
	out := ThreadResult{Messages: []domain.Content{}}
	if uid == 0 {
		return out, errors.New("uid must be positive")
	}
	if limit == 0 {
		limit = 30
	}
	if limit < 1 || limit > 100 {
		return out, errors.New("limit must be between 1 and 100")
	}
	q, err := Normalize(domain.SearchQuery{Folder: folder})
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.AccountTimeout)
	defer cancel()
	a, err := s.account(ctx, account)
	if err != nil {
		return out, err
	}
	out.Messages, out.Truncated, err = s.Mail.Thread(ctx, a, q.Folder, uid, validity, limit)
	if err != nil {
		return out, errors.New(safeError(err))
	}
	sort.Slice(out.Messages, func(i, j int) bool { return out.Messages[i].Date.Before(out.Messages[j].Date) })
	// A thread has a total 1 MiB text+HTML budget in addition to each message's limits.
	remaining := 1024 * 1024
	for i := range out.Messages {
		m := &out.Messages[i]
		if len(m.Text) > remaining {
			m.Text = truncateUTF8(m.Text, remaining)
			m.Truncated = true
			out.Truncated = true
		}
		remaining -= len(m.Text)
		if m.HTML != nil {
			if len(*m.HTML) > remaining {
				m.HTML = nil
				m.Truncated = true
				out.Truncated = true
			} else {
				remaining -= len(*m.HTML)
			}
		}
	}
	return out, nil
}
func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xc0) == 0x80 {
		n--
	}
	return s[:n]
}

// GroupThread computes the connected component of RFC message references.
// Subject-only grouping is deliberately omitted to avoid mixing unrelated private mail.
func GroupThread(seed string, messages []domain.Content) []domain.Content {
	known := map[string]bool{seed: true}
	selected := map[int]bool{}
	changed := true
	for changed {
		changed = false
		for i, m := range messages {
			ids := append([]string{m.MessageID}, m.InReplyTo...)
			ids = append(ids, m.References...)
			linked := false
			for _, id := range ids {
				if id != "" && known[id] {
					linked = true
					break
				}
			}
			if linked {
				selected[i] = true
				for _, id := range ids {
					if id != "" && !known[id] {
						known[id] = true
						changed = true
					}
				}
			}
		}
	}
	out := []domain.Content{}
	for i, m := range messages {
		if selected[i] {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

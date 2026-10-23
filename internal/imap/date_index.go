package imap

import (
	"context"
	"encoding/hex"
	"log/slog"
	"time"

	"mail-mcp/internal/domain"

	imaplib "github.com/emersion/go-imap/v2"
)

// DateIndex persists only immutable dates; flags, folder membership and message
// metadata are always read live. A nil index keeps bounded uncached scanning.
type DateIndex interface {
	LoadDates(context.Context, domain.DateIndexKey, []uint32) (map[uint32]time.Time, error)
	SaveDates(context.Context, domain.DateIndexKey, map[uint32]time.Time) error
}

func (p *Provider) dateCandidates(ctx context.Context, c *connection, a domain.Account, q domain.SearchQuery, validity uint32, uids []imaplib.UID, count int) ([]imaplib.UID, []imaplib.UID, error) {
	ids := make([]uint32, len(uids))
	for i, uid := range uids {
		ids[i] = uint32(uid)
	}
	fp := fingerprint(a)
	key := domain.DateIndexKey{AccountID: a.ID, MailboxKey: hex.EncodeToString(fp[:]), Folder: q.Folder, UIDValidity: validity}
	dates := map[uint32]time.Time{}
	if p.DateIndex != nil {
		var err error
		dates, err = p.DateIndex.LoadDates(ctx, key, ids)
		if err != nil {
			return nil, nil, err
		}
	}
	missing := make([]imaplib.UID, 0, maxDateScan)
	for _, uid := range uids {
		if _, ok := dates[uint32(uid)]; !ok {
			missing = append(missing, uid)
			if len(missing) == maxDateScan {
				break
			}
		}
	}
	cachedCount := len(dates)
	if len(missing) > 0 {
		fetched, err := fetchDates(ctx, c, missing)
		if err != nil {
			return nil, nil, err
		}
		if p.DateIndex != nil {
			if err := p.DateIndex.SaveDates(ctx, key, fetched); err != nil {
				return nil, nil, err
			}
		}
		// A UID may be expunged after SEARCH. It is safe to advance past it, but
		// absence is not cached: no watermark can hide a later imported message.
		for _, uid := range missing {
			dates[uint32(uid)] = fetched[uint32(uid)]
		}
	}
	metadata := make([]imaplib.UID, 0, count)
	scanned := 0
	for _, uid := range uids {
		date, known := dates[uint32(uid)]
		if !known {
			break
		}
		scanned++
		if !date.IsZero() && matchesDate(date, q) {
			metadata = append(metadata, uid)
			if len(metadata) == count {
				break
			}
		}
	}
	slog.DebugContext(ctx, "imap date index page", append(traceAttrs(ctx), "cached_count", cachedCount, "fetched_count", len(missing), "scanned_count", scanned, "match_count", len(metadata))...)
	return uids[:scanned], metadata, nil
}

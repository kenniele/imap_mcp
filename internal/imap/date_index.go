package imap

import (
	"context"
	"encoding/hex"
	"log/slog"

	"mail-mcp/internal/domain"

	imaplib "github.com/emersion/go-imap/v2"
)

// DateIndex persists only immutable dates; flags, folder membership and message
// metadata are always read live. A nil index keeps bounded uncached scanning.
type DateIndex interface {
	LoadDates(context.Context, domain.DateIndexKey, []uint32) (map[uint32]domain.IndexedDate, error)
	SaveDates(context.Context, domain.DateIndexKey, map[uint32]domain.IndexedDate) error
}

func (p *Provider) dateCandidates(ctx context.Context, c *connection, a domain.Account, q domain.SearchQuery, validity uint32, uids []imaplib.UID, count int) ([]imaplib.UID, []imaplib.UID, int, error) {
	ids := make([]uint32, len(uids))
	for i, uid := range uids {
		ids[i] = uint32(uid)
	}
	fp := fingerprint(a)
	key := domain.DateIndexKey{AccountID: a.ID, MailboxKey: hex.EncodeToString(fp[:]), Folder: q.Folder, UIDValidity: validity}
	dates := map[uint32]domain.IndexedDate{}
	if p.DateIndex != nil {
		var err error
		dates, err = p.DateIndex.LoadDates(ctx, key, ids)
		if err != nil {
			return nil, nil, 0, err
		}
	}
	cachedCount := len(dates)
	var low, high uint32
	var retry []imaplib.UID
	for uid, record := range dates {
		if low == 0 || uid < low {
			low = uid
		}
		if uid > high {
			high = uid
		}
		if record.Unavailable {
			retry = append(retry, imaplib.UID(uid))
		}
	}
	updates := map[uint32]domain.IndexedDate{}
	if len(retry) > 0 {
		restored, err := fetchEnvelopeDates(ctx, c, retry, dates)
		if err != nil {
			return nil, nil, 0, err
		}
		for uid, record := range restored {
			previous := dates[uid]
			dates[uid] = record
			if previous.Unavailable != record.Unavailable || !previous.Date.Equal(record.Date) {
				updates[uid] = record
			}
		}
	}
	var missing, headers, gaps []imaplib.UID
	for _, uid := range uids {
		if _, ok := dates[uint32(uid)]; !ok {
			missing = append(missing, uid)
			if low != 0 && uint32(uid) >= low && uint32(uid) <= high {
				gaps = append(gaps, uid)
			} else {
				headers = append(headers, uid)
			}
			if len(missing) == maxDateScan {
				break
			}
		}
	}
	if len(headers) > 0 {
		fetched, err := fetchDates(ctx, c, headers)
		if err != nil && !isUnavailable(err) {
			return nil, nil, 0, err
		}
		for _, uid := range headers {
			if date, ok := fetched[uint32(uid)]; ok {
				updates[uint32(uid)] = domain.IndexedDate{Date: date}
			} else {
				gaps = append(gaps, uid)
			}
		}
	}
	if len(gaps) > 0 {
		fetched, err := fetchEnvelopeDates(ctx, c, gaps, dates)
		if err != nil {
			return nil, nil, 0, err
		}
		for uid, record := range fetched {
			updates[uid] = record
		}
	}
	if p.DateIndex != nil && len(updates) > 0 {
		if err := p.DateIndex.SaveDates(ctx, key, updates); err != nil {
			return nil, nil, 0, err
		}
	}
	for uid, record := range updates {
		dates[uid] = record
	}
	// Absent from both FETCH and INTERNALDATE means expunged during this search.
	// Unlike an unavailable body this absence must not be persisted.
	for _, uid := range missing {
		if _, ok := dates[uint32(uid)]; !ok {
			dates[uint32(uid)] = domain.IndexedDate{}
		}
	}
	unavailable := 0
	for _, record := range dates {
		if record.Unavailable {
			unavailable++
		}
	}

	metadata := make([]imaplib.UID, 0, count)
	scanned := 0
	for _, uid := range uids {
		record, known := dates[uint32(uid)]
		if !known {
			break
		}
		scanned++
		if !record.Unavailable && !record.Date.IsZero() && matchesDate(record.Date, q) {
			metadata = append(metadata, uid)
			if len(metadata) == count {
				break
			}
		}
	}
	slog.DebugContext(ctx, "imap date index page", append(traceAttrs(ctx), "cached_count", cachedCount, "fetched_count", len(missing), "scanned_count", scanned, "match_count", len(metadata), "unavailable_count", unavailable)...)
	return uids[:scanned], metadata, unavailable, nil
}

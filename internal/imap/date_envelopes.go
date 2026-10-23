package imap

import (
	"context"
	"errors"

	"mail-mcp/internal/domain"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func isUnavailable(err error) bool {
	response, ok := errors.AsType[*imaplib.Error](err)
	return ok && response.Type == imaplib.StatusResponseTypeNo && response.Code == imaplib.ResponseCodeUnavailable
}

// Some Yandex UIDs remain searchable but their bodies (including ENVELOPE) are
// unavailable. INTERNALDATE is still readable. Keep that state explicit and
// recheck ENVELOPE on later searches so repaired messages reappear immediately.
func fetchEnvelopeDates(ctx context.Context, c *connection, uids []imaplib.UID, known map[uint32]domain.IndexedDate) (map[uint32]domain.IndexedDate, error) {
	buffers, err := step(ctx, "uid_fetch_date_envelopes", func() ([]*imapclient.FetchMessageBuffer, error) {
		return c.client.Fetch(imaplib.UIDSetNum(uids...), &imaplib.FetchOptions{UID: true, InternalDate: true, Envelope: true}).Collect()
	}, "requested_count", len(uids))
	if err != nil && !isUnavailable(err) {
		return nil, err
	}
	out := map[uint32]domain.IndexedDate{}
	for _, buf := range buffers {
		if buf.Envelope != nil {
			date := buf.Envelope.Date
			if date.IsZero() {
				date = buf.InternalDate
			}
			out[uint32(buf.UID)] = domain.IndexedDate{Date: date}
		}
	}
	var missing []imaplib.UID
	for _, uid := range uids {
		if _, ok := out[uint32(uid)]; ok {
			continue
		}
		if record, ok := known[uint32(uid)]; ok && record.Unavailable {
			out[uint32(uid)] = record
		} else {
			missing = append(missing, uid)
		}
	}
	if len(missing) > 0 {
		buffers, err = step(ctx, "uid_fetch_internal_dates", func() ([]*imapclient.FetchMessageBuffer, error) {
			return c.client.Fetch(imaplib.UIDSetNum(missing...), &imaplib.FetchOptions{UID: true, InternalDate: true}).Collect()
		}, "requested_count", len(missing))
		if err != nil {
			return nil, err
		}
		for _, buf := range buffers {
			out[uint32(buf.UID)] = domain.IndexedDate{Date: buf.InternalDate, Unavailable: true}
		}
	}
	return out, nil
}

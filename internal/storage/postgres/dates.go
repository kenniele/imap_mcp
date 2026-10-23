package postgres

import (
	"context"
	"errors"
	"time"

	"mail-mcp/internal/domain"
)

func (s *Store) LoadDates(ctx context.Context, key domain.DateIndexKey, uids []uint32) (map[uint32]domain.IndexedDate, error) {
	dates := make(map[uint32]domain.IndexedDate)
	if len(uids) == 0 {
		return dates, nil
	}
	ids := make([]int64, len(uids))
	for i, uid := range uids {
		ids[i] = int64(uid)
	}
	rows, err := s.DB.Query(ctx, `SELECT uid,message_date,unavailable FROM mail_message_dates
 WHERE account_id=$1 AND mailbox_key=$2 AND folder=$3 AND uid_validity=$4 AND uid=ANY($5::bigint[])`, key.AccountID, key.MailboxKey, key.Folder, int64(key.UIDValidity), ids)
	if err != nil {
		return nil, errors.New("mail date index query failed")
	}
	defer rows.Close()
	for rows.Next() {
		var uid int64
		var date domain.IndexedDate
		if err := rows.Scan(&uid, &date.Date, &date.Unavailable); err != nil {
			return nil, errors.New("mail date index decoding failed")
		}
		dates[uint32(uid)] = date
	}
	if rows.Err() != nil {
		return nil, errors.New("mail date index query failed")
	}
	return dates, nil
}

func (s *Store) SaveDates(ctx context.Context, key domain.DateIndexKey, dates map[uint32]domain.IndexedDate) error {
	if len(dates) == 0 {
		return nil
	}
	uids := make([]int64, 0, len(dates))
	values := make([]time.Time, 0, len(dates))
	unavailable := make([]bool, 0, len(dates))
	for uid, date := range dates {
		uids = append(uids, int64(uid))
		values = append(values, date.Date)
		unavailable = append(unavailable, date.Unavailable)
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO mail_message_dates(account_id,mailbox_key,folder,uid_validity,uid,message_date,unavailable)
 SELECT $1,$2,$3,$4,d.uid,d.message_date,d.unavailable FROM unnest($5::bigint[],$6::timestamptz[],$7::boolean[]) AS d(uid,message_date,unavailable)
 ON CONFLICT (account_id,mailbox_key,folder,uid_validity,uid) DO UPDATE SET message_date=EXCLUDED.message_date,unavailable=EXCLUDED.unavailable WHERE mail_message_dates.unavailable`, key.AccountID, key.MailboxKey, key.Folder, int64(key.UIDValidity), uids, values, unavailable)
	if err != nil {
		return errors.New("mail date index update failed")
	}
	return nil
}

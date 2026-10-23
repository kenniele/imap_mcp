-- Immutable message dates only. Current UID membership still comes from IMAP.
CREATE TABLE IF NOT EXISTS mail_message_dates (
    account_id UUID NOT NULL REFERENCES mail_accounts(id) ON DELETE CASCADE,
    mailbox_key TEXT NOT NULL,
    folder TEXT NOT NULL,
    uid_validity BIGINT NOT NULL CHECK (uid_validity BETWEEN 1 AND 4294967295),
    uid BIGINT NOT NULL CHECK (uid BETWEEN 1 AND 4294967295),
    message_date TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (account_id, mailbox_key, folder, uid_validity, uid)
);

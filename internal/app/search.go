package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"mail-mcp/internal/domain"
	"mail-mcp/internal/secrets"

	"golang.org/x/sync/errgroup"
)

type MailProvider interface {
	Folders(context.Context, domain.Account) ([]domain.Folder, error)
	Search(context.Context, domain.Account, domain.SearchQuery, domain.Position) (domain.Page, error)
	Get(context.Context, domain.Account, string, uint32, uint32, bool) (domain.Content, error)
	Thread(context.Context, domain.Account, string, uint32, uint32, int) ([]domain.Content, bool, error)
	Test(context.Context, domain.Account) error
}
type Service struct {
	Store                          AccountStore
	Mail                           MailProvider
	Cipher                         *secrets.Cipher
	RequestTimeout, AccountTimeout time.Duration
}

func New(store AccountStore, mail MailProvider, cipher *secrets.Cipher) *Service {
	return &Service{store, mail, cipher, 15 * time.Second, 10 * time.Second}
}

type cursorState struct {
	Version   int                                   `json:"v"`
	Hash      string                                `json:"hash"`
	Expires   int64                                 `json:"expires"`
	Positions map[string]map[string]domain.Position `json:"positions"`
}

// MaxSearchCursorSize bounds authenticated pagination tokens across all folders.
const MaxSearchCursorSize = 1024 * 1024

type folderPage struct {
	folder string
	page   domain.Page
	err    error
	index  int
}

type accountPages struct {
	positions map[string]domain.Position
	pages     []folderPage
	err       error
}

func Normalize(q domain.SearchQuery) (domain.SearchQuery, error) {
	if q.Limit == 0 {
		q.Limit = 20
	}
	if q.Limit < 1 || q.Limit > 100 {
		return q, errors.New("limit must be between 1 and 100")
	}
	if len(q.Folder) > 512 || strings.ContainsAny(q.Folder, "\x00\r\n") {
		return q, errors.New("invalid folder")
	}
	for _, v := range []string{q.Query, q.From, q.To, q.Subject} {
		if len(v) > 1024 || strings.ContainsAny(v, "\x00\r\n") {
			return q, errors.New("search fields must be at most 1024 bytes without control characters")
		}
	}
	var after, before time.Time
	var err error
	if q.After != "" {
		after, err = time.Parse(time.RFC3339, q.After)
		if err != nil {
			return q, errors.New("after must be RFC3339")
		}
	}
	if q.Before != "" {
		before, err = time.Parse(time.RFC3339, q.Before)
		if err != nil {
			return q, errors.New("before must be RFC3339")
		}
	}
	if !after.IsZero() && !before.IsZero() && !after.Before(before) {
		return q, errors.New("after must precede before")
	}
	return q, nil
}
func (s *Service) Search(ctx context.Context, q domain.SearchQuery) (domain.SearchResult, error) {
	out := domain.SearchResult{Messages: []domain.Message{}, Errors: []domain.AccountError{}}
	q, err := Normalize(q)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.RequestTimeout)
	defer cancel()
	all, err := s.Store.List(ctx)
	if err != nil {
		return out, err
	}
	accounts, err := SelectAccounts(all, q.Accounts)
	if err != nil {
		return out, err
	}
	// Canonicalize aliases to immutable account IDs, binding pagination to the enabled account set.
	q.Accounts = []string{}
	for _, a := range accounts {
		q.Accounts = append(q.Accounts, a.ID)
	}
	sort.Strings(q.Accounts)
	token := q.Cursor
	q.Cursor = ""
	canonical, _ := json.Marshal(struct {
		Query   domain.SearchQuery
		Preview bool
	}{q, q.Preview})
	digest := sha256.Sum256(canonical)
	hash := hex.EncodeToString(digest[:])
	state := cursorState{Version: 2, Hash: hash, Expires: time.Now().Add(15 * time.Minute).Unix(), Positions: map[string]map[string]domain.Position{}}
	if token != "" {
		if len(token) > MaxSearchCursorSize {
			return out, errors.New("invalid cursor")
		}
		blob, e := base64.RawURLEncoding.DecodeString(token)
		if e != nil {
			return out, errors.New("invalid cursor")
		}
		plain, e := s.Cipher.Decrypt(blob, "mail-search-cursor-v2")
		if e != nil {
			return out, errors.New("invalid cursor")
		}
		if json.Unmarshal(plain, &state) != nil || state.Version != 2 || state.Hash != hash || state.Expires <= time.Now().Unix() || state.Positions == nil {
			return out, errors.New("expired cursor or changed search filters/accounts")
		}
	}
	results := make([]accountPages, len(accounts))
	var group errgroup.Group
	group.SetLimit(8)
	for i, a := range accounts {
		group.Go(func() error {
			accountCtx, stop := context.WithTimeout(ctx, s.AccountTimeout)
			defer stop()
			results[i] = s.searchAccount(accountCtx, a, q, state.Positions[a.ID])
			return nil // An account failure must never cancel its siblings.
		})
	}
	_ = group.Wait()
	type mailbox struct {
		account domain.Account
		*folderPage
	}
	mailboxes := []mailbox{}
	for i, a := range accounts {
		result := &results[i]
		state.Positions[a.ID] = result.positions
		if result.err != nil {
			out.Errors = append(out.Errors, domain.AccountError{Account: a.Alias, Error: safeError(result.err)})
		}
		for j := range result.pages {
			page := &result.pages[j]
			if page.err != nil {
				out.Errors = append(out.Errors, domain.AccountError{Account: a.Alias, Folder: page.folder, Error: safeError(page.err)})
				continue
			}
			mailboxes = append(mailboxes, mailbox{a, page})
		}
	}
	for len(out.Messages) < q.Limit {
		best := -1
		for i, m := range mailboxes {
			if m.index >= len(m.page.Messages) {
				continue
			}
			if best < 0 || m.page.Messages[m.index].Date.After(mailboxes[best].page.Messages[mailboxes[best].index].Date) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		m := mailboxes[best]
		msg := m.page.Messages[m.index]
		m.index++
		out.Messages = append(out.Messages, msg)
		state.Positions[m.account.ID][m.folder] = domain.Position{BeforeUID: msg.UID, UIDValidity: msg.UIDValidity}
	}
	for _, m := range mailboxes {
		if m.index == len(m.page.Messages) {
			state.Positions[m.account.ID][m.folder] = m.page.Next
		} else if m.index == 0 {
			state.Positions[m.account.ID][m.folder] = m.page.Start
		}
	}
	more := false
	for _, positions := range state.Positions {
		if positions == nil { // Folder discovery failed; retry on the next page.
			more = true
		}
		for _, pos := range positions {
			more = more || !pos.Done
		}
	}
	if more {
		plain, _ := json.Marshal(state)
		blob, e := s.Cipher.Encrypt(plain, "mail-search-cursor-v2")
		if e != nil {
			return out, errors.New("cursor encoding failed")
		}
		out.NextCursor = base64.RawURLEncoding.EncodeToString(blob)
		if len(out.NextCursor) > MaxSearchCursorSize {
			return out, errors.New("too many folders for pagination; narrow accounts or specify folder")
		}
	}
	return out, nil
}

func (s *Service) searchAccount(ctx context.Context, a domain.Account, q domain.SearchQuery, positions map[string]domain.Position) accountPages {
	result := accountPages{positions: positions}
	if positions == nil {
		folders := []domain.Folder{{Name: q.Folder}}
		if q.Folder == "" {
			var err error
			folders, err = s.Mail.Folders(ctx, a)
			if err != nil {
				result.err = err
				return result
			}
		}
		// Freeze folder membership along with each mailbox's UID boundary.
		result.positions = make(map[string]domain.Position, len(folders))
		for _, folder := range folders {
			result.positions[folder.Name] = domain.Position{}
		}
	}
	folders := make([]string, 0, len(result.positions))
	for folder := range result.positions {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	for _, folder := range folders {
		pos := result.positions[folder]
		if pos.Done {
			continue
		}
		if ctx.Err() != nil {
			result.err = ctx.Err()
			break
		}
		q.Folder = folder
		page, err := s.Mail.Search(ctx, a, q, pos)
		result.pages = append(result.pages, folderPage{folder: folder, page: page, err: err})
		if err != nil && ctx.Err() != nil {
			break // This folder's error already reports the expired account deadline.
		}
	}
	return result
}

func safeError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "imap connection timeout or request canceled"
	}
	// Providers expose only controlled errors. Do not echo raw server responses or login failures.
	switch err.Error() {
	case "mailbox UIDVALIDITY changed; start a new search", "message not found", "imap authentication failed", "imap TLS connection failed", "imap operation failed", "message MIME parsing failed":
		return err.Error()
	}
	return "imap account unavailable"
}

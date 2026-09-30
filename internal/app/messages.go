package app

import (
	"context"
	"errors"

	"mail-mcp/internal/domain"
)

func (s *Service) Folders(ctx context.Context, account string) ([]domain.Folder, error) {
	ctx, cancel := context.WithTimeout(ctx, s.AccountTimeout)
	defer cancel()
	a, err := s.account(ctx, account)
	if err != nil {
		return nil, err
	}
	out, err := s.Mail.Folders(ctx, a)
	if err != nil {
		return nil, errors.New(safeError(err))
	}
	return out, nil
}
func (s *Service) Get(ctx context.Context, account, folder string, uid, validity uint32, attachments bool) (domain.Content, error) {
	if uid == 0 {
		return domain.Content{}, errors.New("uid must be positive")
	}
	q, err := Normalize(domain.SearchQuery{Folder: folder})
	if err != nil {
		return domain.Content{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.AccountTimeout)
	defer cancel()
	a, err := s.account(ctx, account)
	if err != nil {
		return domain.Content{}, err
	}
	out, err := s.Mail.Get(ctx, a, q.Folder, uid, validity, attachments)
	if err != nil {
		return out, errors.New(safeError(err))
	}
	return out, nil
}

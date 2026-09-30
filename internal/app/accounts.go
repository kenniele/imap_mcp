package app

import (
	"context"
	"errors"
	"regexp"
	"sort"

	"mail-mcp/internal/domain"
)

type AccountStore interface {
	List(context.Context) ([]domain.Account, error)
}

var aliasPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func ValidAlias(alias string) bool { return aliasPattern.MatchString(alias) }
func SelectAccounts(all []domain.Account, selectors []string) ([]domain.Account, error) {
	enabled := []domain.Account{}
	for _, a := range all {
		if a.Enabled {
			enabled = append(enabled, a)
		}
	}
	if len(selectors) == 0 {
		if len(enabled) > 32 {
			return nil, errors.New("at most 32 enabled accounts are supported")
		}
		return enabled, nil
	}
	if len(selectors) > 32 {
		return nil, errors.New("at most 32 account selectors allowed")
	}
	out := []domain.Account{}
	seen := map[string]bool{}
	for _, selector := range selectors {
		found := false
		for _, a := range enabled {
			if a.Alias == selector || a.ID == selector {
				found = true
				if !seen[a.ID] {
					out = append(out, a)
					seen[a.ID] = true
				}
				break
			}
		}
		if !found {
			return nil, errors.New("unknown or disabled account")
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}
func (s *Service) Accounts(ctx context.Context) ([]domain.Account, error) {
	all, err := s.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	return SelectAccounts(all, nil)
}
func (s *Service) account(ctx context.Context, selector string) (domain.Account, error) {
	if selector == "" {
		return domain.Account{}, errors.New("account is required")
	}
	all, err := s.Store.List(ctx)
	if err != nil {
		return domain.Account{}, err
	}
	selected, err := SelectAccounts(all, []string{selector})
	if err != nil {
		return domain.Account{}, err
	}
	return selected[0], nil
}

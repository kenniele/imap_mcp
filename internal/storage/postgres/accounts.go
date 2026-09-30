package postgres

import (
	"context"
	"errors"

	"mail-mcp/internal/domain"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("account not found")

type Store struct{ DB *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	cfg.MaxConns = 10
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("database connection failed")
	}
	s := &Store{db}
	if err = s.Ping(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() { s.DB.Close() }
func (s *Store) Ping(ctx context.Context) error {
	if err := s.DB.Ping(ctx); err != nil {
		return errors.New("database unavailable")
	}
	return nil
}

const columns = "id::text, alias, email, provider, imap_host, imap_port, imap_tls, username, encrypted_secret, enabled, created_at, updated_at"

func (s *Store) List(ctx context.Context) ([]domain.Account, error) {
	rows, err := s.DB.Query(ctx, "SELECT "+columns+" FROM mail_accounts ORDER BY alias")
	if err != nil {
		return nil, errors.New("account database query failed")
	}
	defer rows.Close()
	accounts := []domain.Account{}
	for rows.Next() {
		var a domain.Account
		if err := rows.Scan(&a.ID, &a.Alias, &a.Email, &a.Provider, &a.IMAPHost, &a.IMAPPort, &a.IMAPTLS, &a.Username, &a.Secret, &a.Enabled, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, errors.New("account database decoding failed")
		}
		accounts = append(accounts, a)
	}
	if rows.Err() != nil {
		return nil, errors.New("account database query failed")
	}
	return accounts, nil
}
func (s *Store) Add(ctx context.Context, a domain.Account) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO mail_accounts (id,alias,email,provider,imap_host,imap_port,imap_tls,username,encrypted_secret) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, a.ID, a.Alias, a.Email, a.Provider, a.IMAPHost, a.IMAPPort, a.IMAPTLS, a.Username, a.Secret)
	if err != nil {
		return errors.New("cannot add account: alias may already exist")
	}
	return nil
}
func (s *Store) Disable(ctx context.Context, alias string) error {
	return s.change(ctx, "UPDATE mail_accounts SET enabled=FALSE, updated_at=NOW() WHERE alias=$1", alias)
}
func (s *Store) Delete(ctx context.Context, alias string) error {
	return s.change(ctx, "DELETE FROM mail_accounts WHERE alias=$1", alias)
}
func (s *Store) change(ctx context.Context, q, alias string) error {
	tag, err := s.DB.Exec(ctx, q, alias)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return errors.New("account database update failed")
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

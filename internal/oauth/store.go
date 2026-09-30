package oauth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var errInvalidGrant = errors.New("invalid OAuth grant")

type grant struct {
	Binding, Scope, RedirectURI, Challenge, CodeHash string
	ClientHash                                       string
	CodeExpiresAt                                    time.Time
}
type exchange struct {
	Kind, Hash, Binding, RedirectURI, Challenge string
	ClientHash                                  string
	RequestedScope                              string
	AccessHash, RefreshHash                     string
	AccessExpiresAt, RefreshExpiresAt           time.Time
}
type grantStore interface {
	SaveCode(context.Context, grant) error
	Exchange(context.Context, exchange) (string, error)
	Validate(context.Context, string, string) (string, error)
	Revoke(context.Context, string, string, string) error
}

type postgresStore struct{ pool *pgxpool.Pool }

func (s *postgresStore) SaveCode(ctx context.Context, g grant) error {
	// Tokens are random opaque secrets. Only SHA-256 digests reach storage.
	_, err := s.pool.Exec(ctx, `DELETE FROM mcp_oauth_grants
  WHERE GREATEST(code_expires_at,access_expires_at,refresh_expires_at) < now()`)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO mcp_oauth_grants
  (binding,scope,redirect_uri,code_challenge,code_hash,code_expires_at,client_hash)
  VALUES ($1,$2,$3,$4,$5,$6,$7)`, g.Binding, g.Scope, g.RedirectURI, g.Challenge, g.CodeHash, g.CodeExpiresAt, g.ClientHash)
	return err
}
func (s *postgresStore) Exchange(ctx context.Context, e exchange) (string, error) {
	var scope string
	var err error
	switch e.Kind {
	case "authorization_code":
		err = s.pool.QueryRow(ctx, `UPDATE mcp_oauth_grants SET code_hash=NULL,
   access_hash=$6,access_expires_at=$7,refresh_hash=NULLIF($8,''),refresh_expires_at=$9,scope=CASE WHEN $10='' THEN scope ELSE $10 END
   WHERE code_hash=$1 AND binding=$2 AND redirect_uri=$3 AND code_challenge=$4
    AND code_expires_at>$5 AND client_hash=$11 AND ($10='' OR string_to_array($10,' ') <@ string_to_array(scope,' '))
   RETURNING scope`, e.Hash, e.Binding, e.RedirectURI, e.Challenge, time.Now(), e.AccessHash, e.AccessExpiresAt, e.RefreshHash, e.RefreshExpiresAt, e.RequestedScope, e.ClientHash).Scan(&scope)
	case "refresh_token":
		// One atomic update consumes the old refresh token and invalidates the old
		// access token. The original refresh expiry is retained across rotations.
		err = s.pool.QueryRow(ctx, `UPDATE mcp_oauth_grants SET access_hash=$3,
   access_expires_at=$4,refresh_hash=$5,scope=CASE WHEN $6='' THEN scope ELSE $6 END
   WHERE refresh_hash=$1 AND binding=$2 AND client_hash=$7 AND refresh_expires_at>now()
    AND 'offline_access'=ANY(string_to_array(scope,' '))
AND ($6='' OR string_to_array($6,' ') <@ string_to_array(scope,' '))
   RETURNING scope`, e.Hash, e.Binding, e.AccessHash, e.AccessExpiresAt, e.RefreshHash, e.RequestedScope, e.ClientHash).Scan(&scope)
	default:
		return "", errInvalidGrant
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errInvalidGrant
	}
	return scope, err
}
func (s *postgresStore) Validate(ctx context.Context, hash, binding string) (string, error) {
	var scope string
	err := s.pool.QueryRow(ctx, `SELECT scope FROM mcp_oauth_grants
  WHERE access_hash=$1 AND binding=$2 AND access_expires_at>now()`, hash, binding).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errInvalidGrant
	}
	return scope, err
}
func (s *postgresStore) Revoke(ctx context.Context, hash, binding, clientHash string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM mcp_oauth_grants WHERE binding=$1 AND client_hash=$3 AND (access_hash=$2 OR refresh_hash=$2)`, binding, hash, clientHash)
	return err
}

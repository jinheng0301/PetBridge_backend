package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

type AuthIdentityRepo struct{ pool *pgxpool.Pool }

func NewAuthIdentityRepo(pool *pgxpool.Pool) *AuthIdentityRepo { return &AuthIdentityRepo{pool: pool} }

// FindUserID returns the user behind (provider, subject), or model.ErrNotFound.
func (r *AuthIdentityRepo) FindUserID(ctx context.Context, provider, subject string) (string, error) {
	var id string
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select user_id::text from auth_identities where provider=$1 and subject=$2`, provider, subject).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", model.ErrNotFound
	}
	return id, err
}

// Create links (provider, subject) to userID. A concurrent first login for the
// same identity returns model.ErrAlreadyExists.
func (r *AuthIdentityRepo) Create(ctx context.Context, provider, subject, userID string) error {
	_, err := conn(ctx, r.pool).Exec(ctx,
		`insert into auth_identities(provider, subject, user_id) values($1,$2,$3)`, provider, subject, userID)
	if IsUniqueViolation(err) {
		return model.ErrAlreadyExists
	}
	return err
}

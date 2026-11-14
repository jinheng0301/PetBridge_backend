package repository

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const profileCols = `id::text, nickname, native_lang, mbti, pet_type, age_band, tz, onboarded_at is not null, status`

type UserRepo struct{ pool *pgxpool.Pool }

func NewUserRepo(pool *pgxpool.Pool) *UserRepo { return &UserRepo{pool: pool} }

// CreateEmpty inserts a bare users row (login only creates this; profile
// comes from onboarding).
func (r *UserRepo) CreateEmpty(ctx context.Context) (string, error) {
	var id string
	err := conn(ctx, r.pool).QueryRow(ctx, `insert into users default values returning id::text`).Scan(&id)
	return id, err
}

// State returns the account status and whether onboarding is complete.
func (r *UserRepo) State(ctx context.Context, userID string) (status string, onboarded bool, err error) {
	err = conn(ctx, r.pool).QueryRow(ctx, `select status, onboarded_at is not null from users where id=$1`, userID).Scan(&status, &onboarded)
	return
}

// LoadProfile returns the profile and the account status. lock adds
// FOR UPDATE, which serialises concurrent onboarding of the same user. A
// malformed id returns ErrNotFound (keeps the id index usable, avoids a cast error).
func (r *UserRepo) LoadProfile(ctx context.Context, userID string, lock bool) (model.Profile, string, error) {
	if !uuidRE.MatchString(userID) {
		return model.Profile{}, "", model.ErrNotFound
	}
	var p model.Profile
	var status string
	sql := `select ` + profileCols + ` from users where id=$1`
	if lock {
		sql += ` for update`
	}
	err := conn(ctx, r.pool).QueryRow(ctx, sql, userID).
		Scan(&p.ID, &p.Nickname, &p.NativeLang, &p.MBTI, &p.PetType, &p.AgeBand, &p.TZ, &p.Onboarded, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Profile{}, "", model.ErrNotFound
	}
	if err != nil {
		return model.Profile{}, "", err
	}
	return p, status, nil
}

// CompleteOnboarding writes the profile columns and marks onboarding done.
func (r *UserRepo) CompleteOnboarding(ctx context.Context, userID string, in model.OnboardingInput, pet string, at time.Time) error {
	_, err := conn(ctx, r.pool).Exec(ctx, `update users set nickname=$2, native_lang=$3, mbti=$4, pet_type=$5, age_band=$6, tz=$7, onboarded_at=$8
		where id=$1`, userID, in.Nickname, in.NativeLang, in.MBTI, pet, in.AgeBand, in.TZ, at)
	return err
}

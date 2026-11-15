package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/service"
)

const (
	zeroCode     = "DEVA2345"
	zeroCodeLife = 10 * 365 * 24 * time.Hour
	seedCodeTTL  = 7

	fullOwnerEmail = "dev-full@local"
	fullVoidCode   = "FULL2345"
)

// fullRoomCodes seat the four fill users in dev-full's room.
var fullRoomCodes = []struct{ code, email string }{
	{"FULA2345", "dev-fill1@local"},
	{"FULB2345", "dev-fill2@local"},
	{"FULC2345", "dev-fill3@local"},
	{"FULD2345", "dev-fill4@local"},
}

type seedUser struct {
	email string
	in    model.OnboardingInput
}

var seedUsers = []seedUser{
	{"dev-a@local", model.OnboardingInput{NativeLang: "ja", MBTI: "ENFP", Nickname: "Dev A", AgeBand: "18_plus", TZ: "Asia/Tokyo"}},
	{"dev-b@local", model.OnboardingInput{NativeLang: "ko", MBTI: "INFP", Nickname: "Dev B", AgeBand: "18_plus", TZ: "Asia/Seoul"}},
	{"dev-c@local", model.OnboardingInput{NativeLang: "zh-Hant", MBTI: "INTJ", Nickname: "Dev C", AgeBand: "18_plus", TZ: "Asia/Taipei"}},
	{"dev-d@local", model.OnboardingInput{NativeLang: "zh-Hans", MBTI: "ESTJ", Nickname: "Dev D", AgeBand: "18_plus", TZ: "Asia/Shanghai"}},
	{"dev-e@local", model.OnboardingInput{NativeLang: "en", MBTI: "ISFP", Nickname: "Dev E", AgeBand: "18_plus", TZ: "Europe/London"}},
	{"dev-f@local", model.OnboardingInput{NativeLang: "en", MBTI: "ESFP", Nickname: "Dev F", AgeBand: "13_17", TZ: "Europe/London"}},
	{"dev-full@local", model.OnboardingInput{NativeLang: "en", MBTI: "ENTJ", Nickname: "Dev Full", AgeBand: "18_plus", TZ: "Europe/London"}},
	{"dev-fill1@local", model.OnboardingInput{NativeLang: "en", MBTI: "INFJ", Nickname: "Dev Fill 1", AgeBand: "18_plus", TZ: "Europe/London"}},
	{"dev-fill2@local", model.OnboardingInput{NativeLang: "ja", MBTI: "ISTP", Nickname: "Dev Fill 2", AgeBand: "18_plus", TZ: "Europe/London"}},
	{"dev-fill3@local", model.OnboardingInput{NativeLang: "ko", MBTI: "ENTP", Nickname: "Dev Fill 3", AgeBand: "18_plus", TZ: "Europe/London"}},
	{"dev-fill4@local", model.OnboardingInput{NativeLang: "zh-Hant", MBTI: "ISFJ", Nickname: "Dev Fill 4", AgeBand: "18_plus", TZ: "Europe/London"}},
}

// newJoinService builds a JoinService with an in-process guard and no event publishing.
func newJoinService(pool *pgxpool.Pool, now func() time.Time) *service.JoinService {
	return service.NewJoinService(repository.NewUserRepo(pool), repository.NewRoomRepo(pool),
		repository.NewRoomMemberRepo(pool), repository.NewInviteCodeRepo(pool), repository.NewBanRepo(pool),
		repository.NewRewardRepo(pool), repository.NewTxManager(pool), ratelimit.NewJoinGuard(ratelimit.NewMemory(time.Now)),
		service.NopPublisher{}, service.JoinConfig{CodeTTLDays: seedCodeTTL, RewardCount: 3, RewardLifetimeCap: 20}, now)
}

// Run creates the eleven seed users through the real onboarding path, the
// zero code DEVA2345 on user A's room, and a full room (dev-full plus four
// fill users seated through real joins) whose last zero code FULL2345 is void.
// It is safe to run repeatedly: on a re-run the fill users' codes are already
// used by them, so Join takes its replay path and succeeds.
func Run(ctx context.Context, pool *pgxpool.Pool, now func() time.Time) error {
	// Seed never needs a usable token; the issuer is a throwaway.
	tx := repository.NewTxManager(pool)
	users := repository.NewUserRepo(pool)
	codes := repository.NewInviteCodeRepo(pool)
	authSvc := service.NewAuthService(users, repository.NewAuthIdentityRepo(pool), repository.NewRefreshTokenRepo(pool), tx,
		service.NewIssuer("seed-throwaway-key-seed-throwaway", time.Minute, now), time.Hour)
	userSvc := service.NewUserService(users, repository.NewRoomRepo(pool), repository.NewRoomMemberRepo(pool), codes, tx, seedCodeTTL, now)

	var aRoom, fullRoom string
	ids := map[string]string{}
	for _, s := range seedUsers {
		uid, err := authSvc.EnsureUser(ctx, "dev", s.email)
		if err != nil {
			return fmt.Errorf("%s: %w", s.email, err)
		}
		res, err := userSvc.Onboard(ctx, uid, s.in)
		if err != nil {
			return fmt.Errorf("%s: %w", s.email, err)
		}
		ids[s.email] = uid
		switch s.email {
		case "dev-a@local":
			aRoom = res.Room.ID
		case fullOwnerEmail:
			fullRoom = res.Room.ID
		}
	}
	if err := codes.InsertZero(ctx, zeroCode, aRoom, now().Add(zeroCodeLife)); err != nil {
		return err
	}

	joins := newJoinService(pool, now)
	for _, fc := range fullRoomCodes {
		if err := codes.InsertZero(ctx, fc.code, fullRoom, now().Add(zeroCodeLife)); err != nil {
			return fmt.Errorf("%s: %w", fc.code, err)
		}
		if _, err := joins.Join(ctx, ids[fc.email], "127.0.0.1", fc.code); err != nil {
			return fmt.Errorf("%s joins with %s: %w", fc.email, fc.code, err)
		}
	}
	return codes.InsertVoidZero(ctx, fullVoidCode, fullRoom, now().Add(zeroCodeLife))
}

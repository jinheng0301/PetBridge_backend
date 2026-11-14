package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/service"
)

const (
	zeroCode     = "DEVA2345"
	zeroCodeLife = 10 * 365 * 24 * time.Hour
	seedCodeTTL  = 7
)

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
}

// Run creates the six seed users through the real onboarding path and the
// zero code DEVA2345 on user A's room. It is safe to run repeatedly.
func Run(ctx context.Context, pool *pgxpool.Pool, now func() time.Time) error {
	// Seed never needs a usable token; the issuer is a throwaway.
	tx := repository.NewTxManager(pool)
	users := repository.NewUserRepo(pool)
	codes := repository.NewInviteCodeRepo(pool)
	authSvc := service.NewAuthService(users, repository.NewAuthIdentityRepo(pool), repository.NewRefreshTokenRepo(pool), tx,
		service.NewIssuer("seed-throwaway-key-seed-throwaway", time.Minute, now), time.Hour)
	userSvc := service.NewUserService(users, repository.NewRoomRepo(pool), repository.NewRoomMemberRepo(pool), codes, tx, seedCodeTTL, now)

	var aRoom string
	for _, s := range seedUsers {
		uid, err := authSvc.EnsureUser(ctx, "dev", s.email)
		if err != nil {
			return fmt.Errorf("%s: %w", s.email, err)
		}
		res, err := userSvc.Onboard(ctx, uid, s.in)
		if err != nil {
			return fmt.Errorf("%s: %w", s.email, err)
		}
		if s.email == "dev-a@local" {
			aRoom = res.Room.ID
		}
	}
	return codes.InsertZero(ctx, zeroCode, aRoom, now().Add(zeroCodeLife))
}

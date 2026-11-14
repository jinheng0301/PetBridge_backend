package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/auth"
	"github.com/jinheng0301/petbridge/internal/user"
)

const (
	zeroCode     = "DEVA2345"
	zeroCodeLife = 10 * 365 * 24 * time.Hour
	seedCodeTTL  = 7
)

type seedUser struct {
	email string
	in    user.OnboardingInput
}

var seedUsers = []seedUser{
	{"dev-a@local", user.OnboardingInput{NativeLang: "ja", MBTI: "ENFP", Nickname: "Dev A", AgeBand: "18_plus", TZ: "Asia/Tokyo"}},
	{"dev-b@local", user.OnboardingInput{NativeLang: "ko", MBTI: "INFP", Nickname: "Dev B", AgeBand: "18_plus", TZ: "Asia/Seoul"}},
	{"dev-c@local", user.OnboardingInput{NativeLang: "zh-Hant", MBTI: "INTJ", Nickname: "Dev C", AgeBand: "18_plus", TZ: "Asia/Taipei"}},
	{"dev-d@local", user.OnboardingInput{NativeLang: "zh-Hans", MBTI: "ESTJ", Nickname: "Dev D", AgeBand: "18_plus", TZ: "Asia/Shanghai"}},
	{"dev-e@local", user.OnboardingInput{NativeLang: "en", MBTI: "ISFP", Nickname: "Dev E", AgeBand: "18_plus", TZ: "Europe/London"}},
	{"dev-f@local", user.OnboardingInput{NativeLang: "en", MBTI: "ESFP", Nickname: "Dev F", AgeBand: "13_17", TZ: "Europe/London"}},
}

// Run creates the six seed users through the real onboarding path and the
// zero code DEVA2345 on user A's room. It is safe to run repeatedly.
func Run(ctx context.Context, pool *pgxpool.Pool, now func() time.Time) error {
	// Seed never needs a usable token; the issuer is a throwaway.
	authSvc := auth.NewService(pool, auth.NewIssuer("seed-throwaway-key-seed-throwaway", time.Minute, now), time.Hour)
	userSvc := user.NewService(pool, seedCodeTTL, now)

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
	_, err := pool.Exec(ctx, `insert into invite_codes(code, room_id, source, expires_at) values($1,$2,'zero',$3)
		on conflict (code) do nothing`, zeroCode, aRoom, now().Add(zeroCodeLife))
	return err
}

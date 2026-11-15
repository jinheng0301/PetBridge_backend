package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// RewardCount is how many join-reward codes to issue (design 5.2):
// max(0, min(configured, freeSlots-unusedValid, lifetimeLeft)). Any negative
// input yields 0.
func RewardCount(configured, freeSlots, unusedValid, lifetimeLeft int) int {
	if configured < 0 || freeSlots < 0 || unusedValid < 0 || lifetimeLeft < 0 {
		return 0
	}
	return max(0, min(configured, freeSlots-unusedValid, lifetimeLeft))
}

// grantRewards records the one-time reward for joining joinedRoom and issues
// the codes into the joiner's own room. own must already be locked, so the
// unused and lifetime counts read here are stable. The result is never nil
// and is sorted by code, the order RewardsFor reads it back in on replay.
func (s *JoinService) grantRewards(ctx context.Context, userID, joinedRoom string, own model.RoomState,
	forCode string, now time.Time) ([]model.CodeInfo, error) {
	unused, err := s.codes.CountUnusedValid(ctx, own.ID, now)
	if err != nil {
		return nil, err
	}
	lifetime, err := s.codes.LifetimeCount(ctx, userID)
	if err != nil {
		return nil, err
	}
	n := RewardCount(s.cfg.RewardCount, own.Capacity-own.MemberCount, unused, s.cfg.RewardLifetimeCap-lifetime)
	// The grant row is written even when n is 0, so leaving and rejoining the
	// same room later cannot farm a reward.
	inserted, err := s.rewards.Grant(ctx, userID, joinedRoom, n)
	if err != nil {
		return nil, err
	}
	out := []model.CodeInfo{}
	if !inserted {
		return out, nil
	}
	// Microsecond precision and UTC: the same value a replay reads back from the database.
	expires := now.UTC().Add(time.Duration(s.cfg.CodeTTLDays) * 24 * time.Hour).Truncate(time.Microsecond)
	for i := 0; i < n; i++ {
		code, err := s.insertReward(ctx, own.ID, userID, forCode, expires)
		if err != nil {
			return nil, err
		}
		out = append(out, model.CodeInfo{Code: code, ExpiresAt: expires})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, nil
}

// insertReward retries on primary-key collisions.
func (s *JoinService) insertReward(ctx context.Context, roomID, userID, forCode string, expires time.Time) (string, error) {
	for i := 0; i < maxCodeAttempts; i++ {
		code, err := s.newCode()
		if err != nil {
			return "", err
		}
		inserted, err := s.codes.InsertReward(ctx, code, roomID, userID, forCode, expires)
		if err != nil {
			return "", err
		}
		if inserted {
			return code, nil
		}
	}
	return "", errors.New("could not allocate a unique reward code")
}

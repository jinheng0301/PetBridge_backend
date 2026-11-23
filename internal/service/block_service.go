package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jinheng0301/petbridge/internal/model"
)

// blockLimit caps how many users one user may block (spec 6.2).
const blockLimit = 500

// BlockService manages a user's block list (spec 6). Blocking sends no
// event: the blocked user must never be able to tell.
type BlockService struct {
	users  UserStore
	blocks BlockStore
}

func NewBlockService(users UserStore, blocks BlockStore) *BlockService {
	return &BlockService{users: users, blocks: blocks}
}

// Block adds targetID to userID's list. Blocking someone already blocked is a
// no-op and does not count against the limit.
func (s *BlockService) Block(ctx context.Context, userID, targetID string) error {
	if err := s.requireOnboarded(ctx, userID); err != nil {
		return err
	}
	targetID = strings.ToLower(targetID)
	if !isUUID(targetID) || targetID == userID {
		return badUserID()
	}
	_, onboarded, err := s.users.State(ctx, targetID)
	if errors.Is(err, model.ErrNotFound) {
		return badUserID()
	}
	if err != nil {
		return fmt.Errorf("target state: %w", err)
	}
	if !onboarded {
		return badUserID()
	}
	already, err := s.blocks.IsBlocking(ctx, userID, targetID)
	if err != nil {
		return fmt.Errorf("is blocking: %w", err)
	}
	if already {
		return nil
	}
	n, err := s.blocks.Count(ctx, userID)
	if err != nil {
		return fmt.Errorf("count blocks: %w", err)
	}
	if n >= blockLimit {
		return ErrBlockLimit
	}
	if err := s.blocks.Block(ctx, userID, targetID); err != nil {
		return fmt.Errorf("block: %w", err)
	}
	return nil
}

// Unblock removes targetID from userID's list; it is a no-op when not blocked.
func (s *BlockService) Unblock(ctx context.Context, userID, targetID string) error {
	if err := s.requireOnboarded(ctx, userID); err != nil {
		return err
	}
	targetID = strings.ToLower(targetID)
	if !isUUID(targetID) {
		return badUserID()
	}
	if err := s.blocks.Unblock(ctx, userID, targetID); err != nil {
		return fmt.Errorf("unblock: %w", err)
	}
	return nil
}

// List returns userID's block list, newest first.
func (s *BlockService) List(ctx context.Context, userID string) ([]model.BlockedUser, error) {
	if err := s.requireOnboarded(ctx, userID); err != nil {
		return nil, err
	}
	out, err := s.blocks.List(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list blocks: %w", err)
	}
	if out == nil {
		out = []model.BlockedUser{}
	}
	return out, nil
}

func (s *BlockService) requireOnboarded(ctx context.Context, userID string) error {
	_, onboarded, err := s.users.State(ctx, userID)
	if errors.Is(err, model.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("user state: %w", err)
	}
	if !onboarded {
		return ErrOnboardingRequired
	}
	return nil
}

func badUserID() error { return &ValidationError{Fields: []string{"user_id"}} }

// blockedBetween reports a block in either direction; a nil store never blocks.
func blockedBetween(ctx context.Context, blocks BlockStore, a, b string) (bool, error) {
	if blocks == nil {
		return false, nil
	}
	return blocks.Between(ctx, a, b)
}

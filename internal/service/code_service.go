package service

import (
	"context"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// CodeService answers "my codes" queries.
type CodeService struct {
	codes InviteCodeStore
	now   func() time.Time
}

func NewCodeService(codes InviteCodeStore, now func() time.Time) *CodeService {
	return &CodeService{codes: codes, now: now}
}

// List returns the user's codes newest first with the effective status; never nil.
func (s *CodeService) List(ctx context.Context, userID string) ([]model.CodeListItem, error) {
	items, err := s.codes.ListForUser(ctx, userID, s.now())
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []model.CodeListItem{}
	}
	return items, nil
}

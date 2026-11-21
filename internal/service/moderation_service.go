package service

import "context"

// ModVerdict is a moderation decision.
type ModVerdict int

const (
	ModPass ModVerdict = iota
	ModReview
	ModBlock
)

// Moderator checks outgoing text before it is stored and translated text
// before it is shown (spec 5.5 step 5). B5 passes everything; B6 replaces it.
type Moderator interface {
	CheckOutgoing(ctx context.Context, text, lang string) (ModVerdict, error)
	CheckTranslated(ctx context.Context, text, lang, risk string) (ModVerdict, error)
}

// PassModerator lets every message through.
type PassModerator struct{}

var _ Moderator = PassModerator{}

func (PassModerator) CheckOutgoing(context.Context, string, string) (ModVerdict, error) {
	return ModPass, nil
}

func (PassModerator) CheckTranslated(context.Context, string, string, string) (ModVerdict, error) {
	return ModPass, nil
}

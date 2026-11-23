package service

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jinheng0301/petbridge/pkg/wordfilter"
)

// ModVerdict is a moderation decision.
type ModVerdict int

const (
	ModPass ModVerdict = iota
	ModReview
	ModBlock
)

// Sources of a moderation decision.
const (
	ModSourceWordlist = "wordlist"
	ModSourceModel    = "model"
	ModSourceBoth     = "both"
)

// maxModTerms caps the matched terms kept on a result.
const maxModTerms = 10

// ModResult is the outcome of one moderation check.
type ModResult struct {
	Verdict ModVerdict // final decision
	Model   ModVerdict // the AI model's own verdict (always ModPass for CheckOutgoing)
	Source  string     // "" when Verdict is ModPass
	Terms   []string   // matched word-list terms, at most maxModTerms
}

// Moderator checks outgoing text before it is stored and translated text
// before it is shown (spec 5.5 step 5).
type Moderator interface {
	CheckOutgoing(ctx context.Context, text, lang string) (ModResult, error)
	CheckTranslated(ctx context.Context, text, lang, risk string) (ModResult, error)
}

// PassModerator lets every message through.
type PassModerator struct{}

var _ Moderator = PassModerator{}

func (PassModerator) CheckOutgoing(context.Context, string, string) (ModResult, error) {
	return ModResult{}, nil
}

func (PassModerator) CheckTranslated(context.Context, string, string, string) (ModResult, error) {
	return ModResult{}, nil
}

//go:embed modwords/*.txt
var modwordsFS embed.FS

// WordlistModerator moderates with the embedded word lists, and for
// translated text also folds in the model's own risk rating.
type WordlistModerator struct {
	f *wordfilter.Filter
}

var _ Moderator = (*WordlistModerator)(nil)

func NewWordlistModerator(f *wordfilter.Filter) *WordlistModerator {
	return &WordlistModerator{f: f}
}

// LoadModerator builds one filter from every embedded list.
func LoadModerator() (*WordlistModerator, error) {
	files, err := fs.Glob(modwordsFS, "modwords/*.txt")
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no embedded word lists")
	}
	var all []wordfilter.Entry
	for _, name := range files {
		r, err := modwordsFS.Open(name)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", name, err)
		}
		entries, err := wordfilter.ParseList(r)
		r.Close()
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		all = append(all, entries...)
	}
	return NewWordlistModerator(wordfilter.New(all)), nil
}

func (m *WordlistModerator) CheckOutgoing(_ context.Context, text, _ string) (ModResult, error) {
	return m.merge(text, ModPass), nil
}

func (m *WordlistModerator) CheckTranslated(_ context.Context, text, _, risk string) (ModResult, error) {
	model := ModPass
	switch risk {
	case "block":
		model = ModBlock
	case "review":
		model = ModReview
	}
	return m.merge(text, model), nil
}

func (m *WordlistModerator) merge(text string, model ModVerdict) ModResult {
	lv, terms := m.f.Match(text)
	var wl ModVerdict
	switch lv {
	case wordfilter.Block:
		wl = ModBlock
	case wordfilter.Review:
		wl = ModReview
	}
	res := ModResult{Verdict: max(wl, model), Model: model}
	switch {
	case wl != ModPass && model != ModPass:
		res.Source = ModSourceBoth
	case wl != ModPass:
		res.Source = ModSourceWordlist
	case model != ModPass:
		res.Source = ModSourceModel
	}
	if wl != ModPass {
		if len(terms) > maxModTerms {
			terms = terms[:maxModTerms]
		}
		res.Terms = terms
	}
	return res
}

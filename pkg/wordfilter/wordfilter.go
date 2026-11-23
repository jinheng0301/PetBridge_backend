// Package wordfilter matches a multilingual word list against text using
// Aho-Corasick. It is business-agnostic.
package wordfilter

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
)

type Level int

const (
	None Level = iota
	Review
	Block
)

const maxTerms = 10

type Entry struct {
	Term  string
	Level Level
}

// ParseList reads lines of "block\tterm" / "review\tterm". Blank lines and
// lines starting with '#' are skipped.
func ParseList(r io.Reader) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lv, term, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("wordfilter: line %d: want \"level<TAB>term\"", n)
		}
		var level Level
		switch strings.TrimSpace(lv) {
		case "block":
			level = Block
		case "review":
			level = Review
		default:
			return nil, fmt.Errorf("wordfilter: line %d: unknown level %q", n, lv)
		}
		out = append(out, Entry{Term: strings.TrimSpace(term), Level: level})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Filter is immutable and safe for concurrent use.
type Filter struct {
	ascii *automaton // whole-word matching on normalized text
	loose *automaton // separator-insensitive matching
}

func isASCIIAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

func isSeparator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(".-_*·・、,，。", r)
}

func stripSeparators(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !isSeparator(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func allASCIIAlnum(s string) bool {
	for _, r := range s {
		if !isASCIIAlnum(r) {
			return false
		}
	}
	return s != ""
}

// New builds a Filter. Duplicate terms keep the highest level; terms that
// normalize to empty are ignored.
func New(entries []Entry) *Filter {
	asciiTerms := map[string]Level{}
	looseTerms := map[string]Level{}
	for _, e := range entries {
		t := Normalize(e.Term)
		m := looseTerms
		if allASCIIAlnum(t) {
			m = asciiTerms
		} else {
			t = stripSeparators(t)
		}
		if t == "" {
			continue
		}
		if e.Level > m[t] {
			m[t] = e.Level
		}
	}
	return &Filter{ascii: buildAutomaton(asciiTerms), loose: buildAutomaton(looseTerms)}
}

type hit struct {
	term  string
	level Level
	pos   int // start rune index in the normalized text
}

// Match returns the highest level hit and the matched terms (normalized,
// deduplicated, in order of first appearance, at most 10).
func (f *Filter) Match(text string) (Level, []string) {
	norm := []rune(Normalize(text))
	var hits []hit

	f.ascii.scan(norm, func(start, end int, term string, lv Level) {
		if start > 0 && isASCIIAlnum(norm[start-1]) {
			return
		}
		if end < len(norm) && isASCIIAlnum(norm[end]) {
			return
		}
		hits = append(hits, hit{term, lv, start})
	})

	if f.loose.size > 0 {
		stripped := make([]rune, 0, len(norm))
		origin := make([]int, 0, len(norm))
		for i, r := range norm {
			if !isSeparator(r) {
				stripped = append(stripped, r)
				origin = append(origin, i)
			}
		}
		f.loose.scan(stripped, func(start, _ int, term string, lv Level) {
			hits = append(hits, hit{term, lv, origin[start]})
		})
	}

	if len(hits) == 0 {
		return None, nil
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	level := None
	seen := map[string]bool{}
	var terms []string
	for _, h := range hits {
		if h.level > level {
			level = h.level
		}
		if !seen[h.term] && len(terms) < maxTerms {
			seen[h.term] = true
			terms = append(terms, h.term)
		}
	}
	return level, terms
}

// automaton is a rune-level Aho-Corasick machine.
type automaton struct {
	next  []map[rune]int
	fail  []int
	out   []int // index into terms, -1 if none
	dict  []int // nearest suffix node with out >= 0, -1 if none
	terms []string
	lens  []int // rune length of each term
	level []Level
	size  int
}

func buildAutomaton(terms map[string]Level) *automaton {
	a := &automaton{size: len(terms)}
	a.newNode()
	keys := make([]string, 0, len(terms))
	for t := range terms {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	for _, t := range keys {
		n := 0
		for _, r := range t {
			nx, ok := a.next[n][r]
			if !ok {
				nx = a.newNode()
				a.next[n][r] = nx
			}
			n = nx
		}
		a.out[n] = len(a.terms)
		a.terms = append(a.terms, t)
		a.lens = append(a.lens, len([]rune(t)))
		a.level = append(a.level, terms[t])
	}
	// BFS to compute fail and dict links.
	var queue []int
	for _, c := range a.next[0] {
		queue = append(queue, c)
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for r, c := range a.next[n] {
			f := a.fail[n]
			for f != 0 {
				if _, ok := a.next[f][r]; ok {
					break
				}
				f = a.fail[f]
			}
			if t, ok := a.next[f][r]; ok && t != c {
				a.fail[c] = t
			}
			if a.out[a.fail[c]] >= 0 {
				a.dict[c] = a.fail[c]
			} else {
				a.dict[c] = a.dict[a.fail[c]]
			}
			queue = append(queue, c)
		}
	}
	return a
}

func (a *automaton) newNode() int {
	a.next = append(a.next, map[rune]int{})
	a.fail = append(a.fail, 0)
	a.out = append(a.out, -1)
	a.dict = append(a.dict, -1)
	return len(a.next) - 1
}

// scan calls fn for every occurrence; start/end are rune indices, end exclusive.
func (a *automaton) scan(text []rune, fn func(start, end int, term string, lv Level)) {
	if a.size == 0 {
		return
	}
	n := 0
	for i, r := range text {
		for n != 0 {
			if _, ok := a.next[n][r]; ok {
				break
			}
			n = a.fail[n]
		}
		if t, ok := a.next[n][r]; ok {
			n = t
		}
		for m := n; m >= 0; m = a.dict[m] {
			if a.out[m] >= 0 {
				idx := a.out[m]
				l := a.lens[idx]
				fn(i+1-l, i+1, a.terms[idx], a.level[idx])
			}
			if a.dict[m] < 0 {
				break
			}
		}
	}
}

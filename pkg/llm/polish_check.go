package llm

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Polish check names (backend TRD 10.3.7 items 2-5).
const (
	CheckFacts  = "facts"
	CheckLength = "length"
	CheckEmoji  = "emoji"
	CheckMBTI   = "mbti"
)

// Length tolerance per tier, and the floor in runes so a short sentence can
// still gain a particle and a mark.
var tierLengthRatio = map[string]float64{TierLight: 0.3}

const lengthFloorRunes = 4

var (
	digitRunRE = regexp.MustCompile(`[0-9]+(?:[:./\-][0-9]+)*`)
	// A CJK numeral counts only right before a unit, so 一下 / 一起 are not numbers.
	cjkNumUnitRE = regexp.MustCompile(`([〇零一二三四五六七八九十百千万两]+)([点時时分秒日号號月年岁歳元円块個个人시분일월년원])`)
	urlRE        = regexp.MustCompile(`https?://\S+`)
	// A Latin handle stops where the script changes; a CJK name glued to the
	// next words is compared by its first two characters only.
	mentionRE = regexp.MustCompile(`@(?:[A-Za-z0-9_.]+|[\p{Han}\p{Hiragana}\p{Katakana}\p{Hangul}]+)`)
	mbtiRE    = regexp.MustCompile(`(?i)[EI][NS][TF][JP]`)
)

const kaomojiRunes = "ωД▽∀＾´｀≧≦◕‿ﾟ｡"

// CheckPolish returns the first rule the polished text breaks against the base
// translation: "facts", "length", "emoji" or "mbti"; "" when it passes.
func CheckPolish(base, polished, tier string) string {
	if !sameMultiset(facts(base), facts(polished)) {
		return CheckFacts
	}
	ratio, ok := tierLengthRatio[tier]
	if !ok {
		ratio = tierLengthRatio[TierLight]
	}
	b, p := utf8.RuneCountInString(base), utf8.RuneCountInString(polished)
	if math.Abs(float64(p-b)) > math.Max(ratio*float64(b), lengthFloorRunes) {
		return CheckLength
	}
	if addsEmoji(base, polished) {
		return CheckEmoji
	}
	baseTypes := mbtiNames(base)
	for t := range mbtiNames(polished) {
		if !baseTypes[t] {
			return CheckMBTI
		}
	}
	return ""
}

// facts extracts numbers (normalised), URLs, @mentions and currency symbols.
func facts(s string) []string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= '０' && r <= '９':
			return '0' + (r - '０')
		case r == '：':
			return ':'
		case r == '．':
			return '.'
		case r == '／':
			return '/'
		case r == '￥':
			return '¥'
		}
		return r
	}, s)
	var out []string
	for _, u := range urlRE.FindAllString(s, -1) {
		out = append(out, strings.TrimRightFunc(u, func(r rune) bool {
			return unicode.IsPunct(r) && r != '/' && r != '-' && r != '_'
		}))
	}
	s = urlRE.ReplaceAllString(s, " ")
	for _, m := range mentionRE.FindAllString(s, -1) {
		if r := []rune(m); len(r) > 3 && !unicode.Is(unicode.Latin, r[1]) && (r[1] < '0' || r[1] > '9') {
			m = string(r[:3]) // @ + first two CJK characters
		}
		out = append(out, m)
	}
	s = mentionRE.ReplaceAllString(s, " ")
	s = replaceCJKNumbers(s)
	out = append(out, digitRunRE.FindAllString(s, -1)...)
	for _, r := range s {
		if strings.ContainsRune("$¥€£₩", r) {
			out = append(out, string(r))
		}
	}
	return out
}

// replaceCJKNumbers turns numeral+unit runs into Arabic digits. Two common
// words are left alone: a lone 一 (一点 "a bit", 一个 "one/a") and 十分
// ("very" in Chinese, "enough" in Japanese), unless 十分 reads as a time:
// after 点/時/时 (三点十分) or before 钟/鐘/後/后/前/間/间 (十分钟).
func replaceCJKNumbers(s string) string {
	var b strings.Builder
	last := 0
	for _, ix := range cjkNumUnitRE.FindAllStringSubmatchIndex(s, -1) {
		num, unit := s[ix[2]:ix[3]], s[ix[4]:ix[5]]
		keep := num == "一"
		if num == "十" && unit == "分" {
			prev, _ := utf8.DecodeLastRuneInString(s[:ix[0]])
			next, _ := utf8.DecodeRuneInString(s[ix[1]:])
			keep = !strings.ContainsRune("点時时", prev) && !strings.ContainsRune("钟鐘後后前間间", next)
		}
		v, ok := cjkNumber(num)
		if keep || !ok {
			continue
		}
		b.WriteString(s[last:ix[0]])
		b.WriteString(strconv.Itoa(v) + unit)
		last = ix[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// cjkNumber parses numerals up to 万 (三, 十二, 二十五, 三百零五, 两千).
func cjkNumber(s string) (int, bool) {
	digit := map[rune]int{'〇': 0, '零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	unit := map[rune]int{'十': 10, '百': 100, '千': 1000}
	total, section, cur := 0, 0, -1
	for _, r := range s {
		if d, ok := digit[r]; ok {
			cur = d
			continue
		}
		if u, ok := unit[r]; ok {
			if cur < 0 {
				cur = 1
			}
			section += cur * u
			cur = -1
			continue
		}
		if r == '万' {
			if cur > 0 {
				section += cur
			}
			total += section * 10000
			section, cur = 0, -1
			continue
		}
		return 0, false
	}
	if cur > 0 {
		section += cur
	}
	return total + section, true
}

func sameMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func addsEmoji(base, polished string) bool {
	count := func(s string) map[rune]int {
		m := map[rune]int{}
		for _, r := range s {
			if unicode.Is(unicode.So, r) || strings.ContainsRune(kaomojiRunes, r) {
				m[r]++
			}
		}
		return m
	}
	b := count(base)
	for r, n := range count(polished) {
		if n > b[r] {
			return true
		}
	}
	return false
}

// mbtiNames returns the four-letter type names not glued to other Latin letters.
func mbtiNames(s string) map[string]bool {
	out := map[string]bool{}
	for _, ix := range mbtiRE.FindAllStringIndex(s, -1) {
		if isLatinBefore(s, ix[0]) || isLatinAfter(s, ix[1]) {
			continue
		}
		out[strings.ToUpper(s[ix[0]:ix[1]])] = true
	}
	return out
}

func isLatinBefore(s string, i int) bool {
	if i == 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r < utf8.RuneSelf && unicode.IsLetter(r)
}

func isLatinAfter(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r < utf8.RuneSelf && unicode.IsLetter(r)
}

// TooShortToPolish reports text with little to rewrite (backend TRD 10.3.1):
// after dropping spaces, punctuation, symbols (emoji) and marks, at most three
// characters when it has any CJK, kana or Hangul, otherwise at most two words.
func TooShortToPolish(s string) bool {
	var kept []rune
	latin := true
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me) {
			kept = append(kept, ' ')
			continue
		}
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			latin = false
		}
		kept = append(kept, r)
	}
	if latin {
		return len(strings.Fields(string(kept))) <= 2
	}
	n := 0
	for _, r := range kept {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n <= 3
}

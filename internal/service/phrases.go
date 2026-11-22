package service

// PhrasesVersion changes whenever any phrase text changes; clients cache the
// book by it (GET /config/phrases?v=).
const PhrasesVersion = "2"

// PhraseLangs are the languages every phrase must carry.
var PhraseLangs = []string{"ja", "ko", "zh-Hant", "zh-Hans", "en"}

// Phrase is one fixed phrase with its text in every supported language.
type Phrase struct {
	ID    string            `json:"id"`
	Texts map[string]string `json:"texts"`
}

func phrase(id, ja, ko, zhHant, zhHans, en string) Phrase {
	return Phrase{ID: id, Texts: map[string]string{
		"ja": ja, "ko": ko, "zh-Hant": zhHant, "zh-Hans": zhHans, "en": en,
	}}
}

var phraseBook = []Phrase{
	phrase("good_morning", "おはようございます", "좋은 아침이에요", "早安", "早上好", "Good morning"),
	phrase("good_night", "おやすみなさい", "잘 자요", "晚安", "晚安", "Good night"),
	phrase("thank_you", "ありがとうございます", "고마워요", "謝謝你", "谢谢你", "Thank you"),
	phrase("sorry", "ごめんなさい", "미안해요", "對不起", "对不起", "Sorry"),
	phrase("ok", "大丈夫です", "괜찮아요", "沒問題", "没问题", "OK"),
	phrase("see_you", "また会いましょう", "또 만나요", "下次見", "下次见", "See you later"),
	phrase("congrats", "おめでとうございます", "축하해요", "恭喜你", "恭喜你", "Congratulations"),
	phrase("take_care", "お体に気をつけてくださいね", "몸조심하세요", "多保重喔", "多保重哦", "Take care"),
}

// Phrases returns the book in its fixed order. The result is a copy.
func Phrases() []Phrase {
	out := make([]Phrase, len(phraseBook))
	for i, p := range phraseBook {
		texts := make(map[string]string, len(p.Texts))
		for k, v := range p.Texts {
			texts[k] = v
		}
		out[i] = Phrase{ID: p.ID, Texts: texts}
	}
	return out
}

// PhraseText returns the text of phrase id in lang.
func PhraseText(id, lang string) (string, bool) {
	for _, p := range phraseBook {
		if p.ID == id {
			s, ok := p.Texts[lang]
			return s, ok
		}
	}
	return "", false
}

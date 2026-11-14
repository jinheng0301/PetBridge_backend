// Package user holds profiles, onboarding and the MBTI -> pet mapping.
package user

// pets maps an MBTI type to a stable pet slug (see design 5.4). Display
// names and copy live in the client's i18n, so renaming art never needs a
// data migration.
var pets = map[string]string{
	"INTJ": "lone_leopard", "ENTJ": "leader_lion",
	"INTP": "thinker_owl", "ENTP": "clever_fox",
	"INFJ": "guardian_deer", "ENFJ": "guide_dolphin",
	"INFP": "healing_rabbit", "ENFP": "happy_shiba",
	"ISTJ": "steady_beaver", "ESTJ": "order_sheepdog",
	"ISFJ": "guardian_penguin", "ESFJ": "warm_otter",
	"ISTP": "calm_blackcat", "ESTP": "daring_falcon",
	"ISFP": "easygoing_alpaca", "ESFP": "party_parrot",
}

// PetForMBTI returns the pet slug for an upper-case MBTI type.
func PetForMBTI(mbti string) (string, bool) {
	slug, ok := pets[mbti]
	return slug, ok
}

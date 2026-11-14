package model

import "time"

type OnboardingInput struct {
	NativeLang string `json:"native_lang"`
	MBTI       string `json:"mbti"`
	Nickname   string `json:"nickname"`
	AgeBand    string `json:"age_band"`
	TZ         string `json:"tz"`
}

type Profile struct {
	ID         string  `json:"id"`
	Nickname   *string `json:"nickname"`
	NativeLang *string `json:"native_lang"`
	MBTI       *string `json:"mbti"`
	PetType    *string `json:"pet_type"`
	AgeBand    *string `json:"age_band"`
	TZ         *string `json:"tz"`
	Onboarded  bool    `json:"onboarded"`
}

type PetInfo struct {
	Type string `json:"type"`
	MBTI string `json:"mbti"`
}

type RoomInfo struct {
	ID          string `json:"id"`
	MemberCount int    `json:"member_count"`
	Capacity    int    `json:"capacity"`
}

type CodeInfo struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type OnboardingResult struct {
	User       Profile  `json:"user"`
	Pet        PetInfo  `json:"pet"`
	Room       RoomInfo `json:"room"`
	InviteCode CodeInfo `json:"invite_code"`
}

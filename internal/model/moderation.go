package model

import "time"

// Moderation verdicts stored in messages.mod_status and deliveries.mod_status.
const (
	ModOK     = "ok"
	ModReview = "review"
	ModBlock  = "block"
)

// mod_flags.stage values.
const (
	StageOutgoing    = "outgoing"
	StageTranslation = "translation"
	StagePolish      = "polish"
	StageNickname    = "nickname"
)

// ModFlag is one row of the human review queue (spec 5.3).
type ModFlag struct {
	Stage, Verdict, Source                          string
	Terms                                           []string
	UserID                                          string
	RoomID, MessageID, DeliveryID, TargetLang, Text *string
}

// BlockedUser is one entry of a user's block list.
type BlockedUser struct {
	UserID    string    `json:"user_id"`
	Nickname  *string   `json:"nickname"`
	PetType   *string   `json:"pet_type"`
	BlockedAt time.Time `json:"blocked_at"`
}

// ReportReasons are the accepted reports.reason values.
var ReportReasons = []string{"harassment", "spam", "sexual", "self_harm", "minor_safety", "other"}

// NewReport is the input for inserting a report.
type NewReport struct {
	ReporterID, TargetID, RoomID string
	MessageID                    *string
	Reason                       string
	Note                         *string
}

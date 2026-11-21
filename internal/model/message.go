package model

import "time"

// Message scope, kind and delivery status values (backend TRD 7.4, spec 5.1).
const (
	ScopeRoom, ScopeDM                                     = "room", "dm"
	KindText, KindPhrase                                   = "text", "phrase"
	TranslationPending, TranslationDone, TranslationFailed = "pending", "done", "failed"
	PolishNone, PolishPending, PolishDone, PolishFallback  = "none", "pending", "done", "fallback"
)

// SenderInfo is the public profile of a message sender shown on each message.
type SenderInfo struct {
	UserID   string  `json:"user_id"`
	Nickname *string `json:"nickname"`
	PetType  *string `json:"pet_type"`
}

// DeliveryView is the receiver's copy of a message: its translation and polish state.
type DeliveryView struct {
	ID                string  `json:"id"`
	TargetLang        string  `json:"target_lang"`
	BaseText          *string `json:"base_text"`
	TranslationStatus string  `json:"translation_status"`
	PolishedText      *string `json:"polished_text"`
	PolishStatus      string  `json:"polish_status"`
}

// MessageView is the API shape of a message. Nullable fields serialise as null.
// JSON names match backend TRD 7.4: id, room_id, sender, scope, to_user_id, kind,
// phrase_id, src_lang, text, client_msg_id, polish_requested, created_at, delivery.
type MessageView struct {
	ID              string        `json:"id"`
	RoomID          string        `json:"room_id"`
	Sender          SenderInfo    `json:"sender"`
	Scope           string        `json:"scope"`
	ToUserID        *string       `json:"to_user_id"`
	Kind            string        `json:"kind"`
	PhraseID        *string       `json:"phrase_id"`
	SrcLang         string        `json:"src_lang"`
	Text            string        `json:"text"`
	ClientMsgID     *string       `json:"client_msg_id"`
	PolishRequested bool          `json:"polish_requested"`
	CreatedAt       time.Time     `json:"created_at"`
	Delivery        *DeliveryView `json:"delivery"`
}

// MessageViews holds the sender's copy of a message and one copy per receiver, keyed by receiver user id.
type MessageViews struct {
	Sender    MessageView
	Receivers map[string]MessageView
}

// PageQuery is the cursor query for message history. An empty string means not given.
type PageQuery struct {
	Before, After string
	Limit         int
}

// MessagePage is one page of message history.
type MessagePage struct {
	Items   []MessageView `json:"items"`
	HasMore bool          `json:"has_more"`
}

// Recipient is a room member who receives a message, with the fields needed to pick a target language.
type Recipient struct {
	UserID, NativeLang, AgeBand string
}

// PendingDelivery is a delivery waiting for translation.
type PendingDelivery struct {
	ID, ReceiverID, TargetLang string
}

// TranslationWork is the input of one translation task for a message.
type TranslationWork struct {
	MessageID, RoomID, SrcLang, Body string
	Pending                          []PendingDelivery
}

// NewMessage is the input for inserting a message row.
type NewMessage struct {
	ID, RoomID, SenderID, Scope string
	ToUserID                    *string
	Kind                        string
	PhraseID                    *string
	SrcLang, Body, ClientMsgID  string
	PolishRequested             bool
}

// NewDelivery is the input for inserting one receiver's delivery row.
type NewDelivery struct {
	ReceiverID, TargetLang          string
	BaseText                        *string
	TranslationStatus, PolishStatus string
	PolishBy                        *string
}

// History page size: default when the client gives none, and the hard maximum.
const (
	DefaultPageLimit = 30
	MaxPageLimit     = 100
)

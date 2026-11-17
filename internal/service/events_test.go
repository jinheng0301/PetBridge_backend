package service

import (
	"reflect"
	"testing"

	"github.com/jinheng0301/petbridge/internal/model"
)

func TestManageEvents(t *testing.T) {
	left := memberLeftEvent("r1", "u1", "kicked", []string{"a", "b"})
	wantLeft := Event{Type: "member.left", Kind: KindState, RoomID: "r1", To: []string{"a", "b"},
		Data: map[string]any{"room_id": "r1", "user_id": "u1", "reason": "kicked"}}
	if !reflect.DeepEqual(left, wantLeft) {
		t.Fatalf("memberLeftEvent=%+v want %+v", left, wantLeft)
	}
	kicked := roomKickedEvents("r1", "dissolved", []string{"x", "y"})
	want := []Event{
		{Type: "room.kicked", Kind: KindPersonal, RoomID: "r1", To: []string{"x"}, Data: map[string]any{"room_id": "r1", "reason": "dissolved"}},
		{Type: "room.kicked", Kind: KindPersonal, RoomID: "r1", To: []string{"y"}, Data: map[string]any{"room_id": "r1", "reason": "dissolved"}},
	}
	if !reflect.DeepEqual(kicked, want) {
		t.Fatalf("roomKickedEvents=%+v want %+v", kicked, want)
	}
	if got := roomKickedEvents("r1", "dissolved", nil); len(got) != 0 {
		t.Fatalf("no users must give no events, got %v", got)
	}
}

func TestEventKinds(t *testing.T) {
	members := []model.MemberInfo{{UserID: "owner"}, {UserID: "new"}}
	evs := joinEvents("r1", "new", members, []string{"holder"})
	if len(evs) != 2 || evs[0].Type != EventMemberJoined || evs[0].Kind != KindState {
		t.Fatalf("member.joined kind: %+v", evs)
	}
	if evs[1].Type != EventCodeInvalidated || evs[1].Kind != KindPersonal {
		t.Fatalf("code.invalidated kind: %+v", evs[1])
	}
	if k := memberLeftEvent("r1", "u", "left", []string{"a"}).Kind; k != KindState {
		t.Fatalf("member.left kind=%v", k)
	}
	for _, e := range roomKickedEvents("r1", "kicked", []string{"a", "b"}) {
		if e.Kind != KindPersonal {
			t.Fatalf("room.kicked kind=%v", e.Kind)
		}
	}
}

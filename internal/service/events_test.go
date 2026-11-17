package service

import (
	"reflect"
	"testing"
)

func TestManageEvents(t *testing.T) {
	left := memberLeftEvent("r1", "u1", "kicked", []string{"a", "b"})
	wantLeft := Event{Type: "member.left", RoomID: "r1", To: []string{"a", "b"},
		Data: map[string]any{"room_id": "r1", "user_id": "u1", "reason": "kicked"}}
	if !reflect.DeepEqual(left, wantLeft) {
		t.Fatalf("memberLeftEvent=%+v want %+v", left, wantLeft)
	}
	kicked := roomKickedEvents("r1", "dissolved", []string{"x", "y"})
	want := []Event{
		{Type: "room.kicked", RoomID: "r1", To: []string{"x"}, Data: map[string]any{"room_id": "r1", "reason": "dissolved"}},
		{Type: "room.kicked", RoomID: "r1", To: []string{"y"}, Data: map[string]any{"room_id": "r1", "reason": "dissolved"}},
	}
	if !reflect.DeepEqual(kicked, want) {
		t.Fatalf("roomKickedEvents=%+v want %+v", kicked, want)
	}
	if got := roomKickedEvents("r1", "dissolved", nil); len(got) != 0 {
		t.Fatalf("no users must give no events, got %v", got)
	}
}

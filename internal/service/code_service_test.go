package service

import (
	"encoding/json"
	"testing"
	"time"
)

func newCodeSvc(e *joinEnv) *CodeService {
	return NewCodeService(repositoryCodes(e), func() time.Time { return e.now })
}

func TestCodeList_EffectiveStatusAndOrder(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@x.com", "18_plus")
	room := e.roomOf(t, a)
	signup := e.signupCode(t, a)

	fresh := e.extraCode(t, a)
	stale := e.extraCode(t, a)
	e.exec(t, `update invite_codes set source='join_reward', issued_at=$2 where code=$1`, fresh, e.now.Add(2*time.Hour))
	e.exec(t, `update invite_codes set source='join_reward', issued_at=$2, expires_at=$3 where code=$1`,
		stale, e.now.Add(time.Hour), e.now.Add(-time.Minute))
	e.exec(t, `update invite_codes set issued_at=$2 where code=$1`, signup, e.now)
	// A zero code belongs to no user and must never be listed.
	e.exec(t, `insert into invite_codes(code, room_id, issued_to, source, expires_at)
		values('ZZZZZZZZ', $1, null, 'zero', $2)`, room, e.now.Add(24*time.Hour))

	got, err := newCodeSvc(e).List(bg, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	wantCodes := []string{fresh, stale, signup}
	wantStatus := []string{"unused", "expired", "unused"}
	wantSource := []string{"join_reward", "join_reward", "signup"}
	for i, it := range got {
		if it.Code != wantCodes[i] || it.Status != wantStatus[i] || it.Source != wantSource[i] {
			t.Errorf("item %d = %+v, want code=%s status=%s source=%s", i, it, wantCodes[i], wantStatus[i], wantSource[i])
		}
		if it.Room.ID != room || it.Room.MemberCount != 1 || it.Room.Capacity != 5 {
			t.Errorf("item %d room = %+v, want id=%s count=1 cap=5", i, it.Room, room)
		}
	}
	if st := e.codeStatus(t, stale); st != "unused" {
		t.Errorf("stored status of expired code = %q; listing must not mutate it", st)
	}
}

func TestCodeList_EmptyIsNotNil(t *testing.T) {
	e := newJoinEnv(t)
	b := e.bareUser(t, "b@x.com")

	got, err := newCodeSvc(e).List(bg, b)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got = %#v, want non-nil empty slice", got)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != "[]" {
		t.Fatalf("json = %s, want []", raw)
	}
}

package repository_test

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/service"
)

var _ service.CheckinStore = (*repository.CheckinRepo)(nil)

var checkinT = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// checkinAt stores a check-in; the (room, user, day) must be new.
func checkinAt(t *testing.T, r *repository.CheckinRepo, room, user, day string, at time.Time) {
	t.Helper()
	ok, err := r.InsertCheckin(bg, room, user, day, 10, at)
	if err != nil || !ok {
		t.Fatalf("insert %s: ok=%v err=%v", day, ok, err)
	}
}

func claim(t *testing.T, r *repository.CheckinRepo, room string) bool {
	t.Helper()
	ok, err := r.ClaimFullHouse(bg, room, checkinT, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// claimedDays lists the user's check-in days a bonus has used up.
func claimedDays(t *testing.T, p *pgxpool.Pool, room, user string) []string {
	t.Helper()
	var out []string
	err := p.QueryRow(bg, `select coalesce(array_agg(local_day::text order by local_day), '{}') from checkins
		where room_id=$1 and user_id=$2 and bonus_at is not null`, room, user).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInsertCheckin_OncePerDay(t *testing.T) {
	p := dbtest.Reset(t)
	r := repository.NewCheckinRepo(p)
	u, u2 := mkUser(t, p), mkUser(t, p)
	room, room2 := mkRoom(t, p, u), mkRoom(t, p, u2)
	checkinAt(t, r, room, u, "2026-11-25", checkinT)
	if ok, err := r.InsertCheckin(bg, room, u, "2026-11-25", 10, checkinT); err != nil || ok {
		t.Fatalf("same day: ok=%v err=%v", ok, err)
	}
	checkinAt(t, r, room, u, "2026-11-26", checkinT)
	checkinAt(t, r, room2, u, "2026-11-25", checkinT)
}

func TestAddEnergy_Accumulates(t *testing.T) {
	p := dbtest.Reset(t)
	r := repository.NewCheckinRepo(p)
	room := mkRoom(t, p, mkUser(t, p))
	if n, err := r.Energy(bg, room); err != nil || n != 0 {
		t.Fatalf("initial %d err=%v", n, err)
	}
	if n, err := r.AddEnergy(bg, room, 10, checkinT); err != nil || n != 10 {
		t.Fatalf("first %d err=%v", n, err)
	}
	if n, err := r.AddEnergy(bg, room, 30, checkinT); err != nil || n != 40 {
		t.Fatalf("second %d err=%v", n, err)
	}
	if n, err := r.Energy(bg, room); err != nil || n != 40 {
		t.Fatalf("total %d err=%v", n, err)
	}
}

func TestUnlockDecor_OnlyNew(t *testing.T) {
	p := dbtest.Reset(t)
	r := repository.NewCheckinRepo(p)
	room := mkRoom(t, p, mkUser(t, p))
	if l, err := r.ListDecor(bg, room); err != nil || l == nil || len(l) != 0 {
		t.Fatalf("empty list %v err=%v", l, err)
	}
	if got, err := r.UnlockDecor(bg, room, nil, checkinT); err != nil || len(got) != 0 {
		t.Fatalf("empty unlock %v err=%v", got, err)
	}
	if got, err := r.UnlockDecor(bg, room, []string{"wall_clock"}, checkinT); err != nil || !slices.Equal(got, []string{"wall_clock"}) {
		t.Fatalf("first %v err=%v", got, err)
	}
	later := checkinT.Add(time.Hour)
	got, err := r.UnlockDecor(bg, room, []string{"wall_clock", "picture_frame"}, later)
	if err != nil || !slices.Equal(got, []string{"picture_frame"}) {
		t.Fatalf("second %v err=%v", got, err)
	}
	l, err := r.ListDecor(bg, room)
	if err != nil || len(l) != 2 || l[0].ItemID != "wall_clock" || !l[0].UnlockedAt.Equal(checkinT) ||
		l[1].ItemID != "picture_frame" || !l[1].UnlockedAt.Equal(later) {
		t.Fatalf("list %+v err=%v", l, err)
	}
}

func TestClaimFullHouse(t *testing.T) {
	p := dbtest.Reset(t)
	r := repository.NewCheckinRepo(p)

	t.Run("alone never", func(t *testing.T) {
		u := mkUser(t, p)
		room := mkRoom(t, p, u)
		checkinAt(t, r, room, u, "2026-11-25", checkinT)
		if claim(t, r, room) {
			t.Fatal("claimed alone")
		}
	})
	t.Run("one of two missing", func(t *testing.T) {
		a, b := mkUser(t, p), mkUser(t, p)
		room := mkRoom(t, p, a)
		mkMember(t, p, room, b)
		checkinAt(t, r, room, a, "2026-11-25", checkinT)
		if claim(t, r, room) || len(claimedDays(t, p, room, a)) != 0 {
			t.Fatal("claimed with one missing")
		}
	})
	t.Run("both, once", func(t *testing.T) {
		a, b := mkUser(t, p), mkUser(t, p)
		room := mkRoom(t, p, a)
		mkMember(t, p, room, b)
		checkinAt(t, r, room, a, "2026-11-25", checkinT.Add(-time.Hour))
		checkinAt(t, r, room, b, "2026-11-25", checkinT)
		if !claim(t, r, room) {
			t.Fatal("not claimed")
		}
		var n int
		err := p.QueryRow(bg, `select count(*) from checkins where room_id=$1 and bonus_at=$2`, room, checkinT).Scan(&n)
		if err != nil || n != 2 {
			t.Fatalf("bonus_at rows %d err=%v", n, err)
		}
		if claim(t, r, room) {
			t.Fatal("claimed twice")
		}
	})
	for _, c := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"exactly 24h ago", checkinT.Add(-24 * time.Hour), false},
		{"just inside 24h", checkinT.Add(-24*time.Hour + time.Second), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, b := mkUser(t, p), mkUser(t, p)
			room := mkRoom(t, p, a)
			mkMember(t, p, room, b)
			checkinAt(t, r, room, a, "2026-11-24", c.at)
			checkinAt(t, r, room, b, "2026-11-25", checkinT)
			if got := claim(t, r, room); got != c.want {
				t.Fatalf("claimed=%v want %v", got, c.want)
			}
		})
	}
	t.Run("uses the oldest of two", func(t *testing.T) {
		a, b := mkUser(t, p), mkUser(t, p)
		room := mkRoom(t, p, a)
		mkMember(t, p, room, b)
		checkinAt(t, r, room, a, "2026-11-24", checkinT.Add(-23*time.Hour))
		checkinAt(t, r, room, a, "2026-11-25", checkinT.Add(-time.Hour))
		checkinAt(t, r, room, b, "2026-11-25", checkinT)
		if !claim(t, r, room) {
			t.Fatal("not claimed")
		}
		if got := claimedDays(t, p, room, a); !slices.Equal(got, []string{"2026-11-24"}) {
			t.Fatalf("used %v", got)
		}
	})
	t.Run("left members and other rooms do not matter", func(t *testing.T) {
		a, b, gone := mkUser(t, p), mkUser(t, p), mkUser(t, p)
		room, other := mkRoom(t, p, a), mkRoom(t, p, b)
		mkMember(t, p, room, b)
		mkMember(t, p, room, gone)
		if _, err := repository.NewRoomMemberRepo(p).Leave(bg, room, gone, "left", checkinT); err != nil {
			t.Fatal(err)
		}
		checkinAt(t, r, room, a, "2026-11-25", checkinT)
		checkinAt(t, r, other, b, "2026-11-25", checkinT)
		if claim(t, r, room) {
			t.Fatal("a check-in in another room counted")
		}
		checkinAt(t, r, room, b, "2026-11-25", checkinT)
		if !claim(t, r, room) {
			t.Fatal("the member who left blocked the bonus")
		}
	})
}

func TestClaimFullHouse_IgnoresInactive(t *testing.T) {
	p := dbtest.Reset(t)
	r := repository.NewCheckinRepo(p)
	setStatus := func(t *testing.T, user, status string) {
		t.Helper()
		if _, err := p.Exec(bg, `update users set status=$2 where id=$1`, user, status); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"banned", "deleted"} {
		t.Run(status, func(t *testing.T) {
			a, b, c := mkUser(t, p), mkUser(t, p), mkUser(t, p)
			room := mkRoom(t, p, a)
			mkMember(t, p, room, b)
			mkMember(t, p, room, c)
			setStatus(t, c, status)
			checkinAt(t, r, room, a, "2026-11-25", checkinT)
			checkinAt(t, r, room, b, "2026-11-25", checkinT)
			if !claim(t, r, room) {
				t.Fatal("an inactive member blocked the bonus")
			}
		})
	}
	t.Run("one active left", func(t *testing.T) {
		a, b := mkUser(t, p), mkUser(t, p)
		room := mkRoom(t, p, a)
		mkMember(t, p, room, b)
		setStatus(t, b, "banned")
		checkinAt(t, r, room, a, "2026-11-25", checkinT)
		checkinAt(t, r, room, b, "2026-11-25", checkinT)
		if claim(t, r, room) {
			t.Fatal("claimed with one active member")
		}
	})
}

func TestMemberCheckins(t *testing.T) {
	p := dbtest.Reset(t)
	r := repository.NewCheckinRepo(p)
	a, b, gone := mkUser(t, p), mkUser(t, p), mkUser(t, p)
	room, other := mkRoom(t, p, a), mkRoom(t, p, b)
	mkMember(t, p, room, b)
	mkMember(t, p, room, gone)
	if _, err := repository.NewRoomMemberRepo(p).Leave(bg, room, gone, "left", checkinT); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(bg, `update users set tz='Asia/Tokyo' where id=$1`, a); err != nil {
		t.Fatal(err)
	}
	checkinAt(t, r, room, a, "2026-11-25", checkinT)
	checkinAt(t, r, room, a, "2026-11-24", checkinT)
	checkinAt(t, r, room, gone, "2026-11-25", checkinT)
	checkinAt(t, r, other, b, "2026-11-25", checkinT)
	got, err := r.MemberCheckins(bg, room)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	for _, m := range got {
		switch m.UserID {
		case a:
			if m.TZ == nil || *m.TZ != "Asia/Tokyo" || m.LastDay == nil || *m.LastDay != "2026-11-25" {
				t.Fatalf("a %+v", m)
			}
		case b:
			if m.TZ != nil || m.LastDay != nil {
				t.Fatalf("b %+v", m)
			}
		default:
			t.Fatalf("unexpected member %+v", m)
		}
	}
}

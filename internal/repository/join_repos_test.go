package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/service"
)

var bg = context.Background()

// Compile-time proof the repositories satisfy the service ports.
var (
	_ service.RoomStore       = (*repository.RoomRepo)(nil)
	_ service.RoomMemberStore = (*repository.RoomMemberRepo)(nil)
	_ service.InviteCodeStore = (*repository.InviteCodeRepo)(nil)
	_ service.BanStore        = (*repository.BanRepo)(nil)
	_ service.RewardStore     = (*repository.RewardRepo)(nil)
)

func mkUser(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := p.QueryRow(bg, `insert into users default values returning id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// mkRoom creates a room with its owner seated at seat 1 (member_count=1).
func mkRoom(t *testing.T, p *pgxpool.Pool, owner string) string {
	t.Helper()
	var id string
	if err := p.QueryRow(bg, `insert into rooms(owner_id, member_count) values($1,1) returning id::text`, owner).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(bg, `insert into room_members(room_id,user_id,seat_no,role) values($1,$2,1,'owner')`, id, owner); err != nil {
		t.Fatal(err)
	}
	return id
}

func mkCode(t *testing.T, p *pgxpool.Pool, code, room string, issuedTo *string, source, status string, exp time.Time) {
	t.Helper()
	if _, err := p.Exec(bg, `insert into invite_codes(code,room_id,issued_to,source,status,expires_at) values($1,$2,$3,$4,$5,$6)`,
		code, room, issuedTo, source, status, exp); err != nil {
		t.Fatal(err)
	}
}

func TestLockRooms_DedupesAndOrders(t *testing.T) {
	p := dbtest.Reset(t)
	rooms := repository.NewRoomRepo(p)
	txm := repository.NewTxManager(p)
	a := mkRoom(t, p, mkUser(t, p))
	b := mkRoom(t, p, mkUser(t, p))

	var got int
	err := txm.WithTx(bg, func(ctx context.Context) error {
		m, err := rooms.LockRooms(ctx, b, a, a, "not-a-uuid", "00000000-0000-0000-0000-000000000000")
		got = len(m)
		if m[a].ID != a || m[a].Capacity != 5 || m[a].Status != "open" || m[a].OwnerID == "" {
			t.Errorf("state = %+v", m[a])
		}
		return err
	})
	if err != nil || got != 2 {
		t.Fatalf("got %d err %v", got, err)
	}

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, order := range [][]string{{a, b}, {b, a}} {
		wg.Add(1)
		go func(ids []string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := txm.WithTx(ctx, func(c context.Context) error {
					_, err := rooms.LockRooms(c, ids...)
					return err
				}); err != nil {
					errs <- err
					return
				}
			}
		}(order)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent lock: %v", err)
	}
}

func TestAdmit_RespectsCapacityAndStatus(t *testing.T) {
	p := dbtest.Reset(t)
	rooms := repository.NewRoomRepo(p)
	room := mkRoom(t, p, mkUser(t, p))

	n, ok, err := rooms.Admit(bg, room)
	if err != nil || !ok || n != 2 {
		t.Fatalf("admit: n=%d ok=%v err=%v", n, ok, err)
	}
	if err := rooms.MarkFull(bg, room); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := rooms.Admit(bg, room); ok {
		t.Fatal("status=full must not admit")
	}
	if _, err := p.Exec(bg, `update rooms set status='open', member_count=5 where id=$1`, room); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := rooms.Admit(bg, room); ok {
		t.Fatal("member_count=capacity must not admit")
	}
}

func TestAddMember_PicksSmallestFreeSeat(t *testing.T) {
	p := dbtest.Reset(t)
	members := repository.NewRoomMemberRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner) // seat 1
	u3, u2, u4 := mkUser(t, p), mkUser(t, p), mkUser(t, p)
	if _, err := p.Exec(bg, `insert into room_members(room_id,user_id,seat_no,role) values($1,$2,3,'member')`, room, u3); err != nil {
		t.Fatal(err)
	}
	seat, err := members.AddMember(bg, room, u2)
	if err != nil || seat != 2 {
		t.Fatalf("seat=%d err=%v", seat, err)
	}
	if seat, _ := members.AddMember(bg, room, u4); seat != 4 {
		t.Fatalf("seat=%d want 4", seat)
	}
	if _, err := p.Exec(bg, `update room_members set left_at=now(), left_reason='left' where room_id=$1 and user_id=$2`, room, u2); err != nil {
		t.Fatal(err)
	}
	u5 := mkUser(t, p)
	if seat, _ := members.AddMember(bg, room, u5); seat != 2 {
		t.Fatalf("freed seat not reused: %d", seat)
	}

	act, err := members.IsActiveMember(bg, room, u2)
	if err != nil || act {
		t.Fatalf("left member active=%v err=%v", act, err)
	}
	if act, _ := members.IsActiveMember(bg, room, u5); !act {
		t.Fatal("u5 should be active")
	}
	if act, err := members.IsActiveMember(bg, "bad", u5); err != nil || act {
		t.Fatalf("malformed id: %v %v", act, err)
	}
	if n, _ := members.CountActiveForUser(bg, u5); n != 1 {
		t.Fatalf("count=%d", n)
	}
	list, err := members.ListMembers(bg, room)
	if err != nil || len(list) != 4 || list[0].SeatNo != 1 || list[0].Role != "owner" || list[1].SeatNo != 2 {
		t.Fatalf("members=%+v err=%v", list, err)
	}
}

func TestAgeBands(t *testing.T) {
	p := dbtest.Reset(t)
	members := repository.NewRoomMemberRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	u := mkUser(t, p)
	if _, err := p.Exec(bg, `update users set age_band='18_plus' where id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(bg, `update users set age_band='13_17' where id=$1`, u); err != nil {
		t.Fatal(err)
	}
	if _, err := members.AddMember(bg, room, u); err != nil {
		t.Fatal(err)
	}
	bands, err := members.AgeBands(bg, room)
	if err != nil || len(bands) != 2 {
		t.Fatalf("bands=%v err=%v", bands, err)
	}
}

func TestVoidUnusedForRoom_ReturnsHoldersOnly(t *testing.T) {
	p := dbtest.Reset(t)
	codes := repository.NewInviteCodeRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	h := mkUser(t, p)
	exp := time.Now().Add(time.Hour)
	mkCode(t, p, "AAAAAAA2", room, &h, "signup", "unused", exp)
	mkCode(t, p, "AAAAAAA3", room, &h, "join_reward", "unused", exp)
	mkCode(t, p, "AAAAAAA4", room, nil, "zero", "unused", exp)
	mkCode(t, p, "AAAAAAA5", room, &owner, "signup", "used", exp)

	holders, err := codes.VoidUnusedForRoom(bg, room)
	if err != nil || len(holders) != 1 || holders[0] != h {
		t.Fatalf("holders=%v err=%v", holders, err)
	}
	var voided, used int
	if err := p.QueryRow(bg, `select count(*) filter (where status='void'), count(*) filter (where status='used') from invite_codes`).Scan(&voided, &used); err != nil {
		t.Fatal(err)
	}
	if voided != 3 || used != 1 {
		t.Fatalf("void=%d used=%d", voided, used)
	}
}

func TestCountUnusedValid_ExcludesExpiredAndBoundary(t *testing.T) {
	p := dbtest.Reset(t)
	codes := repository.NewInviteCodeRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	now := time.Now().UTC().Truncate(time.Microsecond)
	mkCode(t, p, "BBBBBBB2", room, &owner, "signup", "unused", now.Add(time.Hour))
	mkCode(t, p, "BBBBBBB3", room, &owner, "signup", "unused", now)
	mkCode(t, p, "BBBBBBB4", room, &owner, "signup", "unused", now.Add(-time.Hour))
	mkCode(t, p, "BBBBBBB5", room, &owner, "signup", "used", now.Add(time.Hour))
	n, err := codes.CountUnusedValid(bg, room, now)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestLifetimeCount_ExcludesZero(t *testing.T) {
	p := dbtest.Reset(t)
	codes := repository.NewInviteCodeRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	exp := time.Now().Add(time.Hour)
	mkCode(t, p, "CCCCCCC2", room, &owner, "signup", "unused", exp)
	mkCode(t, p, "CCCCCCC3", room, &owner, "join_reward", "void", exp)
	mkCode(t, p, "CCCCCCC4", room, nil, "zero", "unused", exp)
	for i := 0; i < 2; i++ { // second call must be a silent no-op
		if err := codes.InsertVoidZero(bg, "CCCCCCC5", room, exp); err != nil {
			t.Fatal(err)
		}
	}
	var src, st string
	if err := p.QueryRow(bg, `select source,status from invite_codes where code='CCCCCCC5'`).Scan(&src, &st); err != nil {
		t.Fatal(err)
	}
	if src != "zero" || st != "void" {
		t.Fatalf("void zero: %s %s", src, st)
	}
	n, err := codes.LifetimeCount(bg, owner)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestInsertReward_And_RewardsFor(t *testing.T) {
	p := dbtest.Reset(t)
	codes := repository.NewInviteCodeRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	mkCode(t, p, "DDDDDDD2", room, &owner, "signup", "unused", exp)
	joiner := mkUser(t, p)
	jroom := mkRoom(t, p, joiner)

	ins, err := codes.InsertReward(bg, "DDDDDDD3", jroom, joiner, "DDDDDDD2", exp)
	if err != nil || !ins {
		t.Fatalf("ins=%v err=%v", ins, err)
	}
	ins, err = codes.InsertReward(bg, "DDDDDDD3", jroom, joiner, "DDDDDDD2", exp)
	if err != nil || ins {
		t.Fatalf("dup ins=%v err=%v", ins, err)
	}
	rs, err := codes.RewardsFor(bg, "DDDDDDD2")
	if err != nil || len(rs) != 1 || rs[0].Code != "DDDDDDD3" || !rs[0].ExpiresAt.Equal(exp) {
		t.Fatalf("rewards=%+v err=%v", rs, err)
	}
	var src string
	if err := p.QueryRow(bg, `select source from invite_codes where code='DDDDDDD3'`).Scan(&src); err != nil || src != "join_reward" {
		t.Fatalf("src=%q err=%v", src, err)
	}
}

func TestLockByCode_And_MarkUsed(t *testing.T) {
	p := dbtest.Reset(t)
	codes := repository.NewInviteCodeRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	exp := time.Now().Add(time.Hour)
	mkCode(t, p, "FFFFFFF2", room, &owner, "signup", "unused", exp)
	c, err := codes.LockByCode(bg, "FFFFFFF2")
	if err != nil || c.RoomID != room || c.Status != "unused" || c.IssuedTo == nil || *c.IssuedTo != owner {
		t.Fatalf("c=%+v err=%v", c, err)
	}
	if _, err := codes.LockByCode(bg, "ZZZZZZZZ"); err == nil {
		t.Fatal("want ErrNotFound")
	}
	u := mkUser(t, p)
	if err := codes.MarkUsed(bg, "FFFFFFF2", u, time.Now()); err != nil {
		t.Fatal(err)
	}
	c, _ = codes.LockByCode(bg, "FFFFFFF2")
	if c.Status != "used" || c.UsedBy == nil || *c.UsedBy != u {
		t.Fatalf("c=%+v", c)
	}
}

func TestRoomIDOf(t *testing.T) {
	p := dbtest.Reset(t)
	codes := repository.NewInviteCodeRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	mkCode(t, p, "HHHHHHH2", room, &owner, "signup", "used", time.Now().Add(time.Hour))
	got, err := codes.RoomIDOf(bg, "HHHHHHH2")
	if err != nil || got != room {
		t.Fatalf("room=%q err=%v", got, err)
	}
	if _, err := codes.RoomIDOf(bg, "ZZZZZZZZ"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("unknown code: err=%v, want ErrNotFound", err)
	}
}

// LockUnusedForRoom must lock exactly the room's unused codes, so a second
// transaction cannot take them (NOWAIT fails) but can take used ones.
func TestLockUnusedForRoom_LocksUnusedOnly(t *testing.T) {
	p := dbtest.Reset(t)
	codes := repository.NewInviteCodeRepo(p)
	txm := repository.NewTxManager(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	other := mkRoom(t, p, mkUser(t, p))
	exp := time.Now().Add(time.Hour)
	mkCode(t, p, "JJJJJJJ2", room, &owner, "signup", "unused", exp)
	mkCode(t, p, "JJJJJJJ3", room, &owner, "join_reward", "unused", exp)
	mkCode(t, p, "JJJJJJJ4", room, &owner, "join_reward", "used", exp)
	mkCode(t, p, "JJJJJJJ5", other, nil, "zero", "unused", exp)

	tryLock := func(code string) error {
		tx, err := p.Begin(bg)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(bg)
		_, err = tx.Exec(bg, `select 1 from invite_codes where code=$1 for update nowait`, code)
		return err
	}
	err := txm.WithTx(bg, func(ctx context.Context) error {
		if err := codes.LockUnusedForRoom(ctx, room); err != nil {
			return err
		}
		for _, c := range []string{"JJJJJJJ2", "JJJJJJJ3"} {
			if tryLock(c) == nil {
				t.Errorf("%s not locked", c)
			}
		}
		for _, c := range []string{"JJJJJJJ4", "JJJJJJJ5"} {
			if err := tryLock(c); err != nil {
				t.Errorf("%s should be free: %v", c, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	empty := mkRoom(t, p, mkUser(t, p))
	if err := txm.WithTx(bg, func(ctx context.Context) error { return codes.LockUnusedForRoom(ctx, empty) }); err != nil {
		t.Fatalf("room without codes: %v", err)
	}
}

func TestListForUser_EffectiveStatus(t *testing.T) {
	p := dbtest.Reset(t)
	codes := repository.NewInviteCodeRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	now := time.Now().UTC().Truncate(time.Microsecond)
	mkCode(t, p, "EEEEEEE2", room, &owner, "signup", "unused", now.Add(time.Hour))
	if _, err := p.Exec(bg, `update invite_codes set issued_at=now()-interval '1 hour' where code='EEEEEEE2'`); err != nil {
		t.Fatal(err)
	}
	mkCode(t, p, "EEEEEEE3", room, &owner, "join_reward", "unused", now.Add(-time.Hour))
	mkCode(t, p, "EEEEEEE4", room, &owner, "join_reward", "used", now.Add(-time.Hour))
	items, err := codes.ListForUser(bg, owner, now)
	if err != nil || len(items) != 3 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	st := map[string]string{}
	for _, it := range items {
		st[it.Code] = it.Status
		if it.Room.ID != room || it.Room.Capacity != 5 {
			t.Errorf("room=%+v", it.Room)
		}
	}
	if st["EEEEEEE2"] != "unused" || st["EEEEEEE3"] != "expired" || st["EEEEEEE4"] != "used" {
		t.Fatalf("statuses=%v", st)
	}
	if items[2].Code != "EEEEEEE2" {
		t.Fatalf("not issued_at desc: %+v", items)
	}
}

func TestListForMember_OwnRoomFirstAndMembers(t *testing.T) {
	p := dbtest.Reset(t)
	rooms := repository.NewRoomRepo(p)
	members := repository.NewRoomMemberRepo(p)
	me := mkUser(t, p)
	other := mkUser(t, p)
	// other's room is created first so ordering must come from the rule, not insertion.
	otherRoom := mkRoom(t, p, other)
	myRoom := mkRoom(t, p, me)
	if _, err := p.Exec(bg, `update rooms set member_count=2 where id=$1`, otherRoom); err != nil {
		t.Fatal(err)
	}
	if _, err := members.AddMember(bg, otherRoom, me); err != nil {
		t.Fatal(err)
	}
	ds, err := rooms.ListForMember(bg, me)
	if err != nil || len(ds) != 2 {
		t.Fatalf("ds=%+v err=%v", ds, err)
	}
	if ds[0].ID != myRoom || !ds[0].IsOwner || len(ds[0].Members) != 1 {
		t.Fatalf("first=%+v", ds[0])
	}
	if ds[1].ID != otherRoom || ds[1].IsOwner || len(ds[1].Members) != 2 || ds[1].Members[0].Role != "owner" {
		t.Fatalf("second=%+v", ds[1])
	}
	if ds[1].Status != "open" || ds[1].MemberCount != 2 {
		t.Fatalf("summary=%+v", ds[1].RoomSummary)
	}
}

func TestGrant_Once(t *testing.T) {
	p := dbtest.Reset(t)
	rewards := repository.NewRewardRepo(p)
	u := mkUser(t, p)
	room := mkRoom(t, p, mkUser(t, p))
	ins, err := rewards.Grant(bg, u, room, 0)
	if err != nil || !ins {
		t.Fatalf("ins=%v err=%v", ins, err)
	}
	ins, err = rewards.Grant(bg, u, room, 2)
	if err != nil || ins {
		t.Fatalf("second ins=%v err=%v", ins, err)
	}
}

func TestInsertEdge(t *testing.T) {
	p := dbtest.Reset(t)
	rewards := repository.NewRewardRepo(p)
	inviter, invitee := mkUser(t, p), mkUser(t, p)
	room := mkRoom(t, p, inviter)
	mkCode(t, p, "GGGGGGG2", room, &inviter, "signup", "used", time.Now().Add(time.Hour))
	if err := rewards.InsertEdge(bg, inviter, invitee, "GGGGGGG2"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := p.QueryRow(bg, `select count(*) from invite_edges`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("edges=%d err=%v", n, err)
	}
}

func TestIsBanned(t *testing.T) {
	p := dbtest.Reset(t)
	bans := repository.NewBanRepo(p)
	room := mkRoom(t, p, mkUser(t, p))
	u := mkUser(t, p)
	if b, err := bans.IsBanned(bg, room, u); err != nil || b {
		t.Fatalf("b=%v err=%v", b, err)
	}
	if _, err := p.Exec(bg, `insert into room_bans(room_id,user_id) values($1,$2)`, room, u); err != nil {
		t.Fatal(err)
	}
	if b, err := bans.IsBanned(bg, room, u); err != nil || !b {
		t.Fatalf("b=%v err=%v", b, err)
	}
}

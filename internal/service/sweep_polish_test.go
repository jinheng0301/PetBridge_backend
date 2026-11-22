package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSweep_PicksStuckPolish(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	now := time.Date(2026, 11, 21, 9, 30, 45, 0, time.UTC)
	_, stuck := e.polishDelivery(t, k, "zh-Hans", "ko", "明天见面聊", "내일 만나서 얘기해요")
	_, fresh := e.polishDelivery(t, k, "zh-Hans", "ko", "明天见面聊吧", "내일 만나서 얘기해요")
	e.exec(t, `update deliveries set updated_at=$2 where id=$1`, stuck, now.Add(-3*time.Minute))
	e.exec(t, `update deliveries set updated_at=$2 where id=$1`, fresh, now.Add(-time.Minute))

	pq := &recPolishQueue{}
	sw := NewSweepService(e.repo, &fakeQueue{}, func() time.Time { return now }, discardLog()).
		WithPolish(pq, func(context.Context) (int, error) { return 0, nil })
	n, err := sw.Sweep(bg)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := pq.list(); len(got) != 1 || got[0] != stuck {
		t.Fatalf("enqueued %v, want [%s]", got, stuck)
	}
	if !strings.HasPrefix(pq.keys[0], "sweep-") {
		t.Fatalf("dedupe %q", pq.keys[0])
	}
}

func TestSweep_GiveUpRunsFirstAndErrorsDoNotStop(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	now := time.Date(2026, 11, 21, 9, 30, 45, 0, time.UTC)
	_, stuck := e.polishDelivery(t, k, "zh-Hans", "ko", "明天见面聊", "내일 만나서 얘기해요")
	e.exec(t, `update deliveries set updated_at=$2 where id=$1`, stuck, now.Add(-3*time.Minute))
	tr := e.message(t, map[string]string{k: "ko"})
	e.exec(t, `update deliveries set updated_at=$2 where message_id=$1`, tr, now.Add(-3*time.Minute))
	e.exec(t, `update messages set created_at=$1`, now.Add(-5*time.Minute))

	var order []string
	pq := &recPolishQueue{}
	tq := &fakeQueue{}
	boom := errors.New("give up failed")
	sw := NewSweepService(e.repo, tq, func() time.Time { return now }, discardLog()).
		WithPolish(pq, func(context.Context) (int, error) {
			order = append(order, "giveup")
			if len(pq.list()) != 0 || len(tq.ids) != 0 {
				t.Error("give-up did not run first")
			}
			return 0, boom
		})
	n, err := sw.Sweep(bg)
	if !errors.Is(err, boom) || n != 2 {
		t.Fatalf("n=%d err=%v, want 2 and the give-up error", n, err)
	}
	if len(order) != 1 || len(pq.list()) != 1 || len(tq.ids) != 1 || tq.ids[0] != tr {
		t.Fatalf("order=%v polish=%v translate=%v", order, pq.list(), tq.ids)
	}
}

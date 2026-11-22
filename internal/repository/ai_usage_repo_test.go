package repository_test

import (
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/service"
)

var _ service.UsageStore = (*repository.AIUsageRepo)(nil)

func TestAIUsage_Accumulates(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewAIUsageRepo(p)
	u := mkUser(t, p)
	tokyo := time.FixedZone("JST", 9*3600)
	day := time.Date(2026, 11, 22, 23, 30, 0, 0, tokyo) // 14:30Z on 2026-11-22

	if err := repo.Add(bg, day, u, model.UsageTranslate, 1, 100, 20, 30); err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(bg, day.Add(time.Minute), u, model.UsageTranslate, 1, 50, 10, 15); err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(bg, day, u, model.UsagePolish, 2, 7, 3, 1); err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(bg, day, u, model.UsagePolish, 0, 999, 999, 999); err != nil { // calls=0 writes nothing
		t.Fatal(err)
	}

	type row struct {
		day           string
		calls         int
		in, out, cost int64
	}
	got := map[string]row{}
	rows, err := p.Query(bg, `select kind, day::text, calls, tokens_in, tokens_out, cost_micro from ai_usage_daily where user_id=$1`, u)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var r row
		if err := rows.Scan(&k, &r.day, &r.calls, &r.in, &r.out, &r.cost); err != nil {
			t.Fatal(err)
		}
		got[k] = r
	}
	if len(got) != 2 {
		t.Fatalf("rows %v, want 2", got)
	}
	if r := got["translate"]; r != (row{"2026-11-22", 2, 150, 30, 45}) {
		t.Errorf("translate %+v", r)
	}
	if r := got["polish"]; r != (row{"2026-11-22", 2, 7, 3, 1}) {
		t.Errorf("polish %+v", r)
	}
}

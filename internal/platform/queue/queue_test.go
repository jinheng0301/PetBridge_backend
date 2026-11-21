package queue

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hibiken/asynq"
)

func TestRetryDelay(t *testing.T) {
	// asynq passes msg.Retried: 0 for the first retry.
	if got := RetryDelay(0, nil, nil); got != 15*time.Second {
		t.Fatalf("first retry = %v, want 15s", got)
	}
	for _, n := range []int{1, 2, 5} {
		if got := RetryDelay(n, nil, nil); got != 60*time.Second {
			t.Fatalf("retry %d = %v, want 60s", n, got)
		}
	}
}

func TestNew_BadURL(t *testing.T) {
	if _, err := New("not a url"); err == nil {
		t.Fatal("want error for malformed url")
	}
}

func testRedisURL() string {
	if v := os.Getenv("TEST_REDIS_URL"); v != "" {
		return v
	}
	return "redis://127.0.0.1:6379/0"
}

func TestEnqueueTranslate_Dedupes(t *testing.T) {
	if testing.Short() {
		t.Skip("needs redis")
	}
	url := testRedisURL()
	opt, err := asynq.ParseRedisURI(url)
	if err != nil {
		t.Fatal(err)
	}
	insp := asynq.NewInspector(opt)
	defer insp.Close()
	q, err := New(url)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	ctx := context.Background()
	probe := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = insp.DeleteTask(QueueTranslate, "translate:"+probe+":p") })
	// Probe Redis; skip when unavailable.
	if err := q.EnqueueTranslate(ctx, probe, "p"); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	base, _ := insp.GetQueueInfo(QueueTranslate)
	id := fmt.Sprintf("m-%d", time.Now().UnixNano())
	t.Cleanup(func() { // remove only this test's tasks; the dev queue stays intact
		for _, tid := range []string{"translate:" + id + ":a", "translate:" + id + ":b"} {
			_ = insp.DeleteTask(QueueTranslate, tid)
		}
	})
	for i := 0; i < 2; i++ {
		if err := q.EnqueueTranslate(ctx, id, "a"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	info, err := insp.GetQueueInfo(QueueTranslate)
	if err != nil {
		t.Fatal(err)
	}
	if info.Pending-base.Pending != 1 {
		t.Fatalf("pending = %d, want 1", info.Pending)
	}
	if err := q.EnqueueTranslate(ctx, id, "b"); err != nil {
		t.Fatal(err)
	}
	info, _ = insp.GetQueueInfo(QueueTranslate)
	if info.Pending-base.Pending != 2 {
		t.Fatalf("pending = %d, want 2", info.Pending)
	}
}

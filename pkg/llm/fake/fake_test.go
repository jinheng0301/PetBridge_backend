package fake_test

import (
	"context"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/pkg/llm"
	"github.com/jinheng0301/petbridge/pkg/llm/fake"
)

func TestFake(t *testing.T) {
	ctx := context.Background()
	req := llm.TranslateRequest{Text: "你好", SrcLang: "zh-Hans", TgtLang: "ko"}

	p := fake.New("fake-primary", 0, 1, func() float64 { return 0 })
	if p.Name() != "fake-primary" {
		t.Errorf("name %q", p.Name())
	}
	if _, err := p.Translate(ctx, req); err == nil {
		t.Error("failRate=1 must fail")
	}

	ok := fake.New("fake-secondary", 0, 0, nil)
	res, err := ok.Translate(ctx, req)
	if err != nil || res.Text != "[ko] 你好" || res.Model != "fake" {
		t.Fatalf("got %+v %v", res, err)
	}
	// Default rnd with failRate=1 also always fails.
	if _, err := fake.New("x", 0, 1, nil).Translate(ctx, req); err == nil {
		t.Error("failRate=1 with default rnd must fail")
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := fake.New("x", time.Second, 0, nil).Translate(cctx, req); err == nil {
		t.Error("cancelled ctx must error")
	}
}

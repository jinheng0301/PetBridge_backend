package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/pkg/llm"
)

func gwPolishReq(text string) llm.PolishRequest {
	return llm.PolishRequest{Text: text, Source: "원문", SrcLang: "ko", TgtLang: "zh-Hans", MBTI: "ENFP", Tier: llm.TierLight}
}

func polishCfg() GatewayConfig {
	c := gwConfig()
	c.PolishTimeout = 6 * time.Second
	return c
}

func TestGateway_PolishSharesBreaker(t *testing.T) {
	p := &gwProvider{name: "fake-primary", err: errors.New("down"), polishErr: errors.New("down")}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(polishCfg(), newGWCache(), newGWMeter(), nil, p, s)
	for i := 0; i < 20; i++ { // trip the primary's breaker with translations
		if _, err := g.Translate(context.Background(), gwReq(fmt.Sprintf("m%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	o, err := g.Polish(context.Background(), gwPolishReq("明天见。"))
	if err != nil || o.Provider != "fake-secondary" {
		t.Fatalf("outcome %+v err=%v", o, err)
	}
	if n := p.pcalls.Load(); n != 0 {
		t.Fatalf("primary Polish called %d times with its breaker open", n)
	}
}

func TestGateway_PolishRejectedNoFallbackNoCache(t *testing.T) {
	cache, meter := newGWCache(), newGWMeter()
	p := &gwProvider{name: "claude", model: "claude-haiku-5-5", polishText: "好的😊", usage: llm.Usage{InputTokens: 1_000_000}}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(polishCfg(), cache, meter, nil, p, s)
	req := gwPolishReq("好的")
	o, err := g.Polish(context.Background(), req)
	var rej *PolishRejectedError
	if !errors.Is(err, ErrPolishRejected) || !errors.As(err, &rej) || rej.Check != llm.CheckEmoji {
		t.Fatalf("err = %v", err)
	}
	if s.pcalls.Load() != 0 {
		t.Fatal("secondary tried after a rule violation")
	}
	if _, ok := cache.data[PolishCacheKey(req)]; ok {
		t.Fatal("rejected polish was cached")
	}
	if meter.data[gwDay] != 100000 || o.Calls != 1 || o.CostMicros != 100000 || o.Usage.InputTokens != 1_000_000 {
		t.Fatalf("cost not recorded: meter=%v outcome=%+v", meter.data, o)
	}
}

func TestGateway_PolishBadOutputFallsBack(t *testing.T) {
	p := &gwProvider{name: "fake-primary", polishErr: llm.ErrBadOutput}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(polishCfg(), newGWCache(), newGWMeter(), nil, p, s)
	o, err := g.Polish(context.Background(), gwPolishReq("明天见。"))
	if err != nil || o.Provider != "fake-secondary" || o.Text != "明天见。呀" || o.Calls != 1 {
		t.Fatalf("outcome %+v err=%v", o, err)
	}
}

func TestGateway_PolishTimeout(t *testing.T) {
	cfg := polishCfg()
	cfg.PolishTimeout = 20 * time.Millisecond
	p := &gwProvider{name: "fake-primary", sleep: 100 * time.Millisecond}
	s := &gwProvider{name: "fake-secondary"}
	g := newTestGateway(cfg, newGWCache(), newGWMeter(), nil, p, s)
	o, err := g.Polish(context.Background(), gwPolishReq("明天见。"))
	if err != nil || o.Provider != "fake-secondary" {
		t.Fatalf("outcome %+v err=%v", o, err)
	}
}

func TestGateway_PolishCachesAndHits(t *testing.T) {
	cache := newGWCache()
	p := &gwProvider{name: "fake-primary"}
	g := newTestGateway(polishCfg(), cache, newGWMeter(), nil, p)
	if _, err := g.Polish(context.Background(), gwPolishReq("明天见。")); err != nil {
		t.Fatal(err)
	}
	o, err := g.Polish(context.Background(), gwPolishReq(" 明天见。 "))
	if err != nil || !o.Cached || o.Text != "明天见。呀" || o.Calls != 0 || p.pcalls.Load() != 1 {
		t.Fatalf("outcome %+v err=%v calls=%d", o, err, p.pcalls.Load())
	}
}

func TestGateway_CacheBeforeBudget(t *testing.T) {
	cache, meter := newGWCache(), newGWMeter()
	p := &gwProvider{name: "fake-primary"}
	g := newTestGateway(polishCfg(), cache, meter, nil, p)
	tr, po := gwReq("hi"), gwPolishReq("明天见。")
	cache.data[TranslateCacheKey(tr)] = "cached-tr"
	cache.data[PolishCacheKey(po)] = "cached-po"
	meter.data[gwDay] = 3 * polishCfg().DailyBudgetMicros

	if o, err := g.Translate(context.Background(), tr); err != nil || !o.Cached || o.Text != "cached-tr" {
		t.Fatalf("translate %+v %v", o, err)
	}
	if o, err := g.Polish(context.Background(), po); err != nil || !o.Cached || o.Text != "cached-po" {
		t.Fatalf("polish %+v %v", o, err)
	}
	if _, err := g.Translate(context.Background(), gwReq("miss")); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Fatalf("translate miss err=%v", err)
	}
	if _, err := g.Polish(context.Background(), gwPolishReq("没缓存的句子")); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Fatalf("polish miss err=%v", err)
	}
	if p.calls.Load()+p.pcalls.Load() != 0 {
		t.Fatal("provider called over budget")
	}
}

func TestGateway_PolishBudgetIsOneX(t *testing.T) {
	meter := newGWMeter()
	meter.data[gwDay] = polishCfg().DailyBudgetMicros // exactly 1x: polish stops, translation continues
	p := &gwProvider{name: "fake-primary"}
	g := newTestGateway(polishCfg(), newGWCache(), meter, nil, p)
	if _, err := g.Translate(context.Background(), gwReq("hi")); err != nil {
		t.Fatalf("translate at 1x: %v", err)
	}
	if _, err := g.Polish(context.Background(), gwPolishReq("明天见。")); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Fatalf("polish at 1x err=%v", err)
	}
}

func TestPolishCacheKey(t *testing.T) {
	a := gwPolishReq("明天 见")
	b := a
	b.Text = "  明天\t见 "
	if PolishCacheKey(a) != PolishCacheKey(b) {
		t.Fatal("whitespace changed the key")
	}
	c := a
	c.MBTI = "ISTJ"
	d := a
	d.Tier = "standard"
	if PolishCacheKey(a) == PolishCacheKey(c) || PolishCacheKey(a) == PolishCacheKey(d) {
		t.Fatal("MBTI or tier did not change the key")
	}
	if want := ":ko:zh-Hans:ENFP:light:" + llm.PolishPromptVersion; len(PolishCacheKey(a)) < len(want) || PolishCacheKey(a)[:3] != "po:" ||
		PolishCacheKey(a)[len(PolishCacheKey(a))-len(want):] != want {
		t.Fatalf("key %q", PolishCacheKey(a))
	}
}

func TestGateway_TranslateReportsUsage(t *testing.T) {
	p := &gwProvider{name: "claude", model: "claude-haiku-5-5", usage: llm.Usage{InputTokens: 100, OutputTokens: 20}}
	g := newTestGateway(polishCfg(), newGWCache(), newGWMeter(), nil, p)
	o, err := g.Translate(context.Background(), gwReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	want := int64((100*100000 + 20*500000) / 1_000_000)
	if o.Calls != 1 || o.CostMicros != want || o.Provider != "claude" || o.Model != "claude-haiku-5-5" || o.Usage.OutputTokens != 20 || o.Cached {
		t.Fatalf("outcome %+v want cost %d", o, want)
	}
}

package claude_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/pkg/llm"
	"github.com/jinheng0301/petbridge/pkg/llm/claude"
)

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func msgBody(text string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_x", "type": "message", "role": "assistant", "model": "claude-test",
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 21, "output_tokens": 7},
	})
	return string(b)
}

func newProvider(t *testing.T, h http.HandlerFunc) llm.Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return claude.New(claude.Config{APIKey: "test-key", Model: "claude-test", BaseURL: srv.URL})
}

var req = llm.TranslateRequest{Text: "你好", SrcLang: "zh-Hans", TgtLang: "ko"}

func TestClaude_Translate(t *testing.T) {
	var path, key, body string
	p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, key, body = r.URL.Path, r.Header.Get("X-Api-Key")+r.Header.Get("Authorization"), string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(msgBody(`{"text":"안녕하세요","risk":"none"}`)))
	})
	if p.Name() != "claude" {
		t.Errorf("name %q", p.Name())
	}
	res, err := p.Translate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "안녕하세요" || res.Risk != "none" || res.Model != "claude-test" || res.Usage != (llm.Usage{InputTokens: 21, OutputTokens: 7}) {
		t.Errorf("got %+v", res)
	}
	if !strings.HasSuffix(path, "/v1/messages") || !strings.Contains(key, "test-key") {
		t.Errorf("path %q key %q", path, key)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "claude-test" || sent["max_tokens"] != float64(2048) {
		t.Errorf("model/max_tokens: %v", sent)
	}
	oc, _ := sent["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if oc["effort"] != "low" || format["type"] != "json_schema" || format["schema"] == nil {
		t.Errorf("output_config: %v", oc)
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "thinking", "metadata"} {
		if _, has := sent[k]; has {
			t.Errorf("must not send %s", k)
		}
	}
	if uuidRe.MatchString(body) {
		t.Errorf("body contains a UUID-like string: %s", body)
	}
}

func TestClaude_BadResponses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"429", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, llm.ErrRateLimited},
		{"not json", 200, msgBody("hello there"), llm.ErrBadOutput},
		{"empty text", 200, msgBody(`{"text":"","risk":"none"}`), llm.ErrBadOutput},
		{"no content", 200, `{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`, llm.ErrBadOutput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			})
			_, err := p.Translate(context.Background(), req)
			if !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("SDK retried: %d calls", n)
			}
		})
	}

	t.Run("ctx timeout", func(t *testing.T) {
		p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-r.Context().Done():
			}
			_, _ = w.Write([]byte(msgBody(`{"text":"x","risk":"none"}`)))
		})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := p.Translate(ctx, req)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want DeadlineExceeded, got %v", err)
		}
	})
}

package gemini_test

import (
	"context"
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
	"github.com/jinheng0301/petbridge/pkg/llm/gemini"
)

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

const okBody = `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"text\":\"안녕하세요\",\"risk\":\"none\"}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":5}}`

func newProvider(t *testing.T, h http.HandlerFunc) llm.Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p, err := gemini.New(context.Background(), gemini.Config{APIKey: "test-key", Model: "gemini-test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var req = llm.TranslateRequest{Text: "你好", SrcLang: "zh-Hans", TgtLang: "ko"}

func TestGemini_Translate(t *testing.T) {
	var path, key, body string
	p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, key, body = r.URL.Path, r.Header.Get("x-goog-api-key"), string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	})
	if p.Name() != "gemini" {
		t.Errorf("name %q", p.Name())
	}
	res, err := p.Translate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "안녕하세요" || res.Risk != "none" || res.Model != "gemini-test" || res.Usage != (llm.Usage{InputTokens: 12, OutputTokens: 5}) {
		t.Errorf("got %+v", res)
	}
	if !strings.Contains(path, "gemini-test") || key != "test-key" {
		t.Errorf("path %q key %q", path, key)
	}
	if !strings.Contains(body, "application/json") || !strings.Contains(body, "你好") {
		t.Errorf("body lacks JSON mime or text: %s", body)
	}
	if uuidRe.MatchString(body) {
		t.Errorf("body contains a UUID-like string: %s", body)
	}
}

func TestGemini_BadResponses(t *testing.T) {
	text := func(s string) string {
		return `{"candidates":[{"content":{"role":"model","parts":[{"text":` + quote(s) + `}]}}]}`
	}
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"429", 429, `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`, llm.ErrRateLimited},
		{"not json", 200, text("hello there"), llm.ErrBadOutput},
		{"empty text", 200, text(`{"text":" ","risk":"none"}`), llm.ErrBadOutput},
		{"no candidates", 200, `{}`, llm.ErrBadOutput},
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
			_, _ = w.Write([]byte(okBody))
		})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := p.Translate(ctx, req)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want DeadlineExceeded, got %v", err)
		}
	})
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

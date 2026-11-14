package middleware

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func init() { gin.SetMode(gin.TestMode) }

func newEngine(buf *bytes.Buffer, h gin.HandlerFunc) *gin.Engine {
	l := logger.New("debug", buf)
	r := gin.New()
	r.Use(RequestID(), AccessLog(l), Recovery(l))
	r.Any("/x", h)
	return r
}

func do(r http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header[k] = []string{v} // bypass canonicalisation/validation of values
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequestID_AcceptsValidInbound(t *testing.T) {
	r := newEngine(&bytes.Buffer{}, func(c *gin.Context) { handler.OK(c, 200, nil) })
	w := do(r, "GET", "/x", "", map[string]string{"X-Request-Id": "abc12345-DEF_67"})
	if got := w.Header().Get("X-Request-ID"); got != "abc12345-DEF_67" {
		t.Fatalf("got %q", got)
	}
}

func TestRequestID_ReplacesUnsafeInbound(t *testing.T) {
	for _, in := range []string{"abc12345\ninjected", strings.Repeat("a", 65), "has space 123", ""} {
		r := newEngine(&bytes.Buffer{}, func(c *gin.Context) { handler.OK(c, 200, nil) })
		w := do(r, "GET", "/x", "", map[string]string{"X-Request-Id": in})
		got := w.Header().Get("X-Request-ID")
		if !idRe.MatchString(got) || got == in {
			t.Errorf("inbound %q -> %q", in, got)
		}
	}
}

func TestAccessLog_NoQueryNoBody(t *testing.T) {
	var buf bytes.Buffer
	r := newEngine(&buf, func(c *gin.Context) { handler.OK(c, 200, nil) })
	do(r, "POST", "/x?token=secret", "bodytext-private", nil)
	out := buf.String()
	if strings.Contains(out, "secret") || strings.Contains(out, "bodytext-private") {
		t.Fatalf("log leaks: %s", out)
	}
	if !strings.Contains(out, `"path":"/x"`) || !strings.Contains(out, `"status"`) {
		t.Fatalf("log missing fields: %s", out)
	}
}

func TestAccessLog_IncludesHandlerErrorsOnly(t *testing.T) {
	var buf bytes.Buffer
	r := gin.New()
	r.Use(RequestID(), AccessLog(logger.New("debug", &buf)))
	r.GET("/boom", func(c *gin.Context) { _ = c.Error(errors.New("db exploded")); c.Status(500) })
	r.GET("/fine", func(c *gin.Context) { c.Status(200) })
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/boom", nil))
	if !strings.Contains(buf.String(), "db exploded") {
		t.Fatalf("handler error missing from log: %s", buf.String())
	}
	buf.Reset()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/fine", nil))
	if strings.Contains(buf.String(), `"error"`) {
		t.Fatalf("unexpected error field: %s", buf.String())
	}
}

func TestRecovery_Returns500Envelope(t *testing.T) {
	var buf bytes.Buffer
	r := newEngine(&buf, func(c *gin.Context) { panic("boom-internal") })
	w := do(r, "GET", "/x", "", nil)
	if w.Code != 500 || !strings.Contains(w.Body.String(), `"INTERNAL"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "boom-internal") {
		t.Fatal("panic value leaked to response")
	}
	if !strings.Contains(buf.String(), "boom-internal") {
		t.Fatal("panic not logged")
	}
}

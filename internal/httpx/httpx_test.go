package httpx

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func init() { gin.SetMode(gin.TestMode) }

func newEngine(buf *bytes.Buffer, handler gin.HandlerFunc) *gin.Engine {
	l := NewLogger("debug", buf)
	r := gin.New()
	r.Use(RequestID(), AccessLog(l), Recovery(l))
	r.Any("/x", handler)
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
	r := newEngine(&bytes.Buffer{}, func(c *gin.Context) { OK(c, 200, nil) })
	w := do(r, "GET", "/x", "", map[string]string{"X-Request-Id": "abc12345-DEF_67"})
	if got := w.Header().Get("X-Request-ID"); got != "abc12345-DEF_67" {
		t.Fatalf("got %q", got)
	}
}

func TestRequestID_ReplacesUnsafeInbound(t *testing.T) {
	for _, in := range []string{"abc12345\ninjected", strings.Repeat("a", 65), "has space 123", ""} {
		r := newEngine(&bytes.Buffer{}, func(c *gin.Context) { OK(c, 200, nil) })
		w := do(r, "GET", "/x", "", map[string]string{"X-Request-Id": in})
		got := w.Header().Get("X-Request-ID")
		if !idRe.MatchString(got) || got == in {
			t.Errorf("inbound %q -> %q", in, got)
		}
	}
}

func TestFail_Envelope(t *testing.T) {
	r := newEngine(&bytes.Buffer{}, func(c *gin.Context) { Fail(c, 503, CodeNotReady, "x") })
	w := do(r, "GET", "/x", "", nil)
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 503 || body.Error.Code != "NOT_READY" || body.Error.Message != "x" ||
		body.Error.RequestID == "" || body.Error.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestOK_Envelope(t *testing.T) {
	r := newEngine(&bytes.Buffer{}, func(c *gin.Context) { OK(c, 200, map[string]string{"a": "b"}) })
	w := do(r, "GET", "/x", "", nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"data":{"a":"b"}}` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestAccessLog_NoQueryNoBody(t *testing.T) {
	var buf bytes.Buffer
	r := newEngine(&buf, func(c *gin.Context) { OK(c, 200, nil) })
	do(r, "POST", "/x?token=secret", "bodytext-private", nil)
	out := buf.String()
	if strings.Contains(out, "secret") || strings.Contains(out, "bodytext-private") {
		t.Fatalf("log leaks: %s", out)
	}
	if !strings.Contains(out, `"path":"/x"`) || !strings.Contains(out, `"status"`) {
		t.Fatalf("log missing fields: %s", out)
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

package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestFailDetails_IncludesDetailsAndEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	FailDetails(c, http.StatusBadRequest, CodeValidationFailed, "bad", gin.H{"fields": []string{"mbti"}})
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Fields []string `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 400 || body.Error.Code != "VALIDATION_FAILED" || len(body.Error.Details.Fields) != 1 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestErrorCodeConstants(t *testing.T) {
	for got, want := range map[string]string{
		CodeUnauthorized:          "UNAUTHORIZED",
		CodeValidationFailed:      "VALIDATION_FAILED",
		CodeAccountDisabled:       "ACCOUNT_DISABLED",
		CodeRefreshReused:         "REFRESH_REUSED",
		CodeRefreshInvalid:        "REFRESH_INVALID",
		CodeProviderNotConfigured: "PROVIDER_NOT_CONFIGURED",
	} {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func TestAccessLog_IncludesHandlerErrorsOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	r := gin.New()
	r.Use(RequestID(), AccessLog(NewLogger("debug", &buf)))
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

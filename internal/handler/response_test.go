package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/model"
)

func TestFail_Envelope(t *testing.T) {
	r := newEngine(&bytes.Buffer{}, func(c *gin.Context) { handler.Fail(c, 503, model.CodeNotReady, "x") })
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
	r := newEngine(&bytes.Buffer{}, func(c *gin.Context) { handler.OK(c, 200, map[string]string{"a": "b"}) })
	w := do(r, "GET", "/x", "", nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"data":{"a":"b"}}` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestFailDetails_IncludesDetailsAndEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	handler.FailDetails(c, http.StatusBadRequest, model.CodeValidationFailed, "bad", gin.H{"fields": []string{"mbti"}})
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
		model.CodeUnauthorized:          "UNAUTHORIZED",
		model.CodeValidationFailed:      "VALIDATION_FAILED",
		model.CodeAccountDisabled:       "ACCOUNT_DISABLED",
		model.CodeRefreshReused:         "REFRESH_REUSED",
		model.CodeRefreshInvalid:        "REFRESH_INVALID",
		model.CodeProviderNotConfigured: "PROVIDER_NOT_CONFIGURED",
	} {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

package service

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testKey = "test-signing-key-test-signing-key"

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)} }

func newTestIssuer(c *clock) *Issuer { return NewIssuer(testKey, 15*time.Minute, c.now) }

func TestSignParse_RoundTrip(t *testing.T) {
	iss := newTestIssuer(newClock())
	tok, err := iss.Sign("u1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := iss.Parse(tok)
	if err != nil || got != "u1" {
		t.Fatalf("Parse = %q, %v", got, err)
	}
	if iss.TTL() != 15*time.Minute {
		t.Fatalf("TTL = %v", iss.TTL())
	}
}

func TestParse_Expired(t *testing.T) {
	c := newClock()
	iss := newTestIssuer(c)
	tok, _ := iss.Sign("u1")
	c.advance(15*time.Minute + time.Second)
	if _, err := iss.Parse(tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("err = %v, want ErrTokenExpired", err)
	}
}

func TestParse_TamperedSignature(t *testing.T) {
	iss := newTestIssuer(newClock())
	tok, _ := iss.Sign("u1")
	parts := strings.Split(tok, ".")
	parts[2] = parts[2][:len(parts[2])-2] + "AA"
	if parts[2] == strings.Split(tok, ".")[2] {
		parts[2] = parts[2][:len(parts[2])-2] + "BB"
	}
	if _, err := iss.Parse(strings.Join(parts, ".")); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestParse_WrongKey(t *testing.T) {
	c := newClock()
	tok, _ := newTestIssuer(c).Sign("u1")
	other := NewIssuer("another-key-another-key-another-key", 15*time.Minute, c.now)
	if _, err := other.Parse(tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestParse_AlgNoneRejected(t *testing.T) {
	c := newClock()
	iss := newTestIssuer(c)
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := enc([]byte(`{"sub":"u1","exp":` + "9999999999" + `}`))
	if _, err := iss.Parse(header + "." + payload + "."); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestParse_WrongAlgHS512Rejected(t *testing.T) {
	c := newClock()
	iss := newTestIssuer(c)
	claims := jwt.RegisteredClaims{Subject: "u1", ExpiresAt: jwt.NewNumericDate(c.now().Add(time.Hour))}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString([]byte(testKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Parse(tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestParse_GarbageInputs(t *testing.T) {
	iss := newTestIssuer(newClock())
	for _, in := range []string{"", "a.b", "a.b.c", strings.Repeat("x", 100000)} {
		if _, err := iss.Parse(in); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("input len %d: err = %v, want ErrTokenInvalid", len(in), err)
		}
	}
}

func TestParse_MissingSubjectRejected(t *testing.T) {
	c := newClock()
	iss := newTestIssuer(c)
	claims := jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(c.now().Add(time.Hour))}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testKey))
	if _, err := iss.Parse(tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestNewRefreshToken(t *testing.T) {
	raw1, hash1, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	raw2, _, _ := NewRefreshToken()
	if raw1 == raw2 {
		t.Fatal("raw tokens must differ")
	}
	if HashRefreshToken(raw1) != hash1 {
		t.Fatal("hash mismatch")
	}
	if len(raw1) < 43 || len(hash1) != 64 || strings.ToLower(hash1) != hash1 {
		t.Fatalf("raw len %d, hash %q", len(raw1), hash1)
	}
}

func TestExpiry_ReturnsExpClaim(t *testing.T) {
	c := newClock()
	iss := newTestIssuer(c)
	tok, _ := iss.Sign("u1")
	exp, err := iss.Expiry(tok)
	if err != nil || !exp.Equal(c.now().Add(15*time.Minute)) {
		t.Fatalf("Expiry = %v, %v", exp, err)
	}
	if _, err := iss.Expiry("garbage"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("garbage err = %v", err)
	}
	c.advance(16 * time.Minute)
	if _, err := iss.Expiry(tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired err = %v", err)
	}
}

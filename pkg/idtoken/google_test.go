package idtoken

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testClientID = "client-123.apps.googleusercontent.com"

var testNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

type jwksServer struct {
	srv   *httptest.Server
	hits  atomic.Int32
	serve func(hit int32) []jwkKey
}

type jwkKey struct {
	kid string
	pub *rsa.PublicKey
}

func newJWKSServer(t *testing.T, serve func(hit int32) []jwkKey) *jwksServer {
	t.Helper()
	s := &jwksServer{serve: serve}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit := s.hits.Add(1)
		var body struct {
			Keys []map[string]string `json:"keys"`
		}
		for _, k := range s.serve(hit) {
			body.Keys = append(body.Keys, map[string]string{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": k.kid,
				"n": base64.RawURLEncoding.EncodeToString(k.pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.pub.E)).Bytes()),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": "https://accounts.google.com",
		"aud": testClientID,
		"sub": "google-sub-1",
		"iat": testNow.Add(-time.Minute).Unix(),
		"exp": testNow.Add(time.Hour).Unix(),
	}
}

func signRS(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newVerifier(s *jwksServer) *GoogleVerifier {
	return NewGoogleVerifier(testClientID,
		WithJWKSURL(s.srv.URL), WithHTTPClient(s.srv.Client()),
		WithClock(func() time.Time { return testNow }))
}

func TestGoogle_ValidToken(t *testing.T) {
	key := newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	sub, err := newVerifier(s).Verify(context.Background(), signRS(t, key, "k1", validClaims()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if sub != "google-sub-1" {
		t.Fatalf("sub = %q", sub)
	}
}

func TestGoogle_CachesJWKS(t *testing.T) {
	key := newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	v := newVerifier(s)
	for range 3 {
		if _, err := v.Verify(context.Background(), signRS(t, key, "k1", validClaims())); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	}
	if got := s.hits.Load(); got != 1 {
		t.Fatalf("JWKS fetched %d times, want 1", got)
	}
}

func expectInvalid(t *testing.T, v *GoogleVerifier, token string) {
	t.Helper()
	sub, err := v.Verify(context.Background(), token)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
	if sub != "" {
		t.Fatalf("sub = %q, want empty", sub)
	}
}

func TestGoogle_WrongAudience(t *testing.T) {
	key := newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	c := validClaims()
	c["aud"] = "someone-else.apps.googleusercontent.com"
	expectInvalid(t, newVerifier(s), signRS(t, key, "k1", c))
}

func TestGoogle_WrongIssuer(t *testing.T) {
	key := newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	c := validClaims()
	c["iss"] = "https://evil.example.com"
	expectInvalid(t, newVerifier(s), signRS(t, key, "k1", c))
}

func TestGoogle_Expired(t *testing.T) {
	key := newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	c := validClaims()
	c["exp"] = testNow.Add(-time.Hour).Unix()
	expectInvalid(t, newVerifier(s), signRS(t, key, "k1", c))
}

func TestGoogle_MissingSubject(t *testing.T) {
	key := newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	c := validClaims()
	delete(c, "sub")
	expectInvalid(t, newVerifier(s), signRS(t, key, "k1", c))
}

func TestGoogle_WrongSigningKey(t *testing.T) {
	key, other := newKey(t), newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	expectInvalid(t, newVerifier(s), signRS(t, other, "k1", validClaims()))
}

func TestGoogle_UnknownKidRefetchesOnce(t *testing.T) {
	oldKey, newK := newKey(t), newKey(t)
	s := newJWKSServer(t, func(hit int32) []jwkKey {
		if hit == 1 {
			return []jwkKey{{"old", &oldKey.PublicKey}}
		}
		return []jwkKey{{"old", &oldKey.PublicKey}, {"new", &newK.PublicKey}}
	})
	now := testNow
	v := NewGoogleVerifier(testClientID, WithJWKSURL(s.srv.URL), WithHTTPClient(s.srv.Client()),
		WithClock(func() time.Time { return now }))
	// Warm the cache with the old key set.
	if _, err := v.Verify(context.Background(), signRS(t, oldKey, "old", validClaims())); err != nil {
		t.Fatalf("Verify old: %v", err)
	}
	now = now.Add(time.Minute) // past the forced-refetch guard, still inside max-age
	sub, err := v.Verify(context.Background(), signRS(t, newK, "new", validClaims()))
	if err != nil {
		t.Fatalf("Verify new: %v", err)
	}
	if sub != "google-sub-1" {
		t.Fatalf("sub = %q", sub)
	}
	if got := s.hits.Load(); got != 2 {
		t.Fatalf("JWKS fetched %d times, want 2", got)
	}
}

func TestGoogle_UnknownKidAfterRefetchFails(t *testing.T) {
	key := newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	v := newVerifier(s)
	expectInvalid(t, v, signRS(t, key, "nope", validClaims()))
	// First lookup fetches; the unknown kid triggers at most one more fetch.
	if got := s.hits.Load(); got > 2 {
		t.Fatalf("JWKS fetched %d times, want at most 2", got)
	}
}

func TestGoogle_BothIssuerForms(t *testing.T) {
	key := newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	v := newVerifier(s)
	for _, iss := range []string{"accounts.google.com", "https://accounts.google.com"} {
		c := validClaims()
		c["iss"] = iss
		if _, err := v.Verify(context.Background(), signRS(t, key, "k1", c)); err != nil {
			t.Fatalf("iss %q: %v", iss, err)
		}
	}
}

func TestGoogle_RejectsHS256(t *testing.T) {
	key := newKey(t)
	s := newJWKSServer(t, func(int32) []jwkKey { return []jwkKey{{"k1", &key.PublicKey}} })
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString([]byte("shared-secret-shared-secret-32by"))
	if err != nil {
		t.Fatal(err)
	}
	expectInvalid(t, newVerifier(s), signed)
}

func TestGoogle_JWKSUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	key := newKey(t)
	v := NewGoogleVerifier(testClientID, WithJWKSURL(srv.URL), WithHTTPClient(srv.Client()),
		WithClock(func() time.Time { return testNow }))
	sub, err := v.Verify(context.Background(), signRS(t, key, "k1", validClaims()))
	if err == nil || sub != "" {
		t.Fatalf("want error, got sub=%q err=%v", sub, err)
	}
}

package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrTokenInvalid = errors.New("token invalid")
	ErrTokenExpired = errors.New("token expired")
)

// Issuer signs and verifies access tokens (JWT, HS256 only).
type Issuer struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

func NewIssuer(key string, ttl time.Duration, now func() time.Time) *Issuer {
	return &Issuer{key: []byte(key), ttl: ttl, now: now}
}

func (i *Issuer) TTL() time.Duration { return i.ttl }

// Sign returns an access token whose subject is userID.
func (i *Issuer) Sign(userID string) (string, error) {
	n := i.now()
	claims := jwt.RegisteredClaims{
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(n),
		ExpiresAt: jwt.NewNumericDate(n.Add(i.ttl)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.key)
}

// Parse verifies signature, algorithm and expiry and returns the subject.
func (i *Issuer) Parse(token string) (string, error) {
	claims, err := i.claims(token)
	if err != nil {
		return "", err
	}
	return claims.Subject, nil
}

// Expiry verifies the token like Parse and returns when it expires. The
// WebSocket handler closes the connection at that moment.
func (i *Issuer) Expiry(token string) (time.Time, error) {
	claims, err := i.claims(token)
	if err != nil {
		return time.Time{}, err
	}
	return claims.ExpiresAt.Time, nil
}

func (i *Issuer) claims(token string) (*jwt.RegisteredClaims, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(token, claims,
		func(*jwt.Token) (any, error) { return i.key, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithTimeFunc(i.now),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}
	if claims.Subject == "" {
		return nil, ErrTokenInvalid
	}
	return claims, nil
}

// NewRefreshToken returns 32 random bytes (base64url) and its stored hash.
func NewRefreshToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashRefreshToken(raw), nil
}

// HashRefreshToken is the lower-case hex SHA-256 stored in refresh_tokens.
func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

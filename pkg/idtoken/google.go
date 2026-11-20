package idtoken

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidToken means the ID token failed signature or claim checks.
var ErrInvalidToken = errors.New("invalid id token")

// ErrUnavailable marks failures to download the provider's signing keys: an
// outage on their side, not a bad token. The API answers 503 so the client
// retries instead of treating the login as rejected.
var ErrUnavailable = errors.New("fetch signing keys")

const (
	defaultGoogleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"
	defaultJWKSMaxAge    = time.Hour
	jwksFetchTimeout     = 5 * time.Second
	// minForcedRefetch bounds how often an unknown kid may trigger a JWKS
	// download, so random kids cannot turn logins into a fetch storm.
	minForcedRefetch = 30 * time.Second
	clockLeeway      = 30 * time.Second
	maxJWKSBody      = 1 << 20
)

var googleIssuers = map[string]bool{
	"accounts.google.com":         true,
	"https://accounts.google.com": true,
}

// GoogleVerifier validates Google ID tokens against Google's published JWKS.
type GoogleVerifier struct {
	clientID string
	jwksURL  string
	client   *http.Client
	now      func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	expiresAt time.Time
	fetchedAt time.Time
}

// GoogleOption customises a GoogleVerifier.
type GoogleOption func(*GoogleVerifier)

// WithJWKSURL overrides the JWKS endpoint (tests).
func WithJWKSURL(url string) GoogleOption { return func(v *GoogleVerifier) { v.jwksURL = url } }

// WithHTTPClient overrides the HTTP client used to fetch the JWKS.
func WithHTTPClient(c *http.Client) GoogleOption { return func(v *GoogleVerifier) { v.client = c } }

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) GoogleOption { return func(v *GoogleVerifier) { v.now = now } }

// NewGoogleVerifier returns a verifier that accepts tokens issued to clientID.
func NewGoogleVerifier(clientID string, opts ...GoogleOption) *GoogleVerifier {
	v := &GoogleVerifier{
		clientID: clientID,
		jwksURL:  defaultGoogleJWKSURL,
		client:   &http.Client{Timeout: jwksFetchTimeout},
		now:      time.Now,
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

// Verify checks the token's RS256 signature, issuer, audience and expiry and
// returns its subject.
func (v *GoogleVerifier) Verify(ctx context.Context, idToken string) (string, error) {
	tok, err := jwt.Parse(idToken,
		func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			if kid == "" {
				return nil, errors.New("missing kid")
			}
			return v.key(ctx, kid)
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithAudience(v.clientID),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(clockLeeway),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return "", err
		}
		return "", fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	iss, _ := tok.Claims.GetIssuer()
	if !googleIssuers[iss] {
		return "", fmt.Errorf("%w: unexpected issuer", ErrInvalidToken)
	}
	sub, _ := tok.Claims.GetSubject()
	if sub == "" {
		return "", fmt.Errorf("%w: missing subject", ErrInvalidToken)
	}
	return sub, nil
}

// key returns the public key for kid, refreshing the cached JWKS when it has
// expired or, at most once per minForcedRefetch, when kid is unknown.
func (v *GoogleVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.now()
	if v.keys == nil || !now.Before(v.expiresAt) {
		if err := v.refresh(ctx, now); err != nil {
			return nil, err
		}
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if now.Sub(v.fetchedAt) < minForcedRefetch {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	if err := v.refresh(ctx, now); err != nil {
		return nil, err
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

// refresh downloads the JWKS. Callers hold v.mu.
func (v *GoogleVerifier) refresh(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	var body struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBody)).Decode(&body); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	keys := make(map[string]*rsa.PublicKey, len(body.Keys))
	for _, k := range body.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := rsaKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return fmt.Errorf("%w: no usable RSA keys", ErrUnavailable)
	}
	v.keys = keys
	v.fetchedAt = now
	v.expiresAt = now.Add(maxAge(resp.Header.Get("Cache-Control")))
	return nil
}

func rsaKey(n, e string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, err
	}
	exp := new(big.Int).SetBytes(eb)
	if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > 1<<31-1 {
		return nil, errors.New("bad exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(exp.Int64())}, nil
}

// maxAge reads max-age from a Cache-Control header, defaulting to an hour.
func maxAge(cacheControl string) time.Duration {
	for _, part := range strings.Split(cacheControl, ",") {
		name, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(name, "max-age") {
			continue
		}
		if secs, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultJWKSMaxAge
}

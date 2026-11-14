package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
)

var bg = context.Background()

func setup(t *testing.T) (*Service, *pgxpool.Pool, *clock) {
	t.Helper()
	pool := dbtest.Reset(t)
	c := newClock()
	svc := NewService(pool, NewIssuer(testKey, 15*time.Minute, c.now), 720*time.Hour)
	svc.now = c.now
	return svc, pool, c
}

func count(t *testing.T, p *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(bg, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLogin_CreatesUserAndIdentityOnce(t *testing.T) {
	svc, pool, _ := setup(t)
	a, err := svc.LoginWithIdentity(bg, "dev", "a@local", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.LoginWithIdentity(bg, "dev", "a@local", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.UserID != b.UserID || a.Onboarded {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if count(t, pool, `select count(*) from users`) != 1 || count(t, pool, `select count(*) from auth_identities`) != 1 {
		t.Fatal("want exactly one user and one identity")
	}
	if a.ExpiresIn != 900 || a.AccessToken == "" || a.RefreshToken == "" {
		t.Fatalf("tokens = %+v", a.Tokens)
	}
}

func TestLogin_ConcurrentFirstLoginCreatesOneUser(t *testing.T) {
	svc, pool, _ := setup(t)
	const n = 16
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := svc.LoginWithIdentity(bg, "dev", "race@local", "")
			ids[i], errs[i] = r.UserID, err
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("goroutine %d: id=%q err=%v (first id %q)", i, ids[i], errs[i], ids[0])
		}
	}
	if got := count(t, pool, `select count(*) from users`); got != 1 {
		t.Fatalf("users = %d, want 1", got)
	}
}

func TestLogin_BannedUserRejected(t *testing.T) {
	for _, status := range []string{"banned", "deleted"} {
		svc, pool, _ := setup(t)
		r, _ := svc.LoginWithIdentity(bg, "dev", "x@local", "")
		if _, err := pool.Exec(bg, `update users set status=$1 where id=$2`, status, r.UserID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.LoginWithIdentity(bg, "dev", "x@local", ""); !errors.Is(err, ErrAccountDisabled) {
			t.Fatalf("%s: err = %v", status, err)
		}
	}
}

func TestLogin_StoresOnlyTokenHash(t *testing.T) {
	svc, pool, _ := setup(t)
	r, _ := svc.LoginWithIdentity(bg, "dev", "a@local", "dev-1")
	var stored string
	if err := pool.QueryRow(bg, `select token_hash from refresh_tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != HashRefreshToken(r.RefreshToken) || stored == r.RefreshToken {
		t.Fatalf("stored %q", stored)
	}
	if n := count(t, pool, `select count(*) from refresh_tokens r where r::text like '%' || $1 || '%'`, r.RefreshToken); n != 0 {
		t.Fatal("raw refresh token found in database")
	}
}

func TestRefresh_RotatesAndKeepsFamily(t *testing.T) {
	svc, pool, _ := setup(t)
	r, _ := svc.LoginWithIdentity(bg, "dev", "a@local", "")
	r2, err := svc.Refresh(bg, r.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if r2.RefreshToken == r.RefreshToken || r2.UserID != r.UserID {
		t.Fatalf("r2=%+v", r2)
	}
	var revoked bool
	var replacedBy, newID, famOld, famNew string
	err = pool.QueryRow(bg, `select revoked_at is not null, coalesce(replaced_by::text,''), family_id::text
		from refresh_tokens where token_hash=$1`, HashRefreshToken(r.RefreshToken)).Scan(&revoked, &replacedBy, &famOld)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(bg, `select id::text, family_id::text from refresh_tokens where token_hash=$1`,
		HashRefreshToken(r2.RefreshToken)).Scan(&newID, &famNew)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked || replacedBy != newID || famOld != famNew {
		t.Fatalf("revoked=%v replaced_by=%q new=%q fam %q/%q", revoked, replacedBy, newID, famOld, famNew)
	}
}

func TestRefresh_ReplayRevokesWholeFamily(t *testing.T) {
	svc, _, _ := setup(t)
	r, _ := svc.LoginWithIdentity(bg, "dev", "a@local", "")
	r2, err := svc.Refresh(bg, r.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(bg, r.RefreshToken); !errors.Is(err, ErrRefreshReused) {
		t.Fatalf("replay err = %v", err)
	}
	if _, err := svc.Refresh(bg, r2.RefreshToken); err == nil {
		t.Fatal("new token must be unusable after replay")
	}
}

func TestRefresh_ConcurrentSameToken(t *testing.T) {
	svc, _, _ := setup(t)
	r, _ := svc.LoginWithIdentity(bg, "dev", "a@local", "")
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Refresh(bg, r.RefreshToken)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrRefreshReused):
			t.Fatalf("unexpected err %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("successes = %d, want exactly 1", ok)
	}
}

func TestRefresh_ExpiredAndUnknown(t *testing.T) {
	svc, _, c := setup(t)
	r, _ := svc.LoginWithIdentity(bg, "dev", "a@local", "")
	if _, err := svc.Refresh(bg, "totally-unknown-token"); !errors.Is(err, ErrRefreshInvalid) {
		t.Fatalf("unknown err = %v", err)
	}
	c.advance(720*time.Hour + time.Minute)
	if _, err := svc.Refresh(bg, r.RefreshToken); !errors.Is(err, ErrRefreshInvalid) {
		t.Fatalf("expired err = %v", err)
	}
}

func TestRefresh_BannedUserRejected(t *testing.T) {
	svc, pool, _ := setup(t)
	r, _ := svc.LoginWithIdentity(bg, "dev", "a@local", "")
	if _, err := pool.Exec(bg, `update users set status='banned' where id=$1`, r.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(bg, r.RefreshToken); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := svc.Refresh(bg, r.RefreshToken); err == nil {
		t.Fatal("token must not be reusable after a banned refresh attempt")
	}
}

func TestLogout_RevokesFamilyAndIsIdempotent(t *testing.T) {
	svc, _, _ := setup(t)
	r, _ := svc.LoginWithIdentity(bg, "dev", "a@local", "")
	if err := svc.Logout(bg, r.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(bg, r.RefreshToken); err == nil {
		t.Fatal("refresh must fail after logout")
	}
	if err := svc.Logout(bg, r.RefreshToken); err != nil {
		t.Fatalf("second logout: %v", err)
	}
	if err := svc.Logout(bg, "never-issued"); err != nil {
		t.Fatalf("unknown token logout: %v", err)
	}
}

func TestEnsureUser_NoTokensIssued(t *testing.T) {
	svc, pool, _ := setup(t)
	a, err := svc.EnsureUser(bg, "dev", "seed@local")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := svc.EnsureUser(bg, "dev", "seed@local")
	if a == "" || a != b {
		t.Fatalf("a=%q b=%q", a, b)
	}
	if count(t, pool, `select count(*) from refresh_tokens`) != 0 {
		t.Fatal("EnsureUser must not issue tokens")
	}
	r, _ := svc.LoginWithIdentity(bg, "dev", "seed@local", "")
	if r.UserID != a {
		t.Fatal("login must find the ensured user")
	}
}

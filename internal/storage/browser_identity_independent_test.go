// SPDX-License-Identifier: Elastic-2.0
package storage

// Independent durable-store tests. These do not replace end-to-end IdP/TLS,
// ingress, browser cookie, CSRF or the API organization-authorization tests.
import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	oidcidentity "agentplatform.local/platform/internal/identity"
)

func independentLogin(t *testing.T, s *Store, ttl time.Duration) oidcidentity.LoginTransaction {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	x := oidcidentity.LoginTransaction{StateHash: Hash([]byte(domain.NewID("state"))), BindingHash: Hash([]byte(domain.NewID("binding"))), Issuer: "https://issuer.example.test", ClientID: "platform-client", RedirectURI: "https://platform.example.test/auth/oidc/callback", Nonce: domain.NewID("nonce"), CodeVerifier: domain.NewID("verifier"), CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if e := s.CreateLogin(context.Background(), x); e != nil {
		t.Fatal(e)
	}
	return x
}
func independentSession(t *testing.T, s *Store, p domain.Principal, subject string, ttl time.Duration) oidcidentity.BrowserSession {
	t.Helper()
	ctx := context.Background()
	issuer := "https://issuer.example.test"
	if e := s.LinkIdentity(ctx, issuer, subject, p.ID); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	x := oidcidentity.BrowserSession{ID: domain.NewID("ses"), TokenHash: Hash([]byte(domain.NewID("browser-secret"))), PrincipalID: p.ID, Issuer: issuer, Subject: subject, AuthenticatedAt: now, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if e := s.CreateSession(ctx, x, ""); e != nil {
		t.Fatal(e)
	}
	return x
}
func independentIdentityError(t *testing.T, e, want error) {
	t.Helper()
	if !errors.Is(e, want) {
		t.Fatalf("wanted %v, got %v", want, e)
	}
}
func independentLoginKey(t *testing.T, s *Store) []byte {
	t.Helper()
	key := bytes.Repeat([]byte{0x37}, 32)
	if e := s.ConfigureLoginEncryption(key); e != nil {
		t.Fatal(e)
	}
	return key
}

func TestIndependentBrowserLoginEncryptionBindingAndSingleUse(t *testing.T) {
	s, dsn := testStore(t)
	ctx := context.Background()
	key := independentLoginKey(t, s)
	x := independentLogin(t, s, time.Minute)
	var encrypted []byte
	if e := s.Pool.QueryRow(ctx, "SELECT code_verifier_ciphertext FROM login_transactions WHERE state_hash=$1", x.StateHash).Scan(&encrypted); e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(encrypted, []byte(x.CodeVerifier)) || len(encrypted) <= len(x.CodeVerifier) {
		t.Fatal("PKCE verifier stored as plaintext")
	}
	if _, e := s.ConsumeLogin(ctx, x.StateHash, Hash([]byte("wrong browser"))); !errors.Is(e, oidcidentity.ErrLoginRejected) {
		t.Fatalf("wrong binding: %v", e)
	}
	reopened, e := Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if e = reopened.ConfigureLoginEncryption(key); e != nil {
		t.Fatal(e)
	}
	const n = 16
	var wg sync.WaitGroup
	results := make(chan oidcidentity.LoginTransaction, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := reopened.ConsumeLogin(ctx, x.StateHash, x.BindingHash)
			results <- got
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	successes := 0
	for e := range errs {
		if e == nil {
			successes++
		} else {
			independentIdentityError(t, e, oidcidentity.ErrLoginRejected)
		}
	}
	if successes != 1 {
		t.Fatalf("login consumed %d times", successes)
	}
	for got := range results {
		if got.CodeVerifier != "" && (got.CodeVerifier != x.CodeVerifier || got.StateHash != x.StateHash || got.BindingHash != x.BindingHash || got.Issuer != x.Issuer) {
			t.Fatal("wrong decrypted login binding")
		}
	}
	var consumed bool
	var size int
	if e = s.Pool.QueryRow(ctx, "SELECT consumed_at IS NOT NULL,octet_length(code_verifier_ciphertext) FROM login_transactions WHERE state_hash=$1", x.StateHash).Scan(&consumed, &size); e != nil || !consumed || size != 0 {
		t.Fatalf("consumed credential not wiped: %v/%d %v", consumed, size, e)
	}
}
func TestIndependentBrowserLoginWrongKeyTamperAndSwappedCiphertextFailClosed(t *testing.T) {
	for _, mode := range []string{"wrong-key", "aad", "ciphertext", "swap"} {
		t.Run(mode, func(t *testing.T) {
			s, dsn := testStore(t)
			ctx := context.Background()
			independentLoginKey(t, s)
			x := independentLogin(t, s, time.Minute)
			consumer := s
			switch mode {
			case "wrong-key":
				var e error
				consumer, e = Open(ctx, dsn)
				if e != nil {
					t.Fatal(e)
				}
				defer consumer.Close()
				if e = consumer.ConfigureLoginEncryption(bytes.Repeat([]byte{0x38}, 32)); e != nil {
					t.Fatal(e)
				}
			case "aad":
				if _, e := s.Pool.Exec(ctx, "UPDATE login_transactions SET nonce='changed-nonce' WHERE state_hash=$1", x.StateHash); e != nil {
					t.Fatal(e)
				}
			case "ciphertext":
				if _, e := s.Pool.Exec(ctx, "UPDATE login_transactions SET code_verifier_ciphertext=set_byte(code_verifier_ciphertext,20,(get_byte(code_verifier_ciphertext,20)+1)%256) WHERE state_hash=$1", x.StateHash); e != nil {
					t.Fatal(e)
				}
			case "swap":
				other := independentLogin(t, s, time.Minute)
				if _, e := s.Pool.Exec(ctx, "UPDATE login_transactions SET code_verifier_ciphertext=(SELECT code_verifier_ciphertext FROM login_transactions WHERE state_hash=$2) WHERE state_hash=$1", x.StateHash, other.StateHash); e != nil {
					t.Fatal(e)
				}
			}
			got, e := consumer.ConsumeLogin(ctx, x.StateHash, x.BindingHash)
			independentIdentityError(t, e, oidcidentity.ErrLoginRejected)
			if got.CodeVerifier != "" {
				t.Fatal("decryption failure returned credential")
			}
			_, e = s.ConsumeLogin(ctx, x.StateHash, x.BindingHash)
			independentIdentityError(t, e, oidcidentity.ErrLoginRejected)
			var size int
			if e = s.Pool.QueryRow(ctx, "SELECT octet_length(code_verifier_ciphertext) FROM login_transactions WHERE state_hash=$1", x.StateHash).Scan(&size); e != nil || size != 0 {
				t.Fatal("failed consumed login retained encrypted verifier")
			}
		})
	}
}
func TestIndependentBrowserLoginExpiredDuringRowWait(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	independentLoginKey(t, s)
	x := independentLogin(t, s, 400*time.Millisecond)
	blocker, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	if _, e = blocker.Exec(ctx, "SELECT state_hash FROM login_transactions WHERE state_hash=$1 FOR UPDATE", x.StateHash); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := s.ConsumeLogin(ctx, x.StateHash, x.BindingHash); done <- e }()
	waitForBlockedQuery(t, s, "WITH selected AS")
	for {
		var expired bool
		if e = s.Pool.QueryRow(ctx, "SELECT expires_at<=clock_timestamp() FROM login_transactions WHERE state_hash=$1", x.StateHash).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if expired {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	independentIdentityError(t, <-done, oidcidentity.ErrLoginRejected)
	var consumed bool
	if e = s.Pool.QueryRow(ctx, "SELECT consumed_at IS NOT NULL FROM login_transactions WHERE state_hash=$1", x.StateHash).Scan(&consumed); e != nil || consumed {
		t.Fatal("expired login was consumed after lock wait")
	}
}
func TestIndependentBrowserLoginDurableTTLAndEncryptionRequired(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	x := oidcidentity.LoginTransaction{StateHash: Hash([]byte("ttl-state")), BindingHash: Hash([]byte("ttl-bind")), Issuer: "https://issuer.example.test", ClientID: "client", RedirectURI: "https://platform.example.test/auth/oidc/callback", Nonce: "nonce", CodeVerifier: "verifier", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	independentIdentityError(t, s.CreateLogin(ctx, x), oidcidentity.ErrLoginStoreUnavailable)
	for _, size := range []int{0, 16, 31, 33} {
		if e := s.ConfigureLoginEncryption(make([]byte, size)); e == nil {
			t.Fatal("invalid encryption key length accepted")
		}
	}
	independentLoginKey(t, s)
	if e := s.CreateLogin(ctx, x); e != nil {
		t.Fatal(e)
	}
	x.StateHash = Hash([]byte("ttl-too-long"))
	x.ExpiresAt = x.CreatedAt.Add(10*time.Minute + time.Second)
	independentIdentityError(t, s.CreateLogin(ctx, x), oidcidentity.ErrLoginRejected)
	x.StateHash = Hash([]byte("ttl-expired"))
	x.CreatedAt = now.Add(-time.Minute)
	x.ExpiresAt = now.Add(-time.Second)
	independentIdentityError(t, s.CreateLogin(ctx, x), oidcidentity.ErrLoginRejected)
}

func TestIndependentBrowserSessionRotationCASAndAbsoluteExpiry(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 1000)
	old := independentSession(t, s, p, "stable-subject", time.Hour)
	for _, change := range []func(*oidcidentity.BrowserSession){func(x *oidcidentity.BrowserSession) { x.ExpiresAt = x.ExpiresAt.Add(time.Second) }, func(x *oidcidentity.BrowserSession) { x.AuthenticatedAt = x.AuthenticatedAt.Add(-time.Second) }, func(x *oidcidentity.BrowserSession) { x.Subject = "different-subject" }} {
		bad := old
		bad.ID = domain.NewID("ses")
		bad.TokenHash = Hash([]byte(bad.ID))
		change(&bad)
		independentIdentityError(t, s.RotateSession(ctx, old.TokenHash, bad), oidcidentity.ErrSessionInvalid)
	}
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	winners := make(chan oidcidentity.BrowserSession, n)
	for range n {
		replacement := old
		replacement.ID = domain.NewID("ses")
		replacement.TokenHash = Hash([]byte(replacement.ID))
		replacement.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
		wg.Add(1)
		go func(x oidcidentity.BrowserSession) {
			defer wg.Done()
			e := s.RotateSession(ctx, old.TokenHash, x)
			errs <- e
			if e == nil {
				winners <- x
			}
		}(replacement)
	}
	wg.Wait()
	close(errs)
	close(winners)
	success := 0
	for e := range errs {
		if e == nil {
			success++
		} else {
			independentIdentityError(t, e, oidcidentity.ErrSessionInvalid)
		}
	}
	if success != 1 {
		t.Fatalf("rotation CAS produced %d replacements", success)
	}
	_, e := s.GetSession(ctx, old.TokenHash)
	independentIdentityError(t, e, oidcidentity.ErrSessionInvalid)
	for winner := range winners {
		got, e := s.GetSession(ctx, winner.TokenHash)
		if e != nil || !got.ExpiresAt.Equal(old.ExpiresAt) || !got.AuthenticatedAt.Equal(old.AuthenticatedAt) {
			t.Fatal("rotation extended absolute login lifetime")
		}
	}
	var count int
	if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM browser_sessions WHERE principal_id=$1 AND NOT revoked", p.ID).Scan(&count); e != nil || count != 1 {
		t.Fatalf("active replacement count %d: %v", count, e)
	}
}
func TestIndependentBrowserSessionRotationExpiryDuringLockWait(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, _ := identity(t, s, 1000)
	old := independentSession(t, s, p, "expiry-subject", 450*time.Millisecond)
	blocker, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	if _, e = blocker.Exec(ctx, "SELECT token_hash FROM browser_sessions WHERE token_hash=$1 FOR UPDATE", old.TokenHash); e != nil {
		t.Fatal(e)
	}
	replacement := old
	replacement.ID = domain.NewID("ses")
	replacement.TokenHash = Hash([]byte(replacement.ID))
	replacement.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	done := make(chan error, 1)
	go func() { done <- s.RotateSession(ctx, old.TokenHash, replacement) }()
	waitForBlockedQuery(t, s, "SELECT t.id,t.token_hash,t.principal_id")
	for {
		var expired bool
		if e = s.Pool.QueryRow(ctx, "SELECT expires_at<=clock_timestamp() FROM browser_sessions WHERE token_hash=$1", old.TokenHash).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if expired {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	independentIdentityError(t, <-done, oidcidentity.ErrSessionInvalid)
	var count int
	if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM browser_sessions WHERE token_hash=$1", replacement.TokenHash).Scan(&count); e != nil || count != 0 {
		t.Fatal("expired replacement persisted")
	}
}
func TestIndependentBrowserSessionCurrentMappingPrincipalAndRevocation(t *testing.T) {
	for _, kind := range []string{"mapping-inactive", "principal-inactive", "remapped", "revoked"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := testStore(t)
			ctx := context.Background()
			p, _ := identity(t, s, 1000)
			other, _ := identity(t, s, 1000)
			x := independentSession(t, s, p, "mapping-subject", time.Hour)
			switch kind {
			case "mapping-inactive":
				if _, e := s.Pool.Exec(ctx, "UPDATE identity_mappings SET active=false WHERE issuer=$1 AND subject=$2", x.Issuer, x.Subject); e != nil {
					t.Fatal(e)
				}
			case "principal-inactive":
				if _, e := s.Pool.Exec(ctx, "UPDATE principals SET active=false WHERE id=$1", p.ID); e != nil {
					t.Fatal(e)
				}
			case "remapped":
				if _, e := s.Pool.Exec(ctx, "UPDATE identity_mappings SET principal_id=$3 WHERE issuer=$1 AND subject=$2", x.Issuer, x.Subject, other.ID); e != nil {
					t.Fatal(e)
				}
			case "revoked":
				if e := s.RevokeSession(ctx, x.TokenHash); e != nil {
					t.Fatal(e)
				}
			}
			_, e := s.GetSession(ctx, x.TokenHash)
			independentIdentityError(t, e, oidcidentity.ErrSessionInvalid)
			replacement := x
			replacement.ID = domain.NewID("ses")
			replacement.TokenHash = Hash([]byte(replacement.ID))
			independentIdentityError(t, s.RotateSession(ctx, x.TokenHash, replacement), oidcidentity.ErrSessionInvalid)
			if kind != "revoked" {
				independentIdentityError(t, s.CreateSession(ctx, replacement, ""), oidcidentity.ErrSessionInvalid)
			}
		})
	}
}
func TestIndependentBrowserSessionDoesNotRevokeBearerCredential(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, bootstrap := identity(t, s, 1000)
	x := independentSession(t, s, p, "bearer-isolation", time.Hour)
	y := x
	y.ID = domain.NewID("ses")
	y.TokenHash = Hash([]byte(bootstrap.Token))
	if e := s.CreateSession(ctx, y, x.TokenHash); e != nil {
		t.Fatal(e)
	}
	if e := s.RevokeSession(ctx, y.TokenHash); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Authenticate(ctx, bootstrap.Token, p.OrganizationID); e != nil {
		t.Fatal("browser lifecycle revoked service bearer")
	}
	_, e := s.GetSession(ctx, x.TokenHash)
	independentIdentityError(t, e, oidcidentity.ErrSessionInvalid)
}
func TestIndependentBrowserIdentityConcurrentLinkDoesNotReassign(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	one, _ := identity(t, s, 1000)
	two, _ := identity(t, s, 1000)
	issuer := "https://issuer.example.test"
	subject := "same-subject"
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, principal := range []domain.Principal{one, two} {
		go func(p domain.Principal) { <-start; errs <- s.LinkIdentity(ctx, issuer, subject, p.ID) }(principal)
	}
	close(start)
	successes := 0
	for range 2 {
		if e := <-errs; e == nil {
			successes++
		} else {
			code(t, e, "conflict")
		}
	}
	if successes != 1 {
		t.Fatalf("identity reassigned: %d successful links", successes)
	}
	got, e := s.ResolveIdentity(ctx, issuer, subject)
	if e != nil || got != one.ID && got != two.ID {
		t.Fatal("lost mapping winner")
	}
	if e := s.LinkIdentity(ctx, issuer, subject, got); e != nil {
		t.Fatal("same-principal link is not stable")
	}
	var principals int
	if e := s.Pool.QueryRow(ctx, "SELECT count(*) FROM principals").Scan(&principals); e != nil || principals != 2 {
		t.Fatal("link provisioned an unexpected principal")
	}
	if _, e := s.ResolveIdentity(ctx, issuer, "unmapped-email@example.test"); !errors.Is(e, oidcidentity.ErrIdentityUnmapped) {
		t.Fatal("unmapped identity granted account")
	}
}

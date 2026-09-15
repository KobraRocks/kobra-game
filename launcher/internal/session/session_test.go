package session

import (
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/kobraerr"
)

// testClock is a manually advanced clock so TTL, inactivity and the quota
// window can be exercised without sleeping.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock {
	return &testClock{t: time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newTestStore returns a store driven by a fresh test clock. mutate may adjust
// the options (TTLs, capacity); the clock is installed automatically.
func newTestStore(t *testing.T, mutate func(o *Options)) (*Store, *testClock) {
	t.Helper()
	clk := newClock()
	opts := Options{Now: clk.Now}
	if mutate != nil {
		mutate(&opts)
	}
	return NewStore(opts), clk
}

func mustToken(t *testing.T, st *Store) string {
	t.Helper()
	tok, err := st.NewBootstrapToken()
	if err != nil {
		t.Fatalf("NewBootstrapToken: %v", err)
	}
	return tok
}

func mustExchange(t *testing.T, st *Store, token string) *Session {
	t.Helper()
	sess, err := st.Exchange(token, "http://127.0.0.1:8771")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if sess == nil {
		t.Fatal("Exchange returned a nil session with a nil error")
	}
	return sess
}

func exchangeErr(st *Store, token, origin string) error {
	_, err := st.Exchange(token, origin)
	return err
}

func requireCode(t *testing.T, err error, code string) *kobraerr.KobraError {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %s error, got nil", code)
	}
	var ke *kobraerr.KobraError
	if !errors.As(err, &ke) {
		t.Fatalf("want *kobraerr.KobraError, got %T: %v", err, err)
	}
	if ke.Code != code {
		t.Fatalf("want code %q, got %q (%v)", code, ke.Code, err)
	}
	return ke
}

// --- bootstrap token -------------------------------------------------------

func TestBootstrapTokenShape(t *testing.T) {
	st, _ := newTestStore(t, nil)

	tok, err := st.NewBootstrapToken()
	if err != nil {
		t.Fatalf("NewBootstrapToken: %v", err)
	}
	if len(tok) != 43 {
		t.Fatalf("token length = %d, want 43", len(tok))
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("token entropy = %d bytes, want 32", len(raw))
	}

	other, err := st.NewBootstrapToken()
	if err != nil {
		t.Fatalf("NewBootstrapToken: %v", err)
	}
	if tok == other {
		t.Fatal("two bootstrap tokens were identical")
	}
}

func TestBootstrapTokenSingleUse(t *testing.T) {
	st, _ := newTestStore(t, nil)
	tok := mustToken(t, st)

	first := mustExchange(t, st, tok)
	if first == nil {
		t.Fatal("first exchange produced no session")
	}

	_, err := st.Exchange(tok, "http://127.0.0.1:8771")
	requireCode(t, err, kobraerr.CodeNoSession)

	// The failed re-use must not have created another session.
	if got := st.Len(); got != 1 {
		t.Fatalf("Len() = %d after a replayed token, want 1", got)
	}
}

func TestBootstrapTokenTTLExpiry(t *testing.T) {
	st, clk := newTestStore(t, func(o *Options) {
		o.BootstrapTTL = 120 * time.Second
	})

	live := mustToken(t, st)
	stale := mustToken(t, st)

	clk.Advance(119 * time.Second)
	mustExchange(t, st, live) // still inside the TTL

	clk.Advance(2 * time.Second) // stale is now 121s old
	_, err := st.Exchange(stale, "http://127.0.0.1:8771")
	requireCode(t, err, kobraerr.CodeNoSession)
}

func TestBootstrapTokenNegativeTTLExpiresImmediately(t *testing.T) {
	st, _ := newTestStore(t, func(o *Options) {
		o.BootstrapTTL = -time.Second
	})
	tok := mustToken(t, st)
	_, err := st.Exchange(tok, "http://127.0.0.1:8771")
	requireCode(t, err, kobraerr.CodeNoSession)
}

// FR-API-13: unknown, expired and already-used tokens are indistinguishable.
func TestBootstrapFailuresAreIndistinguishable(t *testing.T) {
	st, clk := newTestStore(t, func(o *Options) {
		o.BootstrapTTL = 10 * time.Second
	})

	used := mustToken(t, st)
	mustExchange(t, st, used)
	expired := mustToken(t, st)
	clk.Advance(11 * time.Second)

	unknownErr := func() error { _, err := st.Exchange("not-a-real-token", "http://127.0.0.1:8771"); return err }()
	usedErr := func() error { _, err := st.Exchange(used, "http://127.0.0.1:8771"); return err }()
	expiredErr := func() error { _, err := st.Exchange(expired, "http://127.0.0.1:8771"); return err }()

	keUnknown := requireCode(t, unknownErr, kobraerr.CodeNoSession)
	keUsed := requireCode(t, usedErr, kobraerr.CodeNoSession)
	keExpired := requireCode(t, expiredErr, kobraerr.CodeNoSession)

	if keUnknown.Msg != keUsed.Msg || keUnknown.Msg != keExpired.Msg {
		t.Fatal("no_session messages differ between unknown, used and expired tokens")
	}
	if unknownErr.Error() != usedErr.Error() || unknownErr.Error() != expiredErr.Error() {
		t.Fatalf("no_session errors differ:\n unknown=%q\n used=%q\n expired=%q",
			unknownErr, usedErr, expiredErr)
	}
	if got := string(keUnknown.Envelope()); strings.Contains(got, "expired") || strings.Contains(got, "used") {
		t.Fatalf("client envelope leaks the failure reason: %s", got)
	}
}

// --- session establishment -------------------------------------------------

func TestExchangeMintsIndependentSecrets(t *testing.T) {
	st, clk := newTestStore(t, nil)
	tok := mustToken(t, st)
	sess := mustExchange(t, st, tok)

	for name, v := range map[string]string{"session id": sess.ID, "csrf": sess.CSRF} {
		if len(v) != 43 {
			t.Fatalf("%s length = %d, want 43", name, len(v))
		}
		if _, err := base64.RawURLEncoding.DecodeString(v); err != nil {
			t.Fatalf("%s is not base64url: %v", name, err)
		}
	}
	if sess.ID == sess.CSRF {
		t.Fatal("session id and CSRF token are identical")
	}
	now := clk.Now()
	if !sess.Created.Equal(now) || !sess.LastActive.Equal(now) {
		t.Fatalf("Created/LastActive = %v/%v, want %v", sess.Created, sess.LastActive, now)
	}
	if sess.Writer {
		t.Fatal("a fresh session unexpectedly holds the writer role")
	}
	if _, ok := st.Get(sess.ID); !ok {
		t.Fatal("Get could not find the session under its cookie value")
	}
}

func TestExchangeRejectsUnknownOriginToken(t *testing.T) {
	st, _ := newTestStore(t, nil)
	_, err := st.Exchange("", "")
	requireCode(t, err, kobraerr.CodeNoSession)
}

// --- CSRF ------------------------------------------------------------------

func TestCheckCSRFMatrix(t *testing.T) {
	st, _ := newTestStore(t, nil)
	sess := mustExchange(t, st, mustToken(t, st))
	other := mustExchange(t, st, mustToken(t, st))

	valid := sess.CSRF
	cases := []struct {
		name         string
		cookie       string
		header       string
		sess         *Session
		wantRejected bool
	}{
		{"valid", valid, valid, sess, false},
		{"missing cookie", "", valid, sess, true},
		{"missing header", valid, "", sess, true},
		{"missing both", "", "", sess, true},
		{"cookie/header mismatch", valid, other.CSRF, sess, true},
		{"equal but not the session token", other.CSRF, other.CSRF, sess, true},
		{"header not the session token", valid, valid + "x", sess, true},
		{"nil session", valid, valid, nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.CheckCSRF(tc.cookie, tc.header, tc.sess, "http://127.0.0.1:8771")
			if !tc.wantRejected {
				if err != nil {
					t.Fatalf("CheckCSRF rejected a valid pair: %v", err)
				}
				return
			}
			ke := requireCode(t, err, kobraerr.CodeBadCSRF)
			// The CSRF error must never echo either secret.
			if strings.Contains(ke.Error(), tc.cookie) && tc.cookie != "" {
				t.Fatalf("bad_csrf error contains the cookie token: %v", err)
			}
			if strings.Contains(string(ke.Envelope()), tc.cookie) && tc.cookie != "" {
				t.Fatalf("bad_csrf envelope contains the cookie token: %s", ke.Envelope())
			}
		})
	}
}

// --- writer election -------------------------------------------------------

func TestWriterClaimIdempotentAndExclusive(t *testing.T) {
	st, _ := newTestStore(t, nil)
	first := mustExchange(t, st, mustToken(t, st))
	second := mustExchange(t, st, mustToken(t, st))

	if err := st.ClaimWriter(first); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := st.ClaimWriter(first); err != nil {
		t.Fatalf("repeat claim by the same session must be idempotent: %v", err)
	}
	if !first.Writer {
		t.Fatal("claiming session does not carry Writer=true")
	}
	if id, ok := st.Writer(); !ok || id != first.ID {
		t.Fatalf("Writer() = (%q,%v), want (%q,true)", id, ok, first.ID)
	}

	ke := requireCode(t, st.ClaimWriter(second), kobraerr.CodeConflict)
	if got := ke.Detail["current_writer"]; got != first.ID {
		t.Fatalf("current_writer = %v, want %q", got, first.ID)
	}

	st.ReleaseWriter(first.ID)
	if id, ok := st.Writer(); ok {
		t.Fatalf("Writer() = (%q,true) after release, want unheld", id)
	}
	if first.Writer {
		t.Fatal("released session still carries Writer=true")
	}
	if err := st.ClaimWriter(second); err != nil {
		t.Fatalf("claim after release: %v", err)
	}

	// Releasing a role held by someone else is a no-op.
	st.ReleaseWriter(first.ID)
	if id, _ := st.Writer(); id != second.ID {
		t.Fatalf("stray release cleared the writer: got %q, want %q", id, second.ID)
	}
}

func TestWriterRoleFreedWhenHolderExpires(t *testing.T) {
	st, clk := newTestStore(t, func(o *Options) {
		o.SessionTTL = time.Hour
	})
	holder := mustExchange(t, st, mustToken(t, st))
	other := mustExchange(t, st, mustToken(t, st))
	if err := st.ClaimWriter(holder); err != nil {
		t.Fatalf("claim: %v", err)
	}

	clk.Advance(2 * time.Hour)
	st.Reap()

	if id, ok := st.Writer(); ok {
		t.Fatalf("dead session %q still holds the writer role", id)
	}
	if err := st.ClaimWriter(other); err != nil {
		t.Fatalf("claim after the holder expired: %v", err)
	}
}

// --- capacity --------------------------------------------------------------

func TestSessionCapacityRateLimited(t *testing.T) {
	st, clk := newTestStore(t, func(o *Options) {
		o.MaxSessions = 2
		o.SessionTTL = time.Hour
		o.BootstrapTTL = 8 * time.Hour
	})

	tokens := []string{mustToken(t, st), mustToken(t, st), mustToken(t, st)}
	mustExchange(t, st, tokens[0])
	mustExchange(t, st, tokens[1])

	ke := requireCode(t, func() error {
		_, err := st.Exchange(tokens[2], "http://127.0.0.1:8771")
		return err
	}(), kobraerr.CodeRateLimited)
	if got := ke.Detail["kind"]; got != "sessions" {
		t.Fatalf("rate_limited kind = %v, want sessions", got)
	}
	if ke.RetryAfter != 1 {
		t.Fatalf("RetryAfter = %d, want 1", ke.RetryAfter)
	}
	if got := st.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}

	// The refused token was not consumed, so once the two expired sessions are
	// reaped the same token still establishes a session.
	clk.Advance(2 * time.Hour)
	st.Reap()
	if _, err := st.Exchange(tokens[2], "http://127.0.0.1:8771"); err != nil {
		t.Fatalf("retry of the un-consumed token failed: %v", err)
	}
}

func TestSessionCapacityIgnoresExpiredSessions(t *testing.T) {
	st, clk := newTestStore(t, func(o *Options) {
		o.MaxSessions = 1
		o.SessionTTL = time.Hour
		o.BootstrapTTL = 8 * time.Hour
	})
	mustExchange(t, st, mustToken(t, st))
	next := mustToken(t, st)

	// The single slot is occupied by an expired-but-unreaped session; the new
	// establishment must still succeed.
	clk.Advance(2 * time.Hour)
	if _, err := st.Exchange(next, "http://127.0.0.1:8771"); err != nil {
		t.Fatalf("expired session blocked capacity: %v", err)
	}
	if got := st.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

// --- reaping ---------------------------------------------------------------

func TestReapEvictsInactiveSessions(t *testing.T) {
	st, clk := newTestStore(t, func(o *Options) {
		o.Inactivity = 30 * time.Minute
		o.SessionTTL = 8 * time.Hour
	})
	sess := mustExchange(t, st, mustToken(t, st))

	clk.Advance(29 * time.Minute)
	st.Reap()
	if got := st.Len(); got != 1 {
		t.Fatalf("session reaped before the inactivity timeout: Len() = %d", got)
	}

	clk.Advance(2 * time.Minute)
	st.Reap()
	if got := st.Len(); got != 0 {
		t.Fatalf("Len() = %d after inactivity, want 0", got)
	}
	if _, ok := st.Get(sess.ID); ok {
		t.Fatal("Get returned an inactive session")
	}
}

func TestReapEvictsExpiredSessions(t *testing.T) {
	st, clk := newTestStore(t, func(o *Options) {
		o.SessionTTL = time.Hour
		o.Inactivity = 8 * time.Hour
	})
	sess := mustExchange(t, st, mustToken(t, st))

	clk.Advance(59 * time.Minute)
	st.Reap()
	if got := st.Len(); got != 1 {
		t.Fatalf("session reaped before its TTL: Len() = %d", got)
	}

	clk.Advance(2 * time.Minute)
	st.Reap()
	if got := st.Len(); got != 0 {
		t.Fatalf("Len() = %d after the TTL, want 0", got)
	}
	if _, ok := st.Get(sess.ID); ok {
		t.Fatal("Get returned an expired session")
	}
}

func TestReapDropsExpiredBootstrapTokens(t *testing.T) {
	st, clk := newTestStore(t, func(o *Options) {
		o.BootstrapTTL = time.Minute
	})
	tok := mustToken(t, st)
	clk.Advance(2 * time.Minute)
	st.Reap()

	_, err := st.Exchange(tok, "http://127.0.0.1:8771")
	requireCode(t, err, kobraerr.CodeNoSession)
}

// --- Get -------------------------------------------------------------------

func TestGetRefreshesLastActive(t *testing.T) {
	st, clk := newTestStore(t, func(o *Options) {
		o.Inactivity = 30 * time.Minute
	})
	sess := mustExchange(t, st, mustToken(t, st))

	clk.Advance(10 * time.Minute)
	got, ok := st.Get(sess.ID)
	if !ok {
		t.Fatal("Get lost a live session")
	}
	if !got.LastActive.Equal(clk.Now()) {
		t.Fatalf("LastActive = %v, want %v", got.LastActive, clk.Now())
	}

	// The refresh pushed the inactivity deadline out.
	clk.Advance(20 * time.Minute)
	if _, ok := st.Get(sess.ID); !ok {
		t.Fatal("Get rejected a session refreshed 20m ago")
	}
}

func TestGetUnknownCookie(t *testing.T) {
	st, _ := newTestStore(t, nil)
	if sess, ok := st.Get("nope"); ok || sess != nil {
		t.Fatalf("Get(unknown) = (%v,%v), want (nil,false)", sess, ok)
	}
	if _, ok := st.Get(""); ok {
		t.Fatal("Get(\"\") returned a session")
	}
}

func TestNegativeSessionTTLExpiresImmediately(t *testing.T) {
	st, _ := newTestStore(t, func(o *Options) {
		o.SessionTTL = -time.Second
	})
	sess := mustExchange(t, st, mustToken(t, st))
	if _, ok := st.Get(sess.ID); ok {
		t.Fatal("Get returned a session with a negative TTL")
	}
	if got := st.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
}

// --- Clear / Len -----------------------------------------------------------

func TestClearDropsEverything(t *testing.T) {
	st, _ := newTestStore(t, nil)
	sess := mustExchange(t, st, mustToken(t, st))
	if err := st.ClaimWriter(sess); err != nil {
		t.Fatalf("claim: %v", err)
	}
	pending := mustToken(t, st)

	st.Clear()

	if got := st.Len(); got != 0 {
		t.Fatalf("Len() = %d after Clear, want 0", got)
	}
	if _, ok := st.Get(sess.ID); ok {
		t.Fatal("Get returned a session after Clear")
	}
	if _, ok := st.Writer(); ok {
		t.Fatal("writer role survived Clear")
	}
	_, err := st.Exchange(pending, "http://127.0.0.1:8771")
	requireCode(t, err, kobraerr.CodeNoSession)
}

// --- quotas ----------------------------------------------------------------

func TestQuotaWritesWindow(t *testing.T) {
	st, clk := newTestStore(t, nil)
	sess := mustExchange(t, st, mustToken(t, st))

	for i := 0; i < 3; i++ {
		if err := st.Allow(sess, 10, 3, 1<<30); err != nil {
			t.Fatalf("write %d refused: %v", i+1, err)
		}
	}

	ke := requireCode(t, st.Allow(sess, 10, 3, 1<<30), kobraerr.CodeRateLimited)
	if got := ke.Detail["kind"]; got != "writes" {
		t.Fatalf("kind = %v, want writes", got)
	}
	if ke.RetryAfter < 1 || ke.RetryAfter > 60 {
		t.Fatalf("RetryAfter = %d, want 1..60", ke.RetryAfter)
	}

	// Inside the window the refusal persists.
	clk.Advance(30 * time.Second)
	requireCode(t, st.Allow(sess, 10, 3, 1<<30), kobraerr.CodeRateLimited)

	// Once the window rolls over the budget is fresh.
	clk.Advance(31 * time.Second)
	if err := st.Allow(sess, 10, 3, 1<<30); err != nil {
		t.Fatalf("write refused after the window rolled over: %v", err)
	}
}

func TestQuotaBytesWindow(t *testing.T) {
	st, clk := newTestStore(t, nil)
	sess := mustExchange(t, st, mustToken(t, st))

	if err := st.Allow(sess, 60, 100, 100); err != nil {
		t.Fatalf("first write refused: %v", err)
	}
	ke := requireCode(t, st.Allow(sess, 60, 100, 100), kobraerr.CodeRateLimited)
	if got := ke.Detail["kind"]; got != "bytes" {
		t.Fatalf("kind = %v, want bytes", got)
	}

	// The refused write was not charged: a small write still fits.
	if err := st.Allow(sess, 40, 100, 100); err != nil {
		t.Fatalf("write after a refused one failed: %v", err)
	}

	clk.Advance(61 * time.Second)
	if err := st.Allow(sess, 100, 100, 100); err != nil {
		t.Fatalf("write refused after the window rolled over: %v", err)
	}
}

func TestQuotaCountersArePerSession(t *testing.T) {
	st, _ := newTestStore(t, nil)
	first := mustExchange(t, st, mustToken(t, st))
	second := mustExchange(t, st, mustToken(t, st))

	if err := st.Allow(first, 1024, 1, 1<<30); err != nil {
		t.Fatalf("first session's first write refused: %v", err)
	}
	requireCode(t, st.Allow(first, 1024, 1, 1<<30), kobraerr.CodeRateLimited)

	// A second tab has its own budget (§17.3).
	if err := st.Allow(second, 1024, 1, 1<<30); err != nil {
		t.Fatalf("second session's budget was consumed by the first: %v", err)
	}
}

func TestQuotaNilSession(t *testing.T) {
	st, _ := newTestStore(t, nil)
	requireCode(t, st.Allow(nil, 1, 100, 1<<20), kobraerr.CodeNoSession)
}

// A zero limit means "the §17.3 default", which is what the data-API call sites
// pass when they have no explicit quota configuration.
func TestQuotaZeroLimitsUseSpecDefaults(t *testing.T) {
	st, _ := newTestStore(t, nil)
	sess := mustExchange(t, st, mustToken(t, st))

	for i := 0; i < DefaultWritesPerMinute; i++ {
		if err := st.Allow(sess, 1, 0, 0); err != nil {
			t.Fatalf("write %d refused under the default write quota: %v", i+1, err)
		}
	}
	ke := requireCode(t, st.Allow(sess, 1, 0, 0), kobraerr.CodeRateLimited)
	if got := ke.Detail["kind"]; got != "writes" {
		t.Fatalf("kind = %v, want writes", got)
	}

	// The byte default is enforced independently of the write count.
	other := mustExchange(t, st, mustToken(t, st))
	ke = requireCode(t, st.Allow(other, DefaultBytesPerMinute+1, 0, 0), kobraerr.CodeRateLimited)
	if got := ke.Detail["kind"]; got != "bytes" {
		t.Fatalf("kind = %v, want bytes", got)
	}
}

func TestQuotaNegativeLimitsDisableCounters(t *testing.T) {
	st, _ := newTestStore(t, nil)
	sess := mustExchange(t, st, mustToken(t, st))

	for i := 0; i < 5; i++ {
		if err := st.Allow(sess, DefaultBytesPerMinute*2, -1, -1); err != nil {
			t.Fatalf("write %d refused with both counters disabled: %v", i+1, err)
		}
	}
}

// --- logging ---------------------------------------------------------------

func TestNilLoggerIsSafe(t *testing.T) {
	st, _ := newTestStore(t, func(o *Options) {
		o.BootstrapTTL = time.Minute
		o.Logger = nil
	})
	sess := mustExchange(t, st, mustToken(t, st))
	if err := st.ClaimWriter(sess); err != nil {
		t.Fatalf("claim with a nil logger: %v", err)
	}
	requireCode(t, st.CheckCSRF("", "", sess, "http://127.0.0.1:8771"), kobraerr.CodeBadCSRF)
	requireCode(t, exchangeErr(st, "bogus", ""), kobraerr.CodeNoSession)
	st.Reap()
	// A zero limit selects the §17.3 default rather than refusing everything.
	if err := st.Allow(sess, 1, 0, 0); err != nil {
		t.Fatalf("Allow with default limits: %v", err)
	}
}

// TestDegradedLoggerEmitsEvents exercises the LogDir="" path of diagnostics.Open:
// it is degraded (stderr + ring only) but fully functional, which is what the
// store needs during a sidecar fallback.
func TestDegradedLoggerEmitsEvents(t *testing.T) {
	log, err := diagnostics.Open(diagnostics.Options{LogDir: "", Level: diagnostics.LevelDebug})
	if err != nil {
		// Expected: no log directory was configured. The logger is still usable.
		t.Logf("diagnostics.Open returned the expected degraded error: %v", err)
	}
	if log == nil {
		t.Fatal("diagnostics.Open returned a nil logger")
	}
	defer log.Close()
	if !log.Degraded() {
		t.Fatal("logger with an empty LogDir should report degraded")
	}

	st, _ := newTestStore(t, func(o *Options) {
		o.Logger = log
		o.MaxSessions = 1
	})

	tok := mustToken(t, st)
	sess := mustExchange(t, st, tok)
	if err := st.ClaimWriter(sess); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Full capacity -> session.bootstrap.rejected.
	_, _ = st.Exchange(mustToken(t, st), "http://127.0.0.1:8771")
	// CSRF failure.
	requireCode(t, st.CheckCSRF(sess.CSRF, "wrong", sess, "http://evil.example"), kobraerr.CodeBadCSRF)
	// Quota refusal: the first write fits, the second exceeds 1/min.
	if err := st.Allow(sess, 10, 1, 0); err != nil {
		t.Fatalf("first write refused: %v", err)
	}
	requireCode(t, st.Allow(sess, 10, 1, 0), kobraerr.CodeRateLimited)

	// Tail(0) is the whole retained ring; it is the single ring-rendering path
	// (the former RingLines helper duplicated it).
	lines := strings.Join(log.Tail(0), "\n")
	for _, want := range []string{
		`"evt":"session.bootstrap.issued"`,
		`"evt":"session.established"`,
		`"evt":"session.bootstrap.rejected"`,
		`"evt":"csrf.reject"`,
		`"evt":"quota.exceeded"`,
		`"origin":"http://127.0.0.1:8771"`,
		`"origin":"http://evil.example"`,
		`"kind":"writes"`,
		`"sid":"` + sess.ID[:4] + `"`,
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("log is missing %s\n---\n%s", want, lines)
		}
	}
	// No secret may appear in the log (§22.4).
	for name, secret := range map[string]string{
		"bootstrap token": tok,
		"session cookie":  sess.ID,
		"csrf token":      sess.CSRF,
	} {
		if strings.Contains(lines, secret) {
			t.Fatalf("the %s leaked into the log\n---\n%s", name, lines)
		}
	}
}

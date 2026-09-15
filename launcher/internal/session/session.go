// Package session implements the launcher's session, CSRF, single-writer and
// per-session quota state (Launcher spec §11, §17.2, §17.3).
//
// The store is entirely in-memory: a launcher run is one process, one game and
// one browser tab set, so nothing here is persisted. A single mutex guards every
// map and every Session field; there is no channel-based state.
//
// Two invariants from the spec drive the design:
//
//   - A bootstrap token is single-use and dies with its TTL (§11.1). Unknown,
//     expired and already-used tokens are indistinguishable to the client
//     (FR-API-13), so all three produce the same kobraerr.NoSession message.
//   - No secret ever reaches the log. Event fields are identifiers, counts and
//     timestamps only (§22.4); the CSRF token, the session cookie value and the
//     bootstrap token are never passed to a log event or an error message.
package session

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/kobraerr"
)

// Cookie and header names. These are part of the wire contract (§11.2, §11.4):
// the shell and the tests depend on the exact spelling.
const (
	// CookieSession carries the opaque session id (§11.2). It is HttpOnly.
	CookieSession = "kobra_session"
	// CookieCSRF carries the double-submit token. It is deliberately not
	// HttpOnly so the shell can read it and echo it in HeaderCSRF.
	CookieCSRF = "kobra_csrf"
	// HeaderCSRF is the request header that must echo CookieCSRF.
	HeaderCSRF = "X-Kobra-CSRF"
)

// Defaults from §2.3, §11.3 and §17.3. A zero value in Options selects the
// default; a negative value is honoured as-is so tests can force immediate
// expiry.
const (
	DefaultBootstrapTTL = 120 * time.Second
	DefaultSessionTTL   = 8 * time.Hour
	DefaultInactivity   = 30 * time.Minute
	DefaultMaxSessions  = 16

	// DefaultWritesPerMinute and DefaultBytesPerMinute are the §17.3 quota
	// defaults. A zero limit passed to Allow selects the default; a negative
	// limit disables that counter.
	DefaultWritesPerMinute = 100
	DefaultBytesPerMinute  = 64 << 20 // 64 MiB
)

// secretBytes is the entropy of every opaque value this package mints: 32 bytes
// (256 bits) encoded with base64.RawURLEncoding, which is 43 characters (§11.1).
const secretBytes = 32

// quotaWindow is the rolling window over which per-session counters accumulate
// (§17.3).
const quotaWindow = time.Minute

// Eviction and rejection reasons. These are stable identifiers recorded in the
// session.expired and session.bootstrap.rejected events (Appendix C.3).
const (
	reasonExpired  = "expired"
	reasonInactive = "inactive"
	reasonUnknown  = "unknown"
	reasonUsed     = "used"
	reasonCapacity = "capacity"
)

// errBootstrapRejected is the single internal cause used for every bootstrap
// failure. FR-API-13 requires the client to be unable to tell "unknown" from
// "expired" from "already used"; using one cause keeps even the error string
// identical across the three, so the distinction exists only in the log's
// reason field.
var errBootstrapRejected = errors.New("bootstrap token is not usable")

// errCSRFRejected is the single internal cause for every double-submit failure,
// mirroring the spec's single ErrBadCSRF sentinel (§11.4). It carries no token
// material.
var errCSRFRejected = errors.New("csrf token check failed")

// Session is one established browser session. It is returned by Exchange and
// Get. Its exported fields are written only while the owning Store's mutex is
// held; callers treat the returned pointer as a read-mostly view.
type Session struct {
	ID         string
	CSRF       string
	Created    time.Time
	LastActive time.Time
	Writer     bool

	// Quota counters (§17.3). They reset when the rolling window rolls over.
	windowStart time.Time
	writes      int
	bytes       int64
}

// bootstrapToken is a pending, single-use token from §11.1. A used token is
// kept (not deleted) until its TTL elapses so Reap has one place to clean up
// and the rejection reason can stay accurate.
type bootstrapToken struct {
	created time.Time
	used    bool
}

// Options configures NewStore. All durations are clamped to their §2.3 defaults
// when zero; the config layer is responsible for clamping BootstrapTTL to
// [10s, 600s] before it reaches this package.
type Options struct {
	BootstrapTTL time.Duration // §2.3 default 120s
	SessionTTL   time.Duration // §2.3 default 8h
	Inactivity   time.Duration // §2.3 default 30m
	MaxSessions  int           // §11.3 bound; <=0 means 16
	Logger       *diagnostics.Logger
	Now          func() time.Time // injectable clock; nil means time.Now
}

// Store is the in-memory session table. It is safe for concurrent use.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session // keyed by kobra_session cookie value
	tokens   map[string]*bootstrapToken

	// writerID is the single writer identity of §11.5, independent of the
	// storage lock. Empty means unclaimed.
	writerID string

	bootstrapTTL time.Duration
	sessionTTL   time.Duration
	inactivity   time.Duration
	maxSessions  int

	log *diagnostics.Logger
	now func() time.Time
}

// NewStore returns an empty store. A nil Logger disables event emission; every
// other option falls back to its spec default.
func NewStore(opts Options) *Store {
	if opts.BootstrapTTL == 0 {
		opts.BootstrapTTL = DefaultBootstrapTTL
	}
	if opts.SessionTTL == 0 {
		opts.SessionTTL = DefaultSessionTTL
	}
	if opts.Inactivity == 0 {
		opts.Inactivity = DefaultInactivity
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = DefaultMaxSessions
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{
		sessions:     make(map[string]*Session),
		tokens:       make(map[string]*bootstrapToken),
		bootstrapTTL: opts.BootstrapTTL,
		sessionTTL:   opts.SessionTTL,
		inactivity:   opts.Inactivity,
		maxSessions:  opts.MaxSessions,
		log:          opts.Logger,
		now:          now,
	}
}

// NewBootstrapToken issues a single-use bootstrap token: 256 bits from
// crypto/rand, base64.RawURLEncoding, with the configured TTL (§11.1). An error
// means crypto/rand failed. The token itself is never logged.
func (s *Store) NewBootstrapToken() (string, error) {
	token, err := randomSecret()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.tokens[token] = &bootstrapToken{created: s.now()}
	s.mu.Unlock()

	s.info("session.bootstrap.issued", nil)
	return token, nil
}

// Exchange consumes a bootstrap token and creates a session (§11.2). Unknown,
// expired and already-used tokens all yield an indistinguishable
// kobraerr.NoSession. When the store already holds MaxSessions live sessions
// the establishment is refused with kobraerr.RateLimited("sessions", 1)
// (§11.3); the token is not consumed, so the tab may retry once capacity frees.
//
// The returned Session's ID is the kobra_session cookie value the caller must
// set. On success the session.established event carries the 4-character sid and
// the requester's origin.
func (s *Store) Exchange(token, origin string) (*Session, error) {
	s.mu.Lock()
	now := s.now()

	b, ok := s.tokens[token]
	switch {
	case !ok:
		s.mu.Unlock()
		s.rejectBootstrap(origin, reasonUnknown)
		return nil, kobraerr.NoSession(errBootstrapRejected)
	case b.used:
		s.mu.Unlock()
		s.rejectBootstrap(origin, reasonUsed)
		return nil, kobraerr.NoSession(errBootstrapRejected)
	case now.Sub(b.created) >= s.bootstrapTTL:
		delete(s.tokens, token)
		s.mu.Unlock()
		s.rejectBootstrap(origin, reasonExpired)
		return nil, kobraerr.NoSession(errBootstrapRejected)
	}

	// Capacity counts live sessions only: a session that has already expired
	// or gone inactive must not hold a slot (§11.3).
	evicted := s.evictDeadLocked(now)
	if len(s.sessions) >= s.maxSessions {
		s.mu.Unlock()
		s.logEvictions(evicted)
		s.rejectBootstrap(origin, reasonCapacity)
		return nil, kobraerr.RateLimited("sessions", 1)
	}

	id, err := randomSecret()
	if err != nil {
		s.mu.Unlock()
		s.logEvictions(evicted)
		return nil, kobraerr.IO("The launcher could not create a session.", nil, err)
	}
	csrf, err := randomSecret()
	if err != nil {
		s.mu.Unlock()
		s.logEvictions(evicted)
		return nil, kobraerr.IO("The launcher could not create a session.", nil, err)
	}

	sess := &Session{
		ID:          id,
		CSRF:        csrf,
		Created:     now,
		LastActive:  now,
		windowStart: now,
	}
	b.used = true
	s.sessions[id] = sess
	s.mu.Unlock()

	s.logEvictions(evicted)
	s.info("session.established", map[string]any{
		"sid":    shortID(id),
		"origin": origin,
	})
	return sess, nil
}

// Get returns the live session for a kobra_session cookie value, refreshing
// LastActive. A session that has reached its TTL or its inactivity timeout is
// evicted on the spot (so it cannot be used in the window before the reaper
// runs) and reported as absent.
func (s *Store) Get(cookieValue string) (*Session, bool) {
	s.mu.Lock()
	sess, ok := s.sessions[cookieValue]
	if !ok {
		s.mu.Unlock()
		return nil, false
	}
	now := s.now()
	if reason := s.deadReason(now, sess); reason != "" {
		sid := shortID(sess.ID)
		s.deleteSessionLocked(cookieValue)
		s.mu.Unlock()
		s.info("session.expired", map[string]any{"sid": sid, "reason": reason})
		return nil, false
	}
	sess.LastActive = now
	s.mu.Unlock()
	return sess, true
}

// Reap evicts expired and inactive sessions and expired bootstrap tokens. It is
// called periodically by the session reaper goroutine (§11.3); it is also
// idempotent, so calling it by hand is harmless.
func (s *Store) Reap() {
	s.mu.Lock()
	now := s.now()
	evicted := s.evictDeadLocked(now)
	for token, b := range s.tokens {
		if now.Sub(b.created) >= s.bootstrapTTL {
			delete(s.tokens, token)
		}
	}
	s.mu.Unlock()

	s.logEvictions(evicted)
}

// Len returns the number of live sessions (expired but not yet reaped sessions
// are not counted).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for _, sess := range s.sessions {
		if s.deadReason(now, sess) == "" {
			n++
		}
	}
	return n
}

// Clear drops every session, every pending bootstrap token and the writer role.
// It is called on shutdown (§11.3).
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = make(map[string]*Session)
	s.tokens = make(map[string]*bootstrapToken)
	s.writerID = ""
}

// CheckCSRF implements the §11.4 double-submit check exactly: the kobra_csrf
// cookie must be present, the X-Kobra-CSRF header must be present, and both must
// be constant-time equal to each other and to the session's CSRF value. On any
// failure it returns kobraerr.BadCSRF and logs csrf.reject at WARN with the
// request Origin. Token values are never logged or embedded in the error.
func (s *Store) CheckCSRF(cookieValue, headerValue string, sess *Session, origin string) error {
	ok := sess != nil &&
		cookieValue != "" &&
		headerValue != "" &&
		subtle.ConstantTimeCompare([]byte(cookieValue), []byte(headerValue)) == 1 &&
		subtle.ConstantTimeCompare([]byte(cookieValue), []byte(sess.CSRF)) == 1
	if ok {
		return nil
	}
	s.warn("csrf.reject", map[string]any{"origin": origin})
	return kobraerr.BadCSRF(errCSRFRejected)
}

// ClaimWriter implements the atomic writer claim of §11.5. It succeeds when the
// role is free or already held by sess (idempotent), and otherwise returns
// kobraerr.WriterHeld with the current holder's session id.
func (s *Store) ClaimWriter(sess *Session) error {
	if sess == nil {
		return kobraerr.NoSession(errors.New("no session"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.writerID {
	case "":
		s.writerID = sess.ID
		sess.Writer = true
		return nil
	case sess.ID:
		sess.Writer = true
		return nil
	default:
		return kobraerr.WriterHeld(s.writerID)
	}
}

// Writer returns the current writer's session id and whether the role is held.
func (s *Store) Writer() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerID == "" {
		return "", false
	}
	return s.writerID, true
}

// ReleaseWriter clears the writer role when it is held by id. Releasing a role
// held by someone else is a no-op.
func (s *Store) ReleaseWriter(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerID != id {
		return
	}
	s.writerID = ""
	if sess, ok := s.sessions[id]; ok {
		sess.Writer = false
	}
}

// Allow reports whether a write of n bytes may proceed under the session's
// rolling one-minute window (§17.3). A zero limit selects the §17.3 default
// (100 writes/min, 64 MiB/min) and a negative limit disables that counter; this
// is why the data-API call sites may pass 0 for "the configured default".
//
// On refusal it logs quota.exceeded at WARN with the sid and kind ("writes" or
// "bytes") and returns kobraerr.RateLimited with the seconds until the window
// rolls over. A refused write is not counted.
func (s *Store) Allow(sess *Session, n int64, writesPerMinute int, bytesPerMinute int64) error {
	if sess == nil {
		return kobraerr.NoSession(errors.New("no session"))
	}
	if n < 0 {
		n = 0
	}
	if writesPerMinute == 0 {
		writesPerMinute = DefaultWritesPerMinute
	}
	if bytesPerMinute == 0 {
		bytesPerMinute = DefaultBytesPerMinute
	}

	s.mu.Lock()
	now := s.now()
	if sess.windowStart.IsZero() || now.Sub(sess.windowStart) >= quotaWindow {
		sess.windowStart = now
		sess.writes = 0
		sess.bytes = 0
	}
	elapsed := now.Sub(sess.windowStart)
	if elapsed < 0 {
		elapsed = 0
	}
	retryAfter := int((quotaWindow - elapsed + time.Second - 1) / time.Second)
	if retryAfter < 1 {
		retryAfter = 1
	}
	sid := shortID(sess.ID)

	if writesPerMinute > 0 && sess.writes+1 > writesPerMinute {
		s.mu.Unlock()
		s.warn("quota.exceeded", map[string]any{"sid": sid, "kind": "writes"})
		return kobraerr.RateLimited("writes", retryAfter)
	}
	if bytesPerMinute > 0 && sess.bytes+n > bytesPerMinute {
		s.mu.Unlock()
		s.warn("quota.exceeded", map[string]any{"sid": sid, "kind": "bytes"})
		return kobraerr.RateLimited("bytes", retryAfter)
	}

	sess.writes++
	sess.bytes += n
	s.mu.Unlock()
	return nil
}

// deadReason reports why a session is no longer live, or "" when it is live.
func (s *Store) deadReason(now time.Time, sess *Session) string {
	if now.Sub(sess.Created) >= s.sessionTTL {
		return reasonExpired
	}
	if now.Sub(sess.LastActive) >= s.inactivity {
		return reasonInactive
	}
	return ""
}

// eviction records one session removal so the log line can be written after
// the mutex is released.
type eviction struct {
	sid    string
	reason string
}

// evictDeadLocked removes every dead session and returns what it removed. The
// caller holds s.mu.
func (s *Store) evictDeadLocked(now time.Time) []eviction {
	var out []eviction
	for id, sess := range s.sessions {
		if reason := s.deadReason(now, sess); reason != "" {
			out = append(out, eviction{sid: shortID(sess.ID), reason: reason})
			s.deleteSessionLocked(id)
		}
	}
	return out
}

// deleteSessionLocked removes a session and, if it held the writer role,
// releases that role so a survivor can claim it. The caller holds s.mu.
func (s *Store) deleteSessionLocked(id string) {
	delete(s.sessions, id)
	if s.writerID == id {
		s.writerID = ""
	}
}

func (s *Store) logEvictions(evicted []eviction) {
	for _, e := range evicted {
		s.info("session.expired", map[string]any{"sid": e.sid, "reason": e.reason})
	}
}

func (s *Store) rejectBootstrap(origin, reason string) {
	s.warn("session.bootstrap.rejected", map[string]any{
		"origin": origin,
		"reason": reason,
	})
}

func (s *Store) info(evt string, fields map[string]any) {
	if s.log == nil {
		return
	}
	s.log.Info(evt, fields)
}

func (s *Store) warn(evt string, fields map[string]any) {
	if s.log == nil {
		return
	}
	s.log.Warn(evt, fields)
}

// randomSecret returns 256 bits of crypto/rand as 43 characters of
// base64.RawURLEncoding.
func randomSecret() (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// shortID is the 4-character sid used by the event catalogue (Appendix C.3).
// It is a display label, never a credential: the session id is not secret.
func shortID(id string) string {
	if len(id) <= 4 {
		return id
	}
	return id[:4]
}

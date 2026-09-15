// Package server owns the loopback HTTP server: the listener, the fixed
// middleware order of §10.3, the heartbeat/idle watcher, the drain state
// machine, and the unauthenticated probe/health surface.
//
// It satisfies FR-SRV-1..25. Routing for /api/* is delegated to the dataapi
// package; static serving to the static package.
package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kobragames.local/launcher/internal/apitypes"
	"kobragames.local/launcher/internal/config"
	"kobragames.local/launcher/internal/dataapi"
	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/faultinject"
	"kobragames.local/launcher/internal/kobraerr"
	"kobragames.local/launcher/internal/paths"
	"kobragames.local/launcher/internal/session"
	"kobragames.local/launcher/internal/static"
	"kobragames.local/launcher/internal/storage"
)

// MaxConcurrentRequests bounds in-flight handlers (§23.1). A request beyond the
// bound is rejected with 503 rather than queued.
const MaxConcurrentRequests = 32

// Options configures a Server.
type Options struct {
	Root    paths.Root
	Cfg     config.Config
	Ctx     context.Context
	Storage *storage.Engine
	Log     *diagnostics.Logger
	Port    uint16
	Version string
	Commit  string
	// Release is the game release from the launcher config.
	Release string
	// BrowserInfo is the accepted browser, surfaced in diagnostics.
	BrowserName    string
	BrowserVersion string
	// OnFirstHeartbeat, when set, runs once after the first heartbeat of this
	// process. It is the hook rollback retention needs (Updater spec R15.3):
	// "started successfully" means the launcher reached SERVE *and* the shell
	// connected, so the count must not advance at startup.
	OnFirstHeartbeat func()
	// OnWatchdogTimeout, when set, runs once if the §20.5 connection watchdog
	// fires: the browser opened but no heartbeat ever arrived. It receives the
	// log directory so the caller can surface E22 with the log location, which
	// logging alone does not do.
	OnWatchdogTimeout func(logDir string)
}

// Server is one launcher HTTP server.
type Server struct {
	root    paths.Root
	cfg     config.Config
	port    uint16
	version string
	commit  string
	release string

	log      *diagnostics.Logger
	sessions *session.Store
	storageE *storage.Engine
	api      *dataapi.API

	httpSrv  *http.Server
	listener net.Listener

	lastSeen atomic.Int64 // unix nanos; 0 means no page has connected yet
	started  time.Time
	instance string

	draining atomic.Bool
	inFlight atomic.Int64
	requests atomic.Uint64
	sem      chan struct{}

	shutdownOnce sync.Once
	shutdownCh   chan string

	baseCtx    context.Context
	baseCancel context.CancelFunc

	watchdogOnce sync.Once

	// panicked records that a handler panic was recovered. main turns it into
	// the documented exit code 4 (§2.5, §22.3, FR-SRV-24); Serve itself returns
	// nil for a drain, so the caller needs this to distinguish the two.
	panicked atomic.Bool

	browserName    string
	browserPath    string
	browserVersion string

	// firstHeartbeat guards the OnFirstHeartbeat hook. It is separate from
	// lastSeen because lastSeen is also written by Goodbye.
	firstHeartbeat   atomic.Bool
	onFirstHeartbeat func()

	onWatchdogTimeout func(logDir string)
}

// New builds a server. It does not bind; Bind does.
func New(opts Options) *Server {
	ctx := opts.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	baseCtx, cancel := context.WithCancel(ctx)
	s := &Server{
		root:           opts.Root,
		cfg:            opts.Cfg,
		port:           opts.Port,
		version:        opts.Version,
		commit:         opts.Commit,
		release:        opts.Release,
		log:            opts.Log,
		storageE:       opts.Storage,
		started:        time.Now(),
		instance:       newInstanceID(),
		sem:            make(chan struct{}, MaxConcurrentRequests),
		shutdownCh:     make(chan string, 4),
		baseCtx:        baseCtx,
		baseCancel:     cancel,
		browserName:    opts.BrowserName,
		browserVersion: opts.BrowserVersion,

		onFirstHeartbeat: opts.OnFirstHeartbeat,

		onWatchdogTimeout: opts.OnWatchdogTimeout,
	}
	s.sessions = session.NewStore(session.Options{
		BootstrapTTL: opts.Cfg.Server.BootstrapTTL(),
		SessionTTL:   opts.Cfg.Server.SessionTTL(),
		Inactivity:   30 * time.Minute,
		MaxSessions:  16,
		Logger:       opts.Log,
	})
	return s
}

// newInstanceID builds the opaque per-process id used by the probe and the
// instance lock's health check.
func newInstanceID() string {
	raw := make([]byte, 9)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Bind creates the listening socket. tcp4 is explicit: binding "tcp" would
// accept the IPv6 form, which FR-SRV-3 forbids (§10.1).
func (s *Server) Bind(ln net.Listener) {
	s.listener = ln
}

// Port returns the bound port.
func (s *Server) Port() uint16 { return s.port }

// Origin returns the canonical origin for this server.
func (s *Server) Origin() string { return fmt.Sprintf("http://127.0.0.1:%d", s.port) }

// Instance returns the opaque per-process id.
func (s *Server) Instance() string { return s.instance }

// Sessions exposes the session store.
func (s *Server) Sessions() *session.Store { return s.sessions }

// Storage exposes the storage engine.
func (s *Server) Storage() *storage.Engine { return s.storageE }

// Config exposes the immutable policy values to the request pipeline.
func (s *Server) Config() dataapi.DataConfig {
	return dataapi.DataConfig{
		MaxRequestBytes: s.cfg.DataAPI.MaxRequestBytes,
		KeepRevisions:   s.cfg.DataAPI.KeepRevisions,
		GameID:          s.cfg.GameID,
		CSRFRequired:    s.cfg.Server.CSRFRequired,
		WritesPerMinute: s.cfg.DataAPI.WritesPerMinute,
		BytesPerMinute:  s.cfg.DataAPI.BytesPerMinute,
		SidecarDir:      s.root.SidecarDir,
	}
}

// Log exposes the diagnostics logger.
func (s *Server) Log() *diagnostics.Logger { return s.log }

// Started returns the process start time, for uptime_seconds.
func (s *Server) Started() time.Time { return s.started }

// --- Probe / health surface (§8.2, §9.3) ---------------------------------

// Probe returns the unauthenticated probe body.
func (s *Server) Probe() apitypes.ProbeResponse {
	return apitypes.ProbeResponse{
		App:      apitypes.AppName,
		GameID:   s.cfg.GameID,
		Instance: s.instance,
		Release:  s.release,
		Port:     s.port,
	}
}

// Health returns the liveness body; it carries the same opaque fields as the
// probe, which is what §9.3 rule 3 compares.
func (s *Server) Health() apitypes.ProbeResponse { return s.Probe() }

// Heartbeat records page activity (§10.5).
//
// The first heartbeat of a process is also the moment a release counts as
// "started successfully" for rollback retention: the server is in SERVE and the
// shell has connected (Updater spec R15.2, R15.3). A release that starts and
// then dies before the shell connects must not count, which is exactly the
// failure the two-start rule exists to catch.
func (s *Server) Heartbeat() {
	s.lastSeen.Store(time.Now().UnixNano())
	if s.onFirstHeartbeat != nil && s.firstHeartbeat.CompareAndSwap(false, true) {
		s.onFirstHeartbeat()
	}
}

// Goodbye sets lastSeen far enough in the past that the next watcher tick
// triggers shutdown (§10.5).
func (s *Server) Goodbye() {
	s.lastSeen.Store(time.Now().Add(-2 * s.cfg.Server.IdleTimeout()).UnixNano())
}

// Shutdown requests a drain with a reason.
func (s *Server) Shutdown(reason string) {
	s.shutdownOnce.Do(func() {
		select {
		case s.shutdownCh <- reason:
		default:
		}
	})
}

// State builds the GET /api/state payload (§14.5).
func (s *Server) State() (apitypes.StateInfo, error) {
	return apitypes.StateInfo{
		APIVersion:    1,
		GameID:        s.cfg.GameID,
		Release:       s.release,
		EngineVersion: s.storageE.EngineVersion(),
		SaveVersion:   s.storageE.SaveVersion(),
		DataWritable:  s.storageE.DataWritable(),
		DataDirKind:   s.storageE.DataDirKind(),
		Writer:        s.writerValue(),
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		// FR-SRV-18: the shell's visible-page cadence. config.Parse has already
		// defaulted and clamped it to [1, 60].
		HeartbeatIntervalSeconds: s.cfg.Server.HeartbeatIntervalSeconds,
	}, nil
}

// writerValue renders the writer field: the session id, or JSON null.
func (s *Server) writerValue() any {
	if w := s.storageE.Writer(); w != "" {
		return w
	}
	return nil
}

// Mods returns the mods view.
func (s *Server) Mods() (storage.ModsView, error) { return s.storageE.ReadMods() }

// ModAction applies a mod action.
func (s *Server) ModAction(a storage.ModAction) error { return s.storageE.ModAction(a) }

// Diagnostics builds the §21.4 payload. It deliberately exposes origins and
// identifiers only: never a filesystem path and never the OS username.
func (s *Server) Diagnostics() apitypes.DiagnosticsPayload {
	payload := apitypes.DiagnosticsPayload{
		LauncherVersion:   s.version,
		Release:           s.release,
		EngineVersion:     s.storageE.EngineVersion(),
		Port:              s.port,
		Origin:            s.Origin(),
		OriginHistory:     []string{},
		DataDirKind:       s.storageE.DataDirKind(),
		DataWritable:      s.storageE.DataWritable(),
		AtomicityDegraded: s.storageE.AtomicityDegraded(),
		LogTail:           s.log.Tail(200),
		SidecarKind:       s.root.SidecarKind,
		SidecarFallback:   s.root.SidecarFallbackReason,
	}
	if s.browserName != "" {
		payload.Browser = map[string]string{
			"name":    s.browserName,
			"version": s.browserVersion,
		}
	}
	if view, err := s.storageE.ReadMods(); err == nil {
		for _, m := range view.Available {
			payload.Mods = append(payload.Mods, apitypes.ModDiag{ID: m.ID, Enabled: m.Enabled})
		}
	}
	if list, err := s.storageE.ListSaves(s.baseCtx); err == nil {
		for _, sv := range list.Saves {
			payload.Saves = append(payload.Saves, apitypes.SaveDiag{
				Slot: sv.Slot, Rev: sv.Revision, Modified: sv.Modified,
			})
		}
	}
	return payload
}

// --- Handler construction -------------------------------------------------

// buildHandler assembles the middleware chain of §10.3.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// The data API registers its own routes; it reaches the server through the
	// Service and Host interfaces.
	s.api = dataapi.NewService(s, s, s.log)
	s.api.SetBind("127.0.0.1", s.port)

	staticHandler := static.New(static.Options{
		Root:      s.root,
		Cfg:       s.cfg,
		Log:       s.log,
		ModAssets: s.storageE,
		ReadLock:  s.storageE.ReadLock,
	})
	staticHandler.Register(mux)
	s.api.Register(mux)

	// 1. security headers (outermost: no gate below may answer without them),
	// 2. panic recovery, 3. request id + logging, 4. drain refusal (§10.6),
	// 5. host, 6. origin, 7. concurrency bound, then the mux (which applies the
	// session and CSRF gates inside dataapi).
	//
	// §10.3 numbers these as request-processing steps, but the §13.7 header set
	// is a property of every response — including the 421 and 403 that the host
	// and origin gates return before the rest of the chain runs. Applying it
	// outermost is what makes the comment on securityHeaders true.
	var h http.Handler = mux
	h = s.concurrencyLimit(h)
	h = s.trackInFlight(h)
	h = s.normalisePath(h)
	h = s.originGate(h)
	h = s.hostGate(h)
	h = s.drainGate(h)
	h = s.logging(h)
	h = s.recovery(h)
	h = s.securityHeaders(h)
	return h
}

// normalisePath rejects raw traversal syntax and then cleans the path so that
// net/http's ServeMux never issues its own 307 redirect.
//
// Rejecting (rather than silently cleaning) a path that contains a dot segment
// or a doubled separator is deliberate: §12.6 step 8 requires a malformed
// identifier to fail before it reaches a handler, and a 404 is the honest answer
// for a request that names a path the launcher does not serve. Without this,
// ServeMux's redirect would answer 307 before the static resolver's §13.2
// confinement check ever ran.
func (s *Server) normalisePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Path
		if raw == "" {
			raw = "/"
		}
		if strings.Contains(raw, "\\") || strings.Contains(raw, "//") {
			kobraerr.WriteEnvelope(w, kobraerr.NotFound("path"))
			return
		}
		for _, seg := range strings.Split(raw, "/") {
			if seg == "." || seg == ".." {
				kobraerr.WriteEnvelope(w, kobraerr.NotFound("path"))
				return
			}
		}
		if cleaned := path.Clean(raw); cleaned != raw {
			r.URL.Path = cleaned
			r.URL.RawPath = ""
		}
		next.ServeHTTP(w, r)
	})
}

// Serve runs the HTTP server until the drain completes or the listener fails.
func (s *Server) Serve() error {
	handler := s.buildHandler()
	s.httpSrv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout is deliberately disabled (§10.2): static file serving
		// can exceed any fixed timeout on a slow network share, and every
		// handler writes to a bounded buffer or a known-length file.
		WriteTimeout:   0,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 32 << 10,
		ErrorLog:       newLogAdapter(s.log).logger,
		BaseContext:    func(net.Listener) context.Context { return s.baseCtx },
	}

	s.log.Info("server.start", map[string]any{
		"port":    s.port,
		"origin":  s.Origin(),
		"release": s.release,
	})

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.httpSrv.Serve(s.listener) }()

	// The heartbeat watcher and session reaper live for the SERVE state.
	watcherDone := make(chan struct{})
	go s.watchLoop(watcherDone)

	select {
	case reason := <-s.shutdownCh:
		s.log.Info("server.stop", map[string]any{"reason": reason})
	case err := <-serveErr:
		close(watcherDone)
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}

	err := s.drain()
	close(watcherDone)
	return err
}

// watchdogTimeout is the §20.5 window: if no heartbeat arrives within it, the
// page opened but could not reach the launcher.
const watchdogTimeout = 20 * time.Second

// watchLoop is the single heartbeat/idle goroutine of §10.5 plus the session
// reaper. Both tick once a second; the watchdog fires once (§20.5).
func (s *Server) watchLoop(done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	started := time.Now()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if s.draining.Load() {
				return
			}
			s.sessions.Reap()
			last := s.lastSeen.Load()
			if last == 0 {
				// §20.5 connection watchdog: the page never reached us.
				s.checkWatchdog(started, time.Now())
				continue
			}
			if time.Since(time.Unix(0, last)) > s.cfg.Server.IdleTimeout() {
				s.log.Info("server.stop", map[string]any{"reason": "idle"})
				s.Shutdown("idle")
				return
			}
		}
	}
}

// checkWatchdog applies the §20.5 rule for one tick and reports whether it
// fired. It fires at most once per process. A heartbeat at any point before the
// window expires cancels it permanently, because lastSeen is then non-zero.
//
// The spec requires this to be *surfaced* as E22 with the log location, not
// merely logged: OnWatchdogTimeout is how main turns it into something the user
// can see and act on.
func (s *Server) checkWatchdog(started, now time.Time) bool {
	if s.lastSeen.Load() != 0 || now.Sub(started) <= watchdogTimeout {
		return false
	}
	fired := false
	s.watchdogOnce.Do(func() {
		fired = true
		s.log.Warn("browser.watchdog.timeout", nil)
		if s.onWatchdogTimeout != nil {
			s.onWatchdogTimeout(s.root.LogDir)
		}
	})
	return fired
}

// drain implements §10.6 and the §23.4 shutdown ordering.
func (s *Server) drain() error {
	s.draining.Store(true)
	n := s.inFlight.Load()
	s.log.Info("server.drain.begin", map[string]any{"in_flight": n})
	begin := time.Now()

	// Steps 1-3: the drain. In-flight handlers finish; new requests are
	// already being rejected by the middleware.
	deadline := time.Now().Add(s.cfg.Server.DrainTimeout())
	for s.inFlight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.inFlight.Load() > 0 {
		s.log.Warn("server.drain.timeout", map[string]any{"in_flight": s.inFlight.Load()})
	}

	// Step 2 of §23.3: cancel the base context.
	s.baseCancel()

	// Step 4: close the listener after the drain so a mid-response client is
	// not cut off.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(ctx)
	if s.listener != nil {
		_ = s.listener.Close()
	}

	// Step 5: stop the reaper by clearing sessions.
	s.sessions.Clear()

	s.log.Info("server.drain.complete", map[string]any{
		"ms": time.Since(begin).Milliseconds(),
	})
	return nil
}

// --- Middleware (§10.3 order) ---------------------------------------------

// recovery is step 1: a panic in any handler is logged with a stack trace to a
// crash file and answered with 500 io_error, then the launcher drains and exits
// 4 (§22.3).
func (s *Server) recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()
				name, err := s.log.WriteStackFile(stack)
				s.log.Error("server.panic", map[string]any{
					"stack_file": name,
					"panic":      fmt.Sprintf("%v", rec),
				})
				_ = err
				s.panicked.Store(true)
				kobraerr.WriteEnvelope(w, kobraerr.IO("The launcher hit an internal error.", nil, nil))
				s.Shutdown("panic")
				if repanicOnPanic {
					// A development build must show the developer the real
					// failure. The re-panic goes on a fresh goroutine because
					// net/http recovers a panic raised on the handler's own.
					go func() { panic(fmt.Sprintf("%v\n%s", rec, stack)) }()
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Panicked reports whether a handler panic was recovered during this process's
// lifetime. main maps it to exit code 4 (§2.5).
func (s *Server) Panicked() bool { return s.panicked.Load() }

// drainGate is §10.6: once the drain has begun, a request that starts after it
// is refused with 503 and Connection: close, which the shell treats as the
// session ending. Requests already past this point are in flight and finish
// normally.
//
// It sits inside logging so the refusal is still recorded, and outside the host
// and origin gates so a draining launcher does no further work for anyone.
func (s *Server) drainGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			w.Header().Set("Connection", "close")
			kobraerr.WriteEnvelope(w, kobraerr.Draining())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logging is step 2: a request id plus structured access logging at debug
// level. Tokens are never logged.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// §26.4: every request passes here and recovery wraps this middleware, so
		// an injected panic exercises the real recovery path — 500 io_error, a
		// crash dump, and in a real process exit code 4. A no-op in release
		// builds.
		faultinject.Point(faultinject.ServerHandler)
		id := s.requests.Add(1)
		if s.log.Enabled(diagnostics.LevelDebug) {
			s.log.Debug("http.request", map[string]any{
				"id":     id,
				"method": r.Method,
				"path":   r.URL.Path,
			})
		}
		next.ServeHTTP(w, r)
	})
}

// hostGate is step 3 (§12.1). It runs before anything touches the session store
// or the filesystem, which is what defeats DNS rebinding.
func (s *Server) hostGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != fmt.Sprintf("127.0.0.1:%d", s.port) {
			s.log.Warn("host.reject", map[string]any{"host": r.Host})
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originGate is step 4 (§12.2), applied to every request, not just writes.
func (s *Server) originGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != s.Origin() {
			s.log.Warn("origin.reject", map[string]any{"origin": o})
			kobraerr.WriteEnvelope(w, kobraerr.BadOrigin(o, nil))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders is step 5: every response, including error responses,
// carries the §13.7 header set.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy", s.cfg.CSP())
		next.ServeHTTP(w, r)
	})
}

// concurrencyLimit is the §23.1 counting semaphore. Excess requests are
// rejected rather than queued, because a local client issuing more than 32
// concurrent requests is misbehaving.
func (s *Server) concurrencyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			w.Header().Set("Retry-After", "1")
			kobraerr.WriteEnvelope(w, kobraerr.IO("The launcher is busy.", map[string]any{"reason": "too_many_requests"}, nil))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// trackInFlight increments and decrements the in-flight counter for a handler.
// It is used by the drain to know when it may close the listener.
func (s *Server) trackInFlight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.inFlight.Add(1)
		defer s.inFlight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// logAdapter routes net/http's internal error log into the diagnostics ring
// buffer: nothing goes to stderr in release (§10.2).
type logAdapter struct {
	log    *diagnostics.Logger
	logger *log.Logger
}

func newLogAdapter(l *diagnostics.Logger) *logAdapter {
	a := &logAdapter{log: l}
	a.logger = log.New(a, "", 0)
	return a
}

func (a *logAdapter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		a.log.Warn("http.server.error", map[string]any{"msg": msg})
	}
	return len(p), nil
}

// SetBrowser records the detected browser for the diagnostics payload.
func (s *Server) SetBrowser(path, name string) {
	s.browserName = name
	s.browserPath = path
}

package dataapi

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"kobragames.local/launcher/internal/apitypes"
	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/kobraerr"
	"kobragames.local/launcher/internal/session"
	"kobragames.local/launcher/internal/storage"
)

// API is the registered data-API handler set. It is exported so the HTTP server
// can own the mux and call Register, while every handler stays unexported.
type API struct {
	service
}

// NewService builds the data-API handler set.
func NewService(svc Service, host Host, logger *diagnostics.Logger) *API {
	return &API{service{
		svc:        svc,
		host:       host,
		bindHost:   "127.0.0.1",
		logger:     logger,
		updateUsed: map[string]bool{},
		quota:      svc.Config(),
	}}
}

type service struct {
	svc  Service
	host Host

	bindHost string
	bindPort uint16

	logger *diagnostics.Logger

	updateMu   sync.Mutex
	updateUsed map[string]bool // one POST /api/update/apply per session (§19.2)
	quota      DataConfig
}

// SetBind records the bound port so that Host and Origin validation compare
// against exactly the authority the browser used.
func (s *service) SetBind(host string, port uint16) {
	s.bindHost = host
	s.bindPort = port
}

func (s *service) bindAuthority() string {
	return fmt.Sprintf("%s:%d", s.bindHost, s.bindPort)
}

func (s *service) log() *diagnostics.Logger { return s.logger }

// Config exposes the pipeline's policy values to the handlers in this package.
func (s *service) Config() DataConfig { return s.svc.Config() }

// quotaWrites and quotaBytes are the §17.3 per-session budgets, read from the
// launcher config at request time so a config change is never partially
// applied.
func (s *service) quotaWrites() int  { return s.quota.WritesPerMinute }
func (s *service) quotaBytes() int64 { return s.quota.BytesPerMinute }

// Register installs this package's routes on mux. The caller is responsible for
// the security middleware that wraps them.
func (s *service) Register(mux *http.ServeMux) {
	// Unauthenticated, opaque endpoints.
	mux.HandleFunc("/__kobra/probe", s.handleProbe)
	mux.HandleFunc("/__kobra/health", s.handleHealth)
	mux.HandleFunc("/__kobra/session", s.handleSession)

	// Session-authenticated lifecycle endpoints. These carry no JSON body, so
	// the pipeline's Content-Type gate does not apply.
	mux.HandleFunc("/__kobra/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/__kobra/goodbye", s.handleGoodbye)
	mux.HandleFunc("/__kobra/shutdown", s.handleShutdown)
	mux.HandleFunc("/__kobra/diagnostics", s.handleDiagnostics)

	// Data API (§14.1).
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/data/saves", s.handleSavesList)
	mux.HandleFunc("/api/data/saves/", s.handleSaveRead)
	mux.HandleFunc("/api/data/config", s.handleConfigRead)
	mux.HandleFunc("/api/data/config/mods", s.handleModsRead)
	mux.HandleFunc("/api/export/", s.handleExport)
	mux.HandleFunc("/api/save", s.handleSaveWrite)
	mux.HandleFunc("/api/save/", s.handleSaveDelete)
	mux.HandleFunc("/api/config", s.handleConfigWrite)
	mux.HandleFunc("/api/mod", s.handleModAction)
	mux.HandleFunc("/api/mod/", s.handleModDelete)
	mux.HandleFunc("/api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("/api/update/apply", s.handleUpdateApply)
	mux.HandleFunc("/api/update/cancel", s.handleUpdateCancel)
	mux.HandleFunc("/api/update/progress", s.handleUpdateProgress)
}

// --- Unauthenticated endpoints --------------------------------------------

// handleProbe answers the §8.2 probe. The body is deliberately opaque: an
// unrelated local page learns nothing but the game id.
func (s *service) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		kobraerr.WriteEnvelope(w, kobraerr.MethodNotAllowed("GET"))
		return
	}
	writeJSON(w, http.StatusOK, s.host.Probe())
}

// handleHealth answers the §9.3 liveness check.
func (s *service) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		kobraerr.WriteEnvelope(w, kobraerr.MethodNotAllowed("GET"))
		return
	}
	writeJSON(w, http.StatusOK, s.host.Health())
}

// handleSession exchanges a single-use bootstrap token for a session (§11.2).
//
// Failures are deliberately indistinguishable: unknown, expired and already-used
// all return 401 no_session with the same message (FR-API-13).
func (s *service) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		kobraerr.WriteEnvelope(w, kobraerr.MethodNotAllowed("POST"))
		return
	}
	if !contentTypeOK(r.Header.Get("Content-Type")) {
		kobraerr.WriteEnvelope(w, kobraerr.BadContentType(r.Header.Get("Content-Type")))
		return
	}
	var req SessionRequest
	if err := decodeJSON(r, &req, 4096); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	if len(req.Token) < 32 || len(req.Token) > 256 {
		s.log().Warn("session.bootstrap.rejected", map[string]any{
			"origin": r.Header.Get("Origin"),
			"reason": "malformed",
		})
		kobraerr.WriteEnvelope(w, kobraerr.NoSession(nil))
		return
	}
	sess, err := s.host.Sessions().Exchange(req.Token, r.Header.Get("Origin"))
	if err != nil {
		s.log().Warn("session.bootstrap.rejected", map[string]any{
			"origin": r.Header.Get("Origin"),
			"reason": "rejected",
		})
		kobraerr.WriteEnvelope(w, err)
		return
	}
	setSessionCookies(w, r, sess)
	writeJSON(w, http.StatusOK, SessionResponse{
		SessionID:  sess.ID,
		CSRFToken:  sess.CSRF,
		APIVersion: 1,
	})
}

// setSessionCookies applies the §11.2 cookie policy: the session cookie is
// HttpOnly, the CSRF cookie is not (the shell must read it to echo it back).
// Neither is Secure, because the origin is http://127.0.0.1 and a Secure cookie
// would never be stored.
func setSessionCookies(w http.ResponseWriter, r *http.Request, sess *session.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     session.CookieSession,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     session.CookieCSRF,
		Value:    sess.CSRF,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
	})
}

// --- Session-authenticated lifecycle endpoints ----------------------------

func (s *service) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodPost, false); !ok {
		return
	}
	s.host.Heartbeat()
	w.WriteHeader(http.StatusNoContent)
}

func (s *service) handleGoodbye(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodPost, false); !ok {
		return
	}
	s.host.Goodbye()
	w.WriteHeader(http.StatusNoContent)
}

func (s *service) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodPost, false); !ok {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"stopping": true})
	s.host.Shutdown("api")
}

func (s *service) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodGet, false); !ok {
		return
	}
	payload := s.host.Diagnostics()
	writeJSON(w, http.StatusOK, payload)
}

// --- Data API -------------------------------------------------------------

// handleState is the shell's boot handshake (§14.5).
func (s *service) handleState(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodGet, false); !ok {
		return
	}
	st, err := s.svc.State()
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleSavesList enumerates slots, preferring each file's header over the
// advisory index (§14.6).
func (s *service) handleSavesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodGet, false); !ok {
		return
	}
	list, err := s.svc.Storage().ListSaves(r.Context())
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleSaveRead returns one slot's payload.
func (s *service) handleSaveRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodGet, false); !ok {
		return
	}
	slot, ok := routePath(r.URL.Path, "/api/data/saves/")
	if !ok {
		kobraerr.WriteEnvelope(w, kobraerr.NotFound("save slot"))
		return
	}
	if err := validateIdentifierField("slot", slot); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	payload, err := s.svc.Storage().ReadSave(r.Context(), slot)
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// handleExport returns a slot as a downloadable attachment (FR-API-11).
func (s *service) handleExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodGet, false); !ok {
		return
	}
	slot, ok := routePath(r.URL.Path, "/api/export/")
	if !ok {
		kobraerr.WriteEnvelope(w, kobraerr.NotFound("save slot"))
		return
	}
	if err := validateIdentifierField("slot", slot); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	payload, err := s.svc.Storage().ReadSave(r.Context(), slot)
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", slot+".json"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func (s *service) handleConfigRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodGet, false); !ok {
		return
	}
	raw, err := s.svc.Storage().ReadConfig(r.Context())
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *service) handleModsRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodGet, false); !ok {
		return
	}
	view, err := s.svc.Mods()
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleSaveWrite applies one save write (§14.4).
func (s *service) handleSaveWrite(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.validate(w, r, http.MethodPost, true)
	if !ok {
		return
	}
	var req SaveWriteRequest
	if err := decodeJSON(r, &req, s.Config().MaxRequestBytes); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	if err := validateIdentifierField("slot", req.Slot); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	if len(req.Payload) == 0 {
		kobraerr.WriteEnvelope(w, kobraerr.MalformedField("payload", "The save body had no payload.", nil))
		return
	}
	// §17.3 quota check happens before the write lock and before any
	// filesystem call.
	if err := s.host.Sessions().Allow(sess, int64(len(req.Payload)), s.quotaWrites(), s.quotaBytes()); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	res, err := s.svc.Storage().WriteSave(r.Context(), storage.SaveWrite{
		Slot:        req.Slot,
		IfRevision:  req.IfRevision,
		Claim:       req.Claim,
		Compression: req.Compression,
		Payload:     req.Payload,
		Meta:        req.Meta,
		SessionID:   sess.ID,
	})
	if err != nil {
		kobraerr.WriteEnvelope(w, saveError(err))
		return
	}
	writeJSON(w, http.StatusOK, SaveWriteResponse{
		Slot:     res.Slot,
		Revision: res.Revision,
		Modified: res.Modified.UTC().Format(time.RFC3339),
		Bytes:    res.Bytes,
	})
}

// saveError converts a storage conflict into its wire envelope.
func saveError(err error) error {
	if ce, ok := err.(*storage.ConflictError); ok {
		return ce.AsKobra()
	}
	return err
}

// handleSaveDelete moves a slot to the trash (§14.7).
func (s *service) handleSaveDelete(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.validate(w, r, http.MethodDelete, false)
	if !ok {
		return
	}
	slot, ok := routePath(r.URL.Path, "/api/save/")
	if !ok {
		kobraerr.WriteEnvelope(w, kobraerr.NotFound("save slot"))
		return
	}
	if err := validateIdentifierField("slot", slot); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	_ = sess
	if err := s.svc.Storage().DeleteSave(r.Context(), slot); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": slot})
}

// handleConfigWrite merges top-level keys, preserving everything else (§16.3).
func (s *service) handleConfigWrite(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.validate(w, r, http.MethodPost, true)
	if !ok {
		return
	}
	var req ConfigWriteRequest
	if err := decodeJSON(r, &req, s.Config().MaxRequestBytes); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	if len(req.Merge) == 0 {
		kobraerr.WriteEnvelope(w, kobraerr.MalformedField("merge", "The config write contained no keys.", nil))
		return
	}
	for k := range req.Merge {
		if !allowedConfigKey(k) {
			kobraerr.WriteEnvelope(w, kobraerr.MalformedField("merge", "That settings key is not recognised.", nil))
			return
		}
	}
	if err := s.host.Sessions().Allow(sess, 1024, s.quotaWrites(), s.quotaBytes()); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	res, err := s.svc.Storage().MergeConfig(r.Context(), storage.ConfigMerge{
		IfRevision: req.IfRevision,
		Merge:      req.Merge,
	})
	if err != nil {
		kobraerr.WriteEnvelope(w, saveError(err))
		return
	}
	writeJSON(w, http.StatusOK, RevisionResult{
		Revision: res.Revision,
		Modified: res.Modified.UTC().Format(time.RFC3339),
	})
}

// allowedConfigKeys is the whitelist of top-level keys a page may merge into
// settings.json (§16.3). It is deliberately small: the settings file is
// engine-owned, and an unknown key is a client bug or an injection attempt.
var allowedConfigKeys = map[string]bool{
	"audio":         true,
	"video":         true,
	"controls":      true,
	"gameplay":      true,
	"accessibility": true,
	"locale":        true,
	"language":      true,
	"display":       true,
	"input":         true,
	"network":       true,
	"misc":          true,
}

func allowedConfigKey(k string) bool {
	if allowedConfigKeys[k] {
		return true
	}
	// Dotted keys address a nested group for the same whitelist.
	if i := strings.IndexByte(k, '.'); i > 0 {
		return allowedConfigKeys[k[:i]]
	}
	return false
}

// handleModAction applies a POST /api/mod request.
func (s *service) handleModAction(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.validate(w, r, http.MethodPost, true)
	if !ok {
		return
	}
	var req struct {
		ID           string `json:"id"`
		Action       string `json:"action"`
		ArchiveBytes []byte `json:"archive_bytes,omitempty"`
	}
	if err := decodeJSON(r, &req, s.Config().MaxRequestBytes); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	if err := validateIdentifierField("id", req.ID); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	switch req.Action {
	case "install", "enable", "disable":
	default:
		kobraerr.WriteEnvelope(w, kobraerr.MalformedField("action", "That mod action is not supported.", nil))
		return
	}
	if err := s.host.Sessions().Allow(sess, int64(len(req.ArchiveBytes)), s.quotaWrites(), s.quotaBytes()); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	if err := s.svc.ModAction(storage.ModAction{
		ID:           req.ID,
		Action:       req.Action,
		ArchiveBytes: req.ArchiveBytes,
	}); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	view, _ := s.svc.Mods()
	writeJSON(w, http.StatusOK, view)
}

// handleModDelete removes a mod's files by moving them to the trash.
func (s *service) handleModDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodDelete, false); !ok {
		return
	}
	id, ok := routePath(r.URL.Path, "/api/mod/")
	if !ok {
		kobraerr.WriteEnvelope(w, kobraerr.NotFound("mod"))
		return
	}
	if err := validateIdentifierField("id", id); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	if err := s.svc.Storage().DeleteMod(id); err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleUpdateCheck performs the user-initiated update check (§19.2).
func (s *service) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodGet, false); !ok {
		return
	}
	st, err := s.svc.UpdateCheck(r.Context())
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleUpdateApply records the user's request to update (Updater spec §6.1).
//
// It deliberately does NOT download or swap anything. The trigger writes a
// durable pending marker and answers 202; the next launcher start performs the
// download, verification, extraction and swap before the HTTP server binds
// (decision D1). That is what keeps the swap outside the serving window, and it
// is the contract the endpoint has always advertised.
//
// The one-per-session rate limit of §19.2 is retained as the backstop it was
// always described as, not as the control (Packaging spec §9.4 requirement 9).
func (s *service) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.validate(w, r, http.MethodPost, true)
	if !ok {
		return
	}
	s.updateMu.Lock()
	used := s.updateUsed[sess.ID]
	if !used {
		s.updateUsed[sess.ID] = true
	}
	s.updateMu.Unlock()
	if used {
		kobraerr.WriteEnvelope(w, kobraerr.RateLimited("update", 3600))
		return
	}

	intent, err := s.svc.RecordUpdateIntent(r.Context())
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	if !intent.Recorded {
		detail := intent.Detail
		if detail == "" {
			detail = "already current"
		}
		writeJSON(w, http.StatusOK, apitypes.UpdateApplyResponse{Applied: false, Detail: detail})
		return
	}
	writeJSON(w, http.StatusAccepted, apitypes.UpdateApplyResponse{
		Applied:       false,
		Pending:       true,
		TargetRelease: intent.TargetRelease,
		ArchiveSize:   intent.ArchiveSize,
		Detail:        intent.Detail,
	})
}

// handleUpdateCancel cancels a recorded update request, and asks a running
// apply to stop (Updater spec §12.4).
//
// Under decision D1 the apply normally runs at startup with no session
// attached, so cancellation has two halves: this handler clears a request that
// has not started yet, and writes the cancel flag that a startup apply polls.
func (s *service) handleUpdateCancel(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodPost, true); !ok {
		return
	}
	cancelled, err := s.svc.CancelUpdateIntent()
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	detail := "The update was cancelled."
	if !cancelled {
		detail = "There was no update to cancel."
	}
	writeJSON(w, http.StatusOK, apitypes.UpdateCancelResponse{Cancelled: cancelled, Detail: detail})
}

// handleUpdateProgress reports the recorded progress and outcome of an apply
// (Updater spec §11.3).
//
// It is a read of a small local record, not a stream. The shell may read it in
// response to a user action, and once on the boot that follows an apply to
// report what happened (spec R11.8); it MUST NOT poll it on a timer, because
// Packaging spec §9.4 requirement 1 forbids background checks and unrequested
// update badges.
func (s *service) handleUpdateProgress(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.validate(w, r, http.MethodGet, false); !ok {
		return
	}
	st, err := s.svc.UpdateProgress()
	if err != nil {
		kobraerr.WriteEnvelope(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

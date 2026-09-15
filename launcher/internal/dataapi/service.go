// Package dataapi implements the authenticated /api/* surface and the request
// validation pipeline (Launcher spec §12, §14, §16).
//
// The package depends on the Service and Host interfaces below rather than on
// the HTTP server package, which keeps the dependency graph of §3.2 acyclic
// while leaving the server free to own sessions, middleware and drain state.
package dataapi

import (
	"context"
	"encoding/json"
	"net/http"

	"kobragames.local/launcher/internal/apitypes"
	"kobragames.local/launcher/internal/session"
	"kobragames.local/launcher/internal/storage"
	"kobragames.local/launcher/internal/update"
)

// Service is the launcher surface the authenticated data API needs.
// *server.Server implements it.
type Service interface {
	// Config exposes the immutable policy values the pipeline enforces.
	Config() DataConfig

	// Storage is the data engine.
	Storage() *storage.Engine

	// State returns the fields of GET /api/state (§14.5).
	State() (apitypes.StateInfo, error)

	// Mods returns the mods view for GET /api/data/config/mods.
	Mods() (storage.ModsView, error)

	// ModAction applies a mod action.
	ModAction(storage.ModAction) error

	// UpdateCheck performs the user-initiated update check (§19.2).
	UpdateCheck(ctx context.Context) (apitypes.UpdateStatus, error)

	// RecordUpdateIntent records a requested update so the next launch applies
	// it (Updater spec §6.1). It performs the network work — latest.json and the
	// release manifest — and writes the pending marker, then reports the
	// release and size the user is confirming.
	RecordUpdateIntent(ctx context.Context) (update.IntentResult, error)

	// CancelUpdateIntent drops a recorded update request, and asks a running
	// apply to stop (Updater spec §12.4).
	CancelUpdateIntent() (cancelled bool, err error)

	// UpdateProgress returns the recorded progress and outcome of an apply
	// (Updater spec §11.3).
	UpdateProgress() (apitypes.UpdateProgress, error)
}

// Host is the unauthenticated and lifecycle surface: the probe and health
// endpoints of §8.2/§9.3, the session exchange, and the activity endpoints. It
// is a separate interface so that the probe path cannot reach storage.
type Host interface {
	// Probe returns the body of GET /__kobra/probe.
	Probe() apitypes.ProbeResponse

	// Health returns the body of GET /__kobra/health.
	Health() apitypes.ProbeResponse

	// Sessions is the session store used by the session endpoint and the gates.
	Sessions() *session.Store

	// Heartbeat records page activity (§10.5).
	Heartbeat()

	// Goodbye marks the session as ended (§10.5).
	Goodbye()

	// Shutdown requests a drain.
	Shutdown(reason string)

	// Diagnostics returns the §21.4 payload.
	Diagnostics() apitypes.DiagnosticsPayload
}

// DataConfig is the subset of the launcher config the pipeline needs.
type DataConfig struct {
	MaxRequestBytes int64
	KeepRevisions   int
	GameID          string
	CSRFRequired    bool
	// WritesPerMinute and BytesPerMinute are the §17.3 per-session quotas.
	WritesPerMinute int
	BytesPerMinute  int64
	// SidecarDir is the machine-local state root. The update endpoints need it
	// to read and write the update records of Updater spec §5. It is a path and
	// therefore never serialised into a response (§22.4).
	SidecarDir string
}

// --- Request and response types (§14.2) -----------------------------------

// SaveWriteRequest is the POST /api/save body.
type SaveWriteRequest struct {
	Slot        string            `json:"slot"`
	IfRevision  *int64            `json:"if_revision,omitempty"`
	Claim       bool              `json:"claim,omitempty"`
	Compression string            `json:"compression,omitempty"`
	Payload     json.RawMessage   `json:"payload"`
	Meta        *storage.SaveMeta `json:"meta,omitempty"`
}

// SaveWriteResponse is the POST /api/save response.
type SaveWriteResponse struct {
	Slot     string `json:"slot"`
	Revision int64  `json:"revision"`
	Modified string `json:"modified"`
	Bytes    int64  `json:"bytes"`
}

// ConfigWriteRequest is the POST /api/config body.
type ConfigWriteRequest struct {
	IfRevision *int64                     `json:"if_revision,omitempty"`
	Merge      map[string]json.RawMessage `json:"merge"`
}

// RevisionResult is the POST /api/config response.
type RevisionResult struct {
	Revision int64  `json:"revision"`
	Modified string `json:"modified"`
}

// SessionRequest is the POST /__kobra/session body.
type SessionRequest struct {
	Token string `json:"token"`
}

// SessionResponse is the successful session exchange response (§11.2).
type SessionResponse struct {
	SessionID  string `json:"session_id"`
	CSRFToken  string `json:"csrf_token"`
	APIVersion int    `json:"api_version"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"io_error","message":"The launcher could not encode the response."}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

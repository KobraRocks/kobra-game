// Package apitypes holds the wire shapes shared by the HTTP server and the data
// API: the diagnostics payload, the state payload, and the opaque probe/health
// response.
//
// It exists so that the server package and the data-API package can both name
// these types without importing each other, keeping the dependency graph of
// §3.2 acyclic. The types are exactly the JSON documents of §14.5, §21.4 and
// data-api.schema.json.
package apitypes

// StateInfo is the GET /api/state payload (§14.5). Field order is the order the
// spec documents; the wire format is JSON, so order is cosmetic.
type StateInfo struct {
	APIVersion    int    `json:"api_version"`
	GameID        string `json:"game_id"`
	Release       string `json:"release"`
	EngineVersion string `json:"engine_version"`
	SaveVersion   int    `json:"save_version"`
	DataWritable  bool   `json:"data_writable"`
	DataDirKind   string `json:"data_dir_kind,omitempty"`
	Writer        any    `json:"writer,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds,omitempty"`
	// HeartbeatIntervalSeconds publishes server.heartbeat_interval_seconds
	// (FR-SRV-18). The cadence is the shell's to keep, so without publishing it
	// the configured value could never reach the only component that uses it.
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds,omitempty"`
}

// DiagnosticsPayload is the GET /__kobra/diagnostics payload (§21.4). It must
// contain no filesystem path and no OS username (FR-SRV-9, FR-BETA-2).
type DiagnosticsPayload struct {
	LauncherVersion   string            `json:"launcher_version"`
	Release           string            `json:"release"`
	EngineVersion     string            `json:"engine_version"`
	Port              uint16            `json:"port"`
	Origin            string            `json:"origin"`
	OriginHistory     []string          `json:"origin_history"`
	DataDirKind       string            `json:"data_dir_kind"`
	DataWritable      bool              `json:"data_writable"`
	AtomicityDegraded bool              `json:"atomicity_degraded"`
	Browser           map[string]string `json:"browser,omitempty"`
	Mods              []ModDiag         `json:"mods,omitempty"`
	Saves             []SaveDiag        `json:"saves,omitempty"`
	LogTail           []string          `json:"log_tail"`
	SidecarKind       string            `json:"sidecar_kind,omitempty"`
	SidecarFallback   string            `json:"sidecar_fallback_reason,omitempty"`
}

// ModDiag is one mod entry in the diagnostics payload.
type ModDiag struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// SaveDiag is one save entry in the diagnostics payload.
type SaveDiag struct {
	Slot     string `json:"slot"`
	Rev      int64  `json:"rev"`
	Modified string `json:"modified,omitempty"`
}

// ProbeResponse is the body of the unauthenticated GET /__kobra/probe and
// GET /__kobra/health endpoints. It is deliberately opaque: no filesystem path,
// no OS username, no game folder location (§8.2, FR-SRV-9).
type ProbeResponse struct {
	App      string `json:"app"`
	GameID   string `json:"game_id"`
	Instance string `json:"instance"`
	Release  string `json:"release,omitempty"`
	Port     uint16 `json:"port,omitempty"`
}

// AppName is the fixed value of the probe's app field. It is the string an
// unrelated Kobra launcher looks for when deciding whether a port is "ours".
const AppName = "kobra-launcher"

// UpdateStatus is the GET /api/update/check response (§19.2).
type UpdateStatus struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	NotesURL  string `json:"notes_url,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// UpdateProgress is the GET /api/update/progress response (Updater spec §11.3).
//
// It deliberately carries no URL, no absolute path and no archive hash (spec
// R11.9): the download address of a machine-local install is not something a
// page needs, and the hash is the one value that would let a local page
// recognise the publisher's payload.
type UpdateProgress struct {
	Phase         string `json:"phase"`
	TargetRelease string `json:"target_release,omitempty"`
	FromRelease   string `json:"from_release,omitempty"`
	Bytes         int64  `json:"bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	Percent       int    `json:"percent"`
	Cancellable   bool   `json:"cancellable"`
	// Message is the FS §16 wording for a failure, verbatim where one applies
	// (Packaging spec §9.4 requirement 5).
	Message string `json:"message,omitempty"`
	// Reason is a stable machine code. It is not a path and is safe to publish.
	Reason string `json:"reason,omitempty"`
	// Seq increases monotonically within one apply so a reader can detect a
	// stale record (spec R11.3).
	Seq uint64 `json:"seq,omitempty"`
	// Result is present only when an outcome record is newer than the progress
	// record, which is how the shell reports the update that installed the
	// running session (spec R11.7, R11.8).
	Result *UpdateResult `json:"result,omitempty"`
}

// UpdateResult is the terminal outcome of an apply (Updater spec §11.5).
type UpdateResult struct {
	Outcome        string `json:"outcome"`
	TargetRelease  string `json:"target_release,omitempty"`
	Release        string `json:"installed_release,omitempty"`
	Message        string `json:"message,omitempty"`
	Reason         string `json:"reason,omitempty"`
	RetainedBackup bool   `json:"retained_backup"`
	// RequiredBytes and AvailableBytes accompany an insufficient_space refusal
	// (Updater spec R10.6): the shell needs the numbers to explain it. They are
	// byte counts, so publishing them leaks nothing about the filesystem.
	RequiredBytes  int64  `json:"required_bytes,omitempty"`
	AvailableBytes int64  `json:"available_bytes,omitempty"`
	At             string `json:"at,omitempty"`
}

// UpdateApplyResponse is the POST /api/update/apply response. Under the
// restart-to-apply model the request records intent and the swap happens on the
// next launch, so the body carries what the user needs to see: which release,
// how large, and when it lands (Updater spec R6.4).
type UpdateApplyResponse struct {
	Applied       bool   `json:"applied"`
	Pending       bool   `json:"pending"`
	TargetRelease string `json:"target_release,omitempty"`
	ArchiveSize   int64  `json:"archive_size,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// UpdateCancelResponse is the POST /api/update/cancel response.
type UpdateCancelResponse struct {
	Cancelled bool   `json:"cancelled"`
	Detail    string `json:"detail,omitempty"`
}

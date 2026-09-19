package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kobragames.local/launcher/internal/paths"
)

// PortConfig is the "port" object.
type PortConfig struct {
	Base                uint16 `json:"base"`
	Span                uint16 `json:"span"`
	RequireConfirmation bool   `json:"require_confirmation"`
	AllowOutOfRange     bool   `json:"allow_out_of_range"`
	DenyListFile        string `json:"deny_list_file"`
}

// ServerConfig is the "server" object.
type ServerConfig struct {
	Bind string `json:"bind"`
	// HeartbeatIntervalSeconds is FR-SRV-18: the shell's heartbeat cadence
	// while the page is visible (a hidden page uses six times it). The launcher
	// does not poll on this value — it publishes it through GET /api/state so
	// the shell can.
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	IdleTimeoutSeconds       int    `json:"idle_timeout_seconds"`
	MaxRangeBytes            int64  `json:"max_range_bytes"`
	SessionTTLSeconds        int    `json:"session_ttl_seconds"`
	BootstrapTokenTTLSeconds int    `json:"bootstrap_token_ttl_seconds"`
	ProbePath                string `json:"probe_path"`
	CSP                      string `json:"csp,omitempty"`
	ServeMods                bool   `json:"serve_mods"`
	CSRFRequired             bool   `json:"csrf_required"`
	DrainTimeoutSeconds      int    `json:"drain_timeout_seconds"`
	// CrossOriginEmbedderPolicy is FR-SRV-16's conditional COEP. Empty means
	// "send no COEP header", which is the default and preserves every existing
	// game's behaviour. "require-corp" makes the origin cross-origin isolated,
	// which is what SharedArrayBuffer and WebAssembly threads require;
	// "credentialless" is the weaker alternative. Enabling either means every
	// cross-origin subresource the page uses must opt in with CORP or CORS.
	CrossOriginEmbedderPolicy string `json:"cross_origin_embedder_policy,omitempty"`
	// EntryPath is the document the launcher opens on start (§4 step 14) and the
	// path --print-url prints, so a publisher can open the game or its editor
	// directly. It MUST be one of EntryDocuments: a value the static handler
	// does not serve would leave the key silently inert, which is the failure
	// ValidateEntryPath exists to remove.
	EntryPath string `json:"entry_path,omitempty"`
}

// entryDocuments are the documents the launcher may be told to open. Each must be
// a path the static handler actually serves (launcher/internal/static): the root
// document, its explicit alias, and the editor (§13.1).
var entryDocuments = []string{"/", "/index.html", "/editor"}

// DefaultEntryPath is the entry document when the config does not name one.
const DefaultEntryPath = "/index.html"

// EntryDocuments returns the paths server.entry_path may take.
func EntryDocuments() []string {
	out := make([]string, len(entryDocuments))
	copy(out, entryDocuments)
	return out
}

// ValidateEntryPath rejects an entry_path the launcher would not serve, and
// normalises an empty value to the default.
func ValidateEntryPath(p string) (string, error) {
	if p == "" {
		return DefaultEntryPath, nil
	}
	for _, ok := range entryDocuments {
		if p == ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("server.entry_path must be one of %v", entryDocuments)
}

// CrossOriginEmbedderPolicies are the accepted COEP values. An empty string is
// the default and means "send no header".
var CrossOriginEmbedderPolicies = []string{"require-corp", "credentialless"}

// ValidateCOEP rejects any COEP value the header must not carry.
func ValidateCOEP(v string) error {
	if v == "" {
		return nil
	}
	for _, ok := range CrossOriginEmbedderPolicies {
		if v == ok {
			return nil
		}
	}
	return fmt.Errorf("server.cross_origin_embedder_policy must be empty, require-corp or credentialless")
}

// Isolated reports whether this configuration makes the game origin
// cross-origin isolated, which is the condition SharedArrayBuffer requires.
func (s ServerConfig) Isolated() bool {
	return s.CrossOriginEmbedderPolicy == "require-corp" ||
		s.CrossOriginEmbedderPolicy == "credentialless"
}

func (s ServerConfig) IdleTimeout() time.Duration {
	return time.Duration(s.IdleTimeoutSeconds) * time.Second
}
func (s ServerConfig) DrainTimeout() time.Duration {
	return time.Duration(s.DrainTimeoutSeconds) * time.Second
}
func (s ServerConfig) SessionTTL() time.Duration {
	return time.Duration(s.SessionTTLSeconds) * time.Second
}
func (s ServerConfig) BootstrapTTL() time.Duration {
	return time.Duration(s.BootstrapTokenTTLSeconds) * time.Second
}

// ValidateProbePath rejects a server.probe_path that is not the path the
// launcher actually serves. The endpoint is part of the protocol the shell
// speaks, so it is not the publisher's to move: accepting another value would
// leave the key silently inert, which is exactly the failure this check
// removes. protocolPath is passed in by the caller (port.ProbePath) so this
// package does not depend on the port package.
func (c Config) ValidateProbePath(protocolPath string) error {
	if c.Server.ProbePath == "" || c.Server.ProbePath == protocolPath {
		return nil
	}
	return fmt.Errorf("server.probe_path is %q but the launcher serves %q; the probe endpoint is fixed by the data API",
		c.Server.ProbePath, protocolPath)
}

// MinBrowserVersion is the "min_browser_version" object. The schema declares
// chrome/edge/opera as integers and brave as a string; both are kept verbatim
// and compared by major version in the browser package.
type MinBrowserVersion struct {
	Chrome int    `json:"chrome"`
	Edge   int    `json:"edge"`
	Opera  int    `json:"opera"`
	Brave  string `json:"brave"`
}

// UpdateConfig is the "update" object.
type UpdateConfig struct {
	Channel                      string `json:"channel"`
	PatchBaseURL                 string `json:"patch_base_url,omitempty"`
	UpdateCheckUserInitiatedOnly bool   `json:"update_check_user_initiated_only"`
	RetainPreviousRelease        bool   `json:"retain_previous_release"`
	ProtectDataDir               bool   `json:"protect_data_dir"`
}

// DiagnosticsConfig is the "diagnostics" object.
type DiagnosticsConfig struct {
	LogRetentionFiles     int   `json:"log_retention_files"`
	LogMaxBytes           int64 `json:"log_max_bytes"`
	ExposeFilesystemPaths bool  `json:"expose_filesystem_paths"`
}

// DataAPIConfig is the "data_api" object.
type DataAPIConfig struct {
	MaxRequestBytes      int64 `json:"max_request_bytes"`
	WritesPerMinute      int   `json:"writes_per_minute"`
	BytesPerMinute       int64 `json:"bytes_per_minute"`
	KeepRevisions        int   `json:"keep_revisions"`
	TrashRetentionDays   int   `json:"trash_retention_days"`
	AllowDataDirOverride bool  `json:"allow_data_dir_override"`
}

// Config is the validated launcher configuration.
type Config struct {
	Schema            string            `json:"schema"`
	GameID            string            `json:"game_id"`
	GameName          string            `json:"game_name"`
	Release           string            `json:"release,omitempty"`
	Port              PortConfig        `json:"port"`
	Server            ServerConfig      `json:"server"`
	BrowserPreference []string          `json:"browser_preference"`
	MinBrowserVersion MinBrowserVersion `json:"min_browser_version"`
	MimeTypes         map[string]string `json:"mime_types"`
	Update            UpdateConfig      `json:"update"`
	Diagnostics       DiagnosticsConfig `json:"diagnostics"`
	Locales           []string          `json:"locales"`
	DataAPI           DataAPIConfig     `json:"data_api"`

	// Path is where the config was loaded from. It is used for logging and for
	// resolving relative paths such as port.deny_list_file; it is never
	// serialised into an API response (§22.4).
	Path string `json:"-"`
}

// Default returns the layer-1 configuration: every value the schema declares as
// a default (§7.2). Unmarshalling a config file over this value is what makes
// absent properties take their schema default.
func Default() Config {
	return Config{
		Schema: ExpectedSchema,
		Port: PortConfig{
			Base:                8765,
			Span:                100,
			RequireConfirmation: true,
			AllowOutOfRange:     true,
			DenyListFile:        "port-deny-list.json",
		},
		Server: ServerConfig{
			Bind:                     "127.0.0.1",
			IdleTimeoutSeconds:       90,
			HeartbeatIntervalSeconds: 5,
			MaxRangeBytes:            8388608,
			SessionTTLSeconds:        28800,
			BootstrapTokenTTLSeconds: 120,
			ProbePath:                "/__kobra/probe",
			ServeMods:                true,
			CSRFRequired:             true,
			DrainTimeoutSeconds:      15,
			EntryPath:                DefaultEntryPath,
		},
		BrowserPreference: []string{"chrome", "msedge", "brave", "opera"},
		MinBrowserVersion: MinBrowserVersion{Chrome: 105, Edge: 105, Opera: 91, Brave: "1.45"},
		MimeTypes: map[string]string{
			".wasm":  "application/wasm",
			".js":    "text/javascript",
			".mjs":   "text/javascript",
			".json":  "application/json",
			".html":  "text/html",
			".css":   "text/css",
			".ogg":   "audio/ogg",
			".mp3":   "audio/mpeg",
			".png":   "image/png",
			".webp":  "image/webp",
			".svg":   "image/svg+xml",
			".woff2": "font/woff2",
		},
		Update: UpdateConfig{
			Channel:                      "manual",
			UpdateCheckUserInitiatedOnly: true,
			RetainPreviousRelease:        true,
			ProtectDataDir:               true,
		},
		Diagnostics: DiagnosticsConfig{
			LogRetentionFiles:     5,
			LogMaxBytes:           1 << 20,
			ExposeFilesystemPaths: false,
		},
		Locales: []string{"en"},
		DataAPI: DataAPIConfig{
			MaxRequestBytes:      64 << 20,
			WritesPerMinute:      100,
			BytesPerMinute:       64 << 20,
			KeepRevisions:        8,
			TrashRetentionDays:   30,
			AllowDataDirOverride: true,
		},
	}
}

// Load reads and validates a config file. The returned Config has layer-1
// defaults beneath the file's layer-2 values.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read launcher config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return Config{}, err
	}
	cfg.Path = path
	return cfg, nil
}

// Parse validates raw against the embedded schema and unmarshals it over the
// defaults.
func Parse(raw []byte) (Config, error) {
	cfg := Default()
	if err := Validate(raw); err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode launcher config: %w", err)
	}
	if err := cfg.applyDerived(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func decodeJSONBytes(raw []byte) (any, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// applyDerived implements §7.3's clamps. Values the spec calls "clamped" are
// adjusted; a value the spec says is "rejected, not clamped" is an error.
func (c *Config) applyDerived() error {
	if c.Server.BootstrapTokenTTLSeconds < 10 {
		c.Server.BootstrapTokenTTLSeconds = 10
	}
	if c.Server.BootstrapTokenTTLSeconds > 600 {
		c.Server.BootstrapTokenTTLSeconds = 600
	}
	// "A value below 30 is rejected, not clamped silently."
	if c.Server.IdleTimeoutSeconds < 30 {
		return fmt.Errorf("server.idle_timeout_seconds must be at least 30 seconds")
	}
	if c.Server.IdleTimeoutSeconds > 3600 {
		c.Server.IdleTimeoutSeconds = 3600
	}
	if c.Server.HeartbeatIntervalSeconds < 1 {
		c.Server.HeartbeatIntervalSeconds = 1
	}
	if c.Server.HeartbeatIntervalSeconds > 60 {
		c.Server.HeartbeatIntervalSeconds = 60
	}
	if c.Server.Bind != "127.0.0.1" {
		return fmt.Errorf("server.bind must be 127.0.0.1")
	}
	if !c.Server.CSRFRequired {
		return fmt.Errorf("server.csrf_required must be true")
	}
	entry, err := ValidateEntryPath(c.Server.EntryPath)
	if err != nil {
		return err
	}
	c.Server.EntryPath = entry
	if err := ValidateCOEP(c.Server.CrossOriginEmbedderPolicy); err != nil {
		return err
	}
	if !c.Update.ProtectDataDir {
		return fmt.Errorf("update.protect_data_dir must be true")
	}
	if c.Diagnostics.ExposeFilesystemPaths {
		return fmt.Errorf("diagnostics.expose_filesystem_paths must be false")
	}
	if err := paths.ValidateGameID(c.GameID); err != nil {
		return fmt.Errorf("game_id: %w", err)
	}
	if c.Port.Span == 0 {
		return fmt.Errorf("port.span must be at least 1")
	}
	return nil
}

// DenyListPath resolves port.deny_list_file. A relative path is resolved
// against the game folder's launcher directory, which is where the shared deny
// list ships (FS §6.4).
func (c Config) DenyListPath(gameFolder string) string {
	if c.Port.DenyListFile == "" {
		return ""
	}
	if filepath.IsAbs(c.Port.DenyListFile) {
		return c.Port.DenyListFile
	}
	return filepath.Join(gameFolder, "launcher", c.Port.DenyListFile)
}

// CSP returns the effective Content-Security-Policy: server.csp when set, else
// the built-in strict default (FR-SRV-17, §7.3).
func (c Config) CSP() string {
	if c.Server.CSP != "" {
		return c.Server.CSP
	}
	return DefaultCSP
}

// DefaultCSP is the strict policy of FR-SRV-17. It forbids all network access
// from the page: no connect-src beyond 'self', no remote script, no remote
// frame. The engine is served from the same origin.
const DefaultCSP = "default-src 'none'; " +
	"script-src 'self' 'wasm-unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"media-src 'self' blob:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"worker-src 'self' blob:; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'; " +
	"object-src 'none'"

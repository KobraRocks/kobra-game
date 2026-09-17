// Package static serves the read-only game tree: /, /shell.js, /engine/*,
// /assets/*, /locales/*, /editor/* and, when serve_mods is on,
// /mods/<id>/assets/* (Launcher spec §13).
//
// Static serving never reaches data/saves/, data/config/ or the launcher binary
// itself: a request for /data/saves/slot1.json matches no static prefix and is
// a 404 (§13.1).
package static

import (
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"kobragames.local/launcher/internal/config"
	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/kobraerr"
	"kobragames.local/launcher/internal/paths"
)

// ModAssetProvider resolves a mod's asset path and the enabled set.
type ModAssetProvider interface {
	// ModAssetPath resolves <mods>/<id>/assets/<rest> with confinement.
	ModAssetPath(id, rest string) (string, error)
	// EnabledModIDs returns the currently enabled mod ids.
	EnabledModIDs() []string
	// ReadLock takes the engine's shared data lock and returns its release.
	ReadLock() func()
}

// Options configures a Handler.
type Options struct {
	Root      paths.Root
	Cfg       config.Config
	Log       *diagnostics.Logger
	ModAssets ModAssetProvider
	ReadLock  func() func()
}

// Handler serves the static namespace.
type Handler struct {
	root   paths.Root
	cfg    config.Config
	log    *diagnostics.Logger
	mods   ModAssetProvider
	readLk func() func()

	etagMu sync.Mutex
	etags  map[string]string // path -> ETag, computed once per session (§13.6)
}

// New builds a static handler.
func New(opts Options) *Handler {
	return &Handler{
		root:   opts.Root,
		cfg:    opts.Cfg,
		log:    opts.Log,
		mods:   opts.ModAssets,
		readLk: opts.ReadLock,
		etags:  map[string]string{},
	}
}

// Register installs the static routes on mux and installs the catch-all
// handler. The catch-all answers everything the mux did not match, which is
// what makes an unmatched /data/... path a clean 404 rather than the mux's own
// 404 page.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", h.handleRoot)
	mux.HandleFunc("/index.html", h.handleIndex)
	mux.HandleFunc("/shell.js", h.handleShell)
	mux.HandleFunc("/engine/", h.handleEngine)
	mux.HandleFunc("/assets/", h.handleAssets)
	mux.HandleFunc("/locales/", h.handleLocales)
	mux.HandleFunc("/editor", h.handleEditor)
	mux.HandleFunc("/editor/", h.handleEditor)
	mux.HandleFunc("/mods/", h.handleMods)
}

func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		h.notFound(w, r)
		return
	}
	h.serveGame(w, r, filepath.Join(h.root.GameDir, "index.html"), policyNoCache)
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	h.serveGame(w, r, filepath.Join(h.root.GameDir, "index.html"), policyNoCache)
}

func (h *Handler) handleShell(w http.ResponseWriter, r *http.Request) {
	h.serveGame(w, r, filepath.Join(h.root.GameDir, "shell.js"), policyNoCache)
}

func (h *Handler) handleEngine(w http.ResponseWriter, r *http.Request) {
	full, ok := h.resolveUnder(h.root.GameDir, r.URL.Path)
	if !ok {
		h.notFound(w, r)
		return
	}
	h.serveGame(w, r, full, policyImmutable)
}

// handleEditor serves the editor entry document and its subtree (§13.1).
// "/editor" and "/editor/" serve game/editor/index.html; anything deeper
// resolves under game/editor/ with the same confinement as /engine/ and
// /assets/. A package that ships no editor gets a clean 404, so the route costs
// nothing to a game that does not use it.
//
// The editor is an ordinary static document: it inherits the session cookie of
// whatever established a session on this origin, reads the non-HttpOnly
// kobra_csrf cookie, and is otherwise subject to the same gates as the game.
func (h *Handler) handleEditor(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/editor" || r.URL.Path == "/editor/" {
		h.serveGame(w, r, filepath.Join(h.root.GameDir, "editor", "index.html"), policyNoCache)
		return
	}
	full, ok := h.resolveUnder(h.root.GameDir, r.URL.Path)
	if !ok {
		h.notFound(w, r)
		return
	}
	h.serveGame(w, r, full, policyAssets)
}

func (h *Handler) handleAssets(w http.ResponseWriter, r *http.Request) {
	full, ok := h.resolveUnder(h.root.GameDir, r.URL.Path)
	if !ok {
		h.notFound(w, r)
		return
	}
	// A content-addressed path (/assets/<hash>/...) is immutable; anything
	// else gets the short cache (§13.6).
	policy := policyAssets
	if h.looksContentAddressed(strings.TrimPrefix(r.URL.Path, "/assets/")) {
		policy = policyImmutable
	}
	h.serveGame(w, r, full, policy)
}

func (h *Handler) handleLocales(w http.ResponseWriter, r *http.Request) {
	full, ok := h.resolveUnder(h.root.GameDir, r.URL.Path)
	if !ok {
		h.notFound(w, r)
		return
	}
	h.serveGame(w, r, full, policyNoCache)
}

// handleMods serves /mods/<id>/assets/* only when serve_mods is on and the mod
// is enabled (§13.1).
func (h *Handler) handleMods(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Server.ServeMods {
		h.notFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/mods/")
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		h.notFound(w, r)
		return
	}
	id := rest[:slash]
	sub := rest[slash+1:]
	if !strings.HasPrefix(sub, "assets/") {
		h.notFound(w, r)
		return
	}
	if h.mods == nil || !h.modEnabled(id) {
		h.notFound(w, r)
		return
	}
	release := func() {}
	if h.readLk != nil {
		release = h.readLk()
	}
	defer release()
	full, err := h.mods.ModAssetPath(id, strings.TrimPrefix(sub, "assets/"))
	if err != nil {
		h.notFound(w, r)
		return
	}
	h.serveMod(w, r, full, policyAssets)
}

func (h *Handler) modEnabled(id string) bool {
	for _, e := range h.mods.EnabledModIDs() {
		if e == id {
			return true
		}
	}
	return false
}

// resolveUnder applies the §13.2 resolution rules to urlPath under root.
func (h *Handler) resolveUnder(root, urlPath string) (string, bool) {
	if strings.ContainsRune(urlPath, 0) {
		return "", false
	}
	if isWindowsDeviceName(urlPath) {
		return "", false
	}
	if strings.Contains(urlPath, "\\") {
		return "", false
	}
	cleaned := path.Clean("/" + urlPath)
	full := filepath.Join(root, filepath.FromSlash(cleaned))
	if err := paths.Confine(root, full); err != nil {
		return "", false
	}
	return full, true
}

// looksContentAddressed reports whether the first path segment looks like a
// content hash (16+ hex or base32 characters), which is the §13.6 condition
// for an immutable asset path.
func (h *Handler) looksContentAddressed(rest string) bool {
	seg := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		seg = rest[:i]
	}
	if len(seg) < 16 {
		return false
	}
	for _, c := range seg {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	kobraerr.WriteEnvelope(w, kobraerr.NotFound("file"))
}

// isWindowsDeviceName rejects CON, NUL, COM1..9, LPT1..9, AUX and PRN anywhere
// in the request path (§13.2 step 2). The rule for one segment lives in
// paths.ReservedName so static serving and the storage engine cannot disagree
// about it; this function only walks the segments.
func isWindowsDeviceName(p string) bool {
	for _, seg := range strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") {
		if paths.ReservedName(seg) {
			return true
		}
	}
	return false
}

// rangeSpec is a parsed single-range request (§13.5).
type rangeSpec struct {
	start int64
	end   int64
}

// parseRange parses a single byte range. Multiple ranges are not supported and
// report ok=false with multi=true so the caller can answer 416.
func parseRange(header string, size int64) (rs rangeSpec, ok bool, multi bool, unsatisfiable bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return rs, false, false, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if strings.Contains(spec, ",") {
		return rs, false, true, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return rs, false, false, true
	}
	startStr := strings.TrimSpace(spec[:dash])
	endStr := strings.TrimSpace(spec[dash+1:])
	switch {
	case startStr == "" && endStr == "":
		return rs, false, false, true
	case startStr == "":
		// bytes=-N: the last N bytes.
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return rs, false, false, true
		}
		if n > size {
			n = size
		}
		return rangeSpec{start: size - n, end: size - 1}, true, false, false
	default:
		start, err := strconv.ParseInt(startStr, 10, 64)
		if err != nil || start < 0 || start >= size {
			return rs, false, false, true
		}
		end := size - 1
		if endStr != "" {
			e, err := strconv.ParseInt(endStr, 10, 64)
			if err != nil || e < start {
				return rs, false, false, true
			}
			if e < end {
				end = e
			}
		}
		return rangeSpec{start: start, end: end}, true, false, false
	}
}

func formatContentRange(start, end, size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", start, end, size)
}

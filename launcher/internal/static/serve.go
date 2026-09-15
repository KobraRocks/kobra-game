package static

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kobragames.local/launcher/internal/kobraerr"
	"kobragames.local/launcher/internal/paths"
)

// cachePolicy selects the Cache-Control header for a namespace (§13.6).
type cachePolicy int

const (
	policyNoCache   cachePolicy = iota // index.html, shell.js, locales, manifests
	policyImmutable                    // engine/*, content-addressed assets
	policyAssets                       // other /assets/*, mod assets
)

func (p cachePolicy) header() string {
	switch p {
	case policyImmutable:
		return "public, max-age=31536000, immutable"
	case policyAssets:
		return "public, max-age=3600"
	default:
		return "no-cache"
	}
}

// serveGame serves a file under <GameFolder>/game (§13.1).
func (h *Handler) serveGame(w http.ResponseWriter, r *http.Request, fullPath string, policy cachePolicy) {
	h.serveFile(w, r, h.root.GameDir, fullPath, policy)
}

// serveMod serves a file under <DataDir>/mods (§13.1).
//
// The confinement root is <DataDir>/mods, not the whole data directory: a mod
// provider that returned <data>/saves/slot1.json must be a 404 here, whatever
// the storage layer does. This is defence in depth — static serving must not
// rely on ModAssetPath having confined the path already.
func (h *Handler) serveMod(w http.ResponseWriter, r *http.Request, fullPath string, policy cachePolicy) {
	h.serveFile(w, r, filepath.Join(h.root.DataDir, "mods"), fullPath, policy)
}

// serveFile writes one file with the §13.4-§13.6 semantics: explicit MIME type,
// advertised ranges, single-range support, ETag/Last-Modified validation.
//
// confineRoot is the filesystem root the path was resolved against; it is used
// for the symlink re-check of §13.3.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, confineRoot, fullPath string, policy cachePolicy) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		kobraerr.WriteEnvelope(w, kobraerr.MethodNotAllowed("GET, HEAD"))
		return
	}
	if strings.ContainsRune(r.URL.Path, 0) || isWindowsDeviceName(r.URL.Path) || strings.Contains(r.URL.Path, "\\") {
		h.notFound(w, r)
		return
	}
	info, ok := h.statConfined(fullPath, confineRoot)
	if !ok {
		h.notFound(w, r)
		return
	}

	etag, err := h.etagFor(fullPath, info)
	if err != nil {
		h.notFound(w, r)
		return
	}
	mimeType := h.mimeFor(fullPath)

	header := w.Header()
	header.Set("Cache-Control", policy.header())
	header.Set("Accept-Ranges", "bytes")
	header.Set("ETag", etag)
	header.Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	header.Set("Content-Type", mimeType)
	if mimeType == "application/octet-stream" {
		// §13.4: anything unknown is downloaded, never rendered.
		header.Set("Content-Disposition", "attachment")
	}

	// Conditional requests (FR-SRV-15).
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if since := r.Header.Get("If-Modified-Since"); since != "" && r.Header.Get("If-None-Match") == "" {
		if t, err := http.ParseTime(since); err == nil && !info.ModTime().Truncate(time.Second).After(t) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	f, err := os.Open(fullPath)
	if err != nil {
		h.notFound(w, r)
		return
	}
	defer f.Close()

	size := info.Size()
	// §13.5: range handling, single range only. Multiple ranges are 416
	// because the server does not implement multipart/byteranges.
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		rs, ok, multi, unsatisfiable := parseRange(rangeHeader, size)
		if multi || unsatisfiable || !ok {
			header.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		length := rs.end - rs.start + 1
		if max := h.cfg.Server.MaxRangeBytes; max > 0 && length > max {
			header.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if _, err := f.Seek(rs.start, io.SeekStart); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		header.Set("Content-Range", formatContentRange(rs.start, rs.end, size))
		header.Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.CopyN(w, f, length)
		return
	}

	header.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// etagFor returns a strong ETag computed once per session (§13.6). The hash is
// a SHA-256 prefix of the file's contents plus its size, so a same-size
// modification cannot alias.
func (h *Handler) etagFor(path string, info os.FileInfo) (string, error) {
	h.etagMu.Lock()
	if v, ok := h.etags[path]; ok {
		h.etagMu.Unlock()
		return v, nil
	}
	h.etagMu.Unlock()

	sum := sha256.New()
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	if _, err := io.WriteString(sum, strconv.FormatInt(info.Size(), 10)); err != nil {
		return "", err
	}
	etag := `"` + hex.EncodeToString(sum.Sum(nil))[:32] + `"`

	h.etagMu.Lock()
	if len(h.etags) < 4096 {
		h.etags[path] = etag
	}
	h.etagMu.Unlock()
	return etag, nil
}

// mimeFor looks the type up in launcher.config.json's mime_types map, falling
// back to application/octet-stream (§13.4). .wasm matters most: instantiateStreaming
// refuses any other type.
func (h *Handler) mimeFor(path string) string {
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return "application/octet-stream"
	}
	ext := strings.ToLower(path[dot:])
	if t, ok := h.cfg.MimeTypes[ext]; ok && t != "" {
		return t
	}
	return "application/octet-stream"
}

func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		if p == etag || p == "*" || strings.TrimPrefix(p, "W/") == etag {
			return true
		}
	}
	return false
}

// statConfined stats a path, resolving symlinks and re-checking the target
// against the root (§13.3). A broken symlink or an escape is a 404, not a
// disclosure.
func (h *Handler) statConfined(p, root string) (os.FileInfo, bool) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	realFull, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, false
	}
	if err := paths.Confine(realRoot, realFull); err != nil {
		return nil, false
	}
	info, err := os.Stat(realFull)
	if err != nil || info.IsDir() {
		return nil, false
	}
	return info, true
}

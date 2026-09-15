package static

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kobragames.local/launcher/internal/config"
	"kobragames.local/launcher/internal/paths"
)

type fakeMods struct {
	enabled map[string]bool
	root    string
}

func (f *fakeMods) ModAssetPath(id, rest string) (string, error) {
	return filepath.Join(f.root, id, "assets", filepath.FromSlash(filepath.Clean("/"+rest))), nil
}
func (f *fakeMods) EnabledModIDs() []string {
	out := []string{}
	for id := range f.enabled {
		out = append(out, id)
	}
	return out
}
func (f *fakeMods) ReadLock() func() { return func() {} }

// newStatic builds a handler over a synthetic game tree.
func newStatic(t *testing.T, serveMods bool) (*Handler, string) {
	t.Helper()
	root := t.TempDir()
	game := filepath.Join(root, "TestGame")
	for _, d := range []string{"game/assets", "game/engine", "game/locales", "data/mods/hd/assets", "data/saves"} {
		if err := os.MkdirAll(filepath.Join(game, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"game/index.html":                      "<html>",
		"game/shell.js":                        "// shell",
		"game/engine/core.wasm":                "\x00asm",
		"game/assets/tex.png":                  "png",
		"game/assets/abcdef0123456789/tex.png": "hashed",
		"game/locales/en.json":                 `{"a":"b"}`,
		"data/mods/hd/assets/skin.png":         "mod",
		"data/saves/slot1.json":                `{"secret":true}`,
		"launcher/launcher.config.json":        "{}",
	}
	for rel, body := range files {
		target := filepath.Join(game, filepath.FromSlash(rel))
		// The fixture declares real directories (launcher/, data/mods/hd/)
		// that the loop above does not create, so create each parent.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Server.ServeMods = serveMods
	cfg.MimeTypes = map[string]string{
		".wasm": "application/wasm", ".js": "text/javascript",
		".html": "text/html", ".png": "image/png", ".json": "application/json",
	}
	h := New(Options{
		Root: paths.Root{
			GameFolder: game,
			GameDir:    filepath.Join(game, "game"),
			DataDir:    filepath.Join(game, "data"),
		},
		Cfg: cfg,
		ModAssets: &fakeMods{
			enabled: map[string]bool{"hd": true},
			root:    filepath.Join(game, "data", "mods"),
		},
	})
	return h, game
}

// route builds the mux exactly as the server does, so these tests exercise the
// real route table rather than a single handler.
func route(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

func get(t *testing.T, h *Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, route(h), http.MethodGet, path, headers)
}

func do(t *testing.T, mux *http.ServeMux, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestStaticServesGameFilesWithMIMEAndCachePolicy(t *testing.T) {
	h, _ := newStatic(t, true)

	rec := get(t, h, "/shell.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("shell.js status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/javascript" {
		t.Errorf("shell.js content type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("shell.js cache control = %q, want no-cache", got)
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges not advertised")
	}
	if rec.Header().Get("ETag") == "" {
		t.Errorf("ETag missing")
	}
}

// TestWasmMIMEIsCorrect guards FR-AST-2: the engine refuses any type other than
// application/wasm.
func TestWasmMIMEIsCorrect(t *testing.T) {
	h, _ := newStatic(t, true)
	rec := get(t, h, "/engine/core.wasm", nil)
	if got := rec.Header().Get("Content-Type"); got != "application/wasm" {
		t.Fatalf("wasm content type = %q, want application/wasm", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("engine cache control = %q", got)
	}
}

func TestContentAddressedAssetIsImmutable(t *testing.T) {
	h, _ := newStatic(t, true)
	rec := get(t, h, "/assets/abcdef0123456789/tex.png", nil)
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("content-addressed cache control = %q", got)
	}
	rec = get(t, h, "/assets/tex.png", nil)
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("plain asset cache control = %q", got)
	}
}

func TestUnknownExtensionIsAttachment(t *testing.T) {
	h, game := newStatic(t, true)
	if err := os.WriteFile(filepath.Join(game, "game", "assets", "blob.xyz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, "/assets/blob.xyz", nil)
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("unknown type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != "attachment" {
		t.Errorf("unknown type disposition = %q, want attachment", got)
	}
}

// TestDataAndLauncherAreNeverServed is the §13.1 confinement property.
func TestDataAndLauncherAreNeverServed(t *testing.T) {
	h, _ := newStatic(t, true)
	for _, p := range []string{
		"/data/saves/slot1.json",
		"/launcher/launcher.config.json",
		"/../launcher/launcher.config.json",
		"/assets/../../data/saves/slot1.json",
		"/assets/..%2f..%2fdata/saves/slot1.json",
		"/assets/CON",
	} {
		rec := get(t, h, p, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s returned 200; it must never be served", p)
		}
	}
}

func TestRangeRequests(t *testing.T) {
	h, _ := newStatic(t, true)

	rec := get(t, h, "/shell.js", map[string]string{"Range": "bytes=0-3"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-3/8" {
		t.Errorf("Content-Range = %q", got)
	}
	if rec.Body.String() != "// s" {
		t.Errorf("range body = %q", rec.Body.String())
	}

	// bytes=N- to EOF.
	rec = get(t, h, "/shell.js", map[string]string{"Range": "bytes=3-"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "shell" {
		t.Errorf("open-ended range: %d %q", rec.Code, rec.Body.String())
	}

	// bytes=-N last N bytes.
	rec = get(t, h, "/shell.js", map[string]string{"Range": "bytes=-4"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "hell" {
		t.Errorf("suffix range: %d %q", rec.Code, rec.Body.String())
	}

	// Multiple ranges are not implemented (§13.5).
	rec = get(t, h, "/shell.js", map[string]string{"Range": "bytes=0-1,3-4"})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("multi-range status = %d, want 416", rec.Code)
	}

	// Unsatisfiable range.
	rec = get(t, h, "/shell.js", map[string]string{"Range": "bytes=99-200"})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("unsatisfiable range status = %d, want 416", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */8" {
		t.Errorf("unsatisfiable Content-Range = %q", got)
	}
}

func TestConditionalRequests(t *testing.T) {
	h, _ := newStatic(t, true)
	first := get(t, h, "/shell.js", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	rec := get(t, h, "/shell.js", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match status = %d, want 304", rec.Code)
	}
	lm := first.Header().Get("Last-Modified")
	rec = get(t, h, "/shell.js", map[string]string{"If-Modified-Since": lm})
	if rec.Code != http.StatusNotModified {
		t.Errorf("If-Modified-Since status = %d, want 304", rec.Code)
	}
}

func TestModAssetsRespectServeModsAndEnablement(t *testing.T) {
	h, _ := newStatic(t, true)
	rec := get(t, h, "/mods/hd/assets/skin.png", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("enabled mod asset status = %d, want 200", rec.Code)
	}
	// A mod that is not enabled is not served.
	rec = get(t, h, "/mods/notenabled/assets/skin.png", nil)
	if rec.Code == http.StatusOK {
		t.Errorf("disabled mod asset was served")
	}
	// A non-assets path under a mod is not served.
	rec = get(t, h, "/mods/hd/mod.manifest.json", nil)
	if rec.Code == http.StatusOK {
		t.Errorf("non-asset mod path was served")
	}

	off, _ := newStatic(t, false)
	rec = get(t, off, "/mods/hd/assets/skin.png", nil)
	if rec.Code == http.StatusOK {
		t.Errorf("mod asset served with serve_mods=false")
	}
}

// TestSymlinkEscapeIs404 covers §13.3: a symlink pointing outside the root is a
// 404, not a disclosure.
func TestSymlinkEscapeIs404(t *testing.T) {
	h, game := newStatic(t, true)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(game, "game", "assets", "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	rec := get(t, h, "/assets/link.txt", nil)
	if rec.Code == http.StatusOK {
		t.Errorf("symlink escape was served: %s", rec.Body.String())
	}
}

func TestNulByteInPathIsRejected(t *testing.T) {
	h, _ := newStatic(t, true)
	// net/url refuses a NUL in the request target before it reaches a handler,
	// but the handler's own guard (§13.2 step 1) must also reject it if a
	// future caller reaches it directly.
	req := httptest.NewRequest(http.MethodGet, "/assets/tex.png", nil)
	req.URL.Path = "/assets/tex.png\x00.txt"
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("a path containing NUL was served")
	}
}

func TestMethodNotAllowedOnStatic(t *testing.T) {
	h, _ := newStatic(t, true)
	rec := do(t, route(h), http.MethodPost, "/shell.js", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /shell.js status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") == "" {
		t.Errorf("405 response lacks an Allow header")
	}
}

func TestWindowsDeviceNamesRejected(t *testing.T) {
	for _, p := range []string{
		"/CON", "/a/NUL", "/assets/COM1.txt", "/assets/LPT9", "/aux",
		"/assets/con.txt", "/assets/nul.json", "/assets/aux.bak", "/assets/prn.dat", "/assets/con.",
	} {
		if !isWindowsDeviceName(p) {
			t.Errorf("isWindowsDeviceName(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/console", "/assets/COM0", "/COM10", "/assets/tex.png", "/assets/lpt0", "/assets/LPT10"} {
		if isWindowsDeviceName(p) {
			t.Errorf("isWindowsDeviceName(%q) = true, want false", p)
		}
	}
}

// TestReservedDeviceNamesAreNotServed is the static half of the Windows
// silent-save-loss class: the file exists on disk, so only the reserved-name
// guard stops con.txt / nul.json / ... from being served (and, on Windows, from
// opening a device).
func TestReservedDeviceNamesAreNotServed(t *testing.T) {
	h, game := newStatic(t, true)
	for _, name := range []string{"con.txt", "nul.json", "aux.bak", "prn.dat"} {
		if err := os.WriteFile(filepath.Join(game, "game", "assets", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := get(t, h, "/assets/"+name, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("GET /assets/%s returned 200; a reserved device name must never be served", name)
		}
	}
}

// fixedMods is a ModAssetProvider that returns one pre-resolved path, so a test
// can hand the static layer a path that its own confinement must reject.
type fixedMods struct {
	enabled []string
	path    string
}

func (f *fixedMods) ModAssetPath(id, rest string) (string, error) { return f.path, nil }
func (f *fixedMods) EnabledModIDs() []string                      { return f.enabled }
func (f *fixedMods) ReadLock() func()                             { return func() {} }

// TestModAssetsConfinedToModsDir is the §13.1 defence-in-depth property: the
// static layer confines mod assets to <DataDir>/mods itself, so a provider that
// resolves a path elsewhere in the data directory (for example a save file)
// produces a 404 rather than a disclosure. The real storage provider blocks
// this today; this layer must not depend on that.
func TestModAssetsConfinedToModsDir(t *testing.T) {
	h, game := newStatic(t, true)
	save := filepath.Join(game, "data", "saves", "slot1.json")
	if _, err := os.Stat(save); err != nil {
		t.Fatalf("fixture save is missing: %v", err)
	}
	h.mods = &fixedMods{enabled: []string{"hd"}, path: save}

	rec := get(t, h, "/mods/hd/assets/anything.png", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("mod provider returning <data>/saves: status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("save bytes were served through /mods/: %q", rec.Body.String())
	}
}

// TestModAssetInsideModsDirIsServed is the positive half of the same property:
// a genuine <DataDir>/mods/<id>/assets file still serves.
func TestModAssetInsideModsDirIsServed(t *testing.T) {
	h, game := newStatic(t, true)
	h.mods = &fixedMods{
		enabled: []string{"hd"},
		path:    filepath.Join(game, "data", "mods", "hd", "assets", "skin.png"),
	}
	rec := get(t, h, "/mods/hd/assets/skin.png", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("real mod asset status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "mod" {
		t.Fatalf("mod asset body = %q, want %q", rec.Body.String(), "mod")
	}
}

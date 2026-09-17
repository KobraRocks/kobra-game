package static

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"kobragames.local/launcher/internal/config"
	"kobragames.local/launcher/internal/paths"
)

// newEditorTree builds a game tree with or without an editor, so both the
// "ships an editor" and the "ships no editor" halves of FR-AST-16 are covered by
// the real route table.
func newEditorTree(t *testing.T, withEditor bool) *Handler {
	t.Helper()
	root := t.TempDir()
	game := filepath.Join(root, "TestGame")
	for _, d := range []string{"game/assets", "game/engine", "data/saves"} {
		if err := os.MkdirAll(filepath.Join(game, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"game/index.html":       "<html>game</html>",
		"game/shell.js":         "// shell",
		"game/assets/tex.png":   "png",
		"data/saves/slot1.json": `{"secret":true}`,
	}
	if withEditor {
		files["game/editor/index.html"] = "<html>editor</html>"
		files["game/editor/editor.js"] = "// editor"
		files["game/editor/modes/powers.js"] = "// powers"
	}
	for rel, body := range files {
		target := filepath.Join(game, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New(Options{
		Root: paths.Root{
			GameFolder: game,
			GameDir:    filepath.Join(game, "game"),
			DataDir:    filepath.Join(game, "data"),
		},
		Cfg: config.Default(),
	})
}

func TestEditorRouteServesDocumentAtBothSpellings(t *testing.T) {
	h := newEditorTree(t, true)
	for _, path := range []string{"/editor", "/editor/"} {
		rec := get(t, h, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if got := rec.Body.String(); got != "<html>editor</html>" {
			t.Errorf("GET %s body = %q", path, got)
		}
	}
}

func TestEditorRouteServesSubtree(t *testing.T) {
	h := newEditorTree(t, true)
	for path, want := range map[string]string{
		"/editor/editor.js":       "// editor",
		"/editor/modes/powers.js": "// powers",
	} {
		rec := get(t, h, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if rec.Body.String() != want {
			t.Errorf("GET %s body = %q, want %q", path, rec.Body.String(), want)
		}
	}
}

// TestEditorRouteCannotEscapeItsSubtree is the confinement test: the editor
// route must not become a way to read game/, data/ or the launcher binary.
func TestEditorRouteCannotEscapeItsSubtree(t *testing.T) {
	h := newEditorTree(t, true)
	cases := []string{
		"/editor/../index.html",
		"/editor/../shell.js",
		"/editor/../../data/saves/slot1.json",
		"/editor/..%2f..%2fdata%2fsaves%2fslot1.json",
		"/editor/%2e%2e/%2e%2e/data/saves/slot1.json",
		"/editor/./../index.html",
	}
	for _, path := range cases {
		rec := get(t, h, path, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200; the editor route escaped its subtree (body %q)", path, rec.Body.String())
		}
	}
}

// TestEditorRouteAbsentIs404 pins the other half of FR-AST-16: a game that ships
// no editor must serve a clean 404, not fail to route.
func TestEditorRouteAbsentIs404(t *testing.T) {
	h := newEditorTree(t, false)
	for _, path := range []string{"/editor", "/editor/", "/editor/editor.js"} {
		rec := get(t, h, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with no editor shipped = %d, want 404", path, rec.Code)
		}
	}
}

// TestEditorFilesAreNotReachableUnderAssets guards the route allowlist itself:
// /assets/* and /editor/* are separate roots, so the editor cannot be smuggled
// in through the asset overlay and vice versa.
func TestEditorFilesAreNotReachableUnderAssets(t *testing.T) {
	h := newEditorTree(t, true)
	if rec := get(t, h, "/assets/editor/index.html", nil); rec.Code == http.StatusOK {
		t.Error("/assets/editor/index.html served; the editor root must be reached only at /editor")
	}
}

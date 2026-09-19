package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageGame writes a minimal game/ tree and returns the staging root.
func stageGame(t *testing.T, files map[string]string) string {
	t.Helper()
	stage := t.TempDir()
	for rel, body := range files {
		target := filepath.Join(stage, "TestGame", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return stage
}

// TestEditorTreeIsServable is the packaging half of FR-AST-16: an editor the
// game ships must be packaged and indexed rather than packaged-and-404'd.
func TestEditorTreeIsServable(t *testing.T) {
	stage := stageGame(t, map[string]string{
		"game/index.html":             "<html>",
		"game/shell.js":               "// shell",
		"game/editor/index.html":      "<html>editor</html>",
		"game/editor/editor.js":       "// editor",
		"game/editor/lib/zstd.wasm":   "\x00asm",
		"game/editor/modes/powers.js": "// powers",
	})
	if err := CheckServable(stage, "TestGame"); err != nil {
		t.Fatalf("an editor tree was rejected: %v", err)
	}
}

// TestEditorTreeIsOptional pins the other half: a game with no editor is still
// shippable, so the route costs nothing to a game that does not use it.
func TestEditorTreeIsOptional(t *testing.T) {
	stage := stageGame(t, map[string]string{
		"game/index.html":     "<html>",
		"game/shell.js":       "// shell",
		"game/assets/tex.png": "png",
	})
	if err := CheckServable(stage, "TestGame"); err != nil {
		t.Fatalf("a game without an editor was rejected: %v", err)
	}
}

// TestUnservedRootFileIsStillRejected guards the rule the editor route did not
// relax: the game root serves exactly index.html and shell.js.
func TestUnservedRootFileIsStillRejected(t *testing.T) {
	stage := stageGame(t, map[string]string{
		"game/index.html":  "<html>",
		"game/shell.js":    "// shell",
		"game/editor.html": "<html>editor</html>",
	})
	err := CheckServable(stage, "TestGame")
	if err == nil {
		t.Fatal("game/editor.html was accepted; the root must serve only index.html and shell.js")
	}
	if !strings.Contains(err.Error(), "editor.html") {
		t.Errorf("error does not name the offending file: %v", err)
	}
}

// TestUnservedSubtreeIsStillRejected guards the prefix allowlist against being
// widened by accident.
func TestUnservedSubtreeIsStillRejected(t *testing.T) {
	stage := stageGame(t, map[string]string{
		"game/index.html":  "<html>",
		"game/shell.js":    "// shell",
		"game/src/main.rs": "fn main() {}",
	})
	if err := CheckServable(stage, "TestGame"); err == nil {
		t.Fatal("game/src/main.rs was accepted; it matches no served prefix")
	}
}

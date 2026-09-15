package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// gameIDRe is the game_id pattern from launcher.config.schema.json. It is
// duplicated here, rather than imported from config, so that the sidecar
// location can be derived without importing the whole config package.
var gameIDRe = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)

// ValidateGameID reports whether id is a valid reverse-DNS game id. A game id
// becomes a directory name (§6.1), so it is validated before first use.
func ValidateGameID(id string) error {
	if !gameIDRe.MatchString(id) {
		return errors.New("game id must be reverse-DNS lowercase, for example com.kobra.stardrifter")
	}
	return nil
}

// GameFolderFromExecutable locates the game folder from the executable path
// alone, using the same one-level-upward search as Resolve but WITHOUT
// requiring game/index.html to exist.
//
// It exists for one caller: the §19.4 update-recovery pass must run before the
// strict layout check, because an interrupted swap can legitimately leave game/
// missing, and Resolve would reject exactly that state. Every other caller uses
// Resolve, which enforces the full FR-LNCH-1 contract.
func GameFolderFromExecutable(execPath string) (string, error) {
	if execPath == "" {
		return "", ErrExecutable
	}
	abs, err := filepath.Abs(execPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrExecutable, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	root := filepath.Dir(filepath.Dir(abs))
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(ConfigRelPath))); err != nil {
		if parent := filepath.Dir(root); parent != root {
			if _, perr := os.Stat(filepath.Join(parent, filepath.FromSlash(ConfigRelPath))); perr == nil {
				return parent, nil
			}
		}
		return "", ErrConfigMissing
	}
	return root, nil
}

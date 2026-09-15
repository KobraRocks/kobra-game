package pack

import (
	"fmt"
	"os"
	"path/filepath"
)

// DevShim describes the throwaway game folder `kobra-pack dev` assembles.
type DevShim struct {
	Dir        string // the shim root, which is the launcher's game folder
	GameTarget string // the source tree the shim's game/ links to
	Launcher   string // the launcher binary to run
	DataDir    string // --data-dir override, if one was requested
}

// DevShimDirName is the shim directory, created beside pkg.toml.
const DevShimDirName = ".kobra-dev"

// DevShim assembles a game folder whose game/ is a symlink to the source tree.
//
// The launcher derives its game folder exclusively from its own executable path
// and deliberately has no --game-dir in a release build (FR-LNCH-1), so this is
// how a developer points it at a tree they are editing:
//
//	<configdir>/.kobra-dev/
//	├── launcher/            the real binary, this game's rendered config,
//	│                        the shared deny list, and VERSION
//	├── game -> <pkgroot>/game
//	└── data/                a real directory, so saves never enter the source
//
// The launcher follows symlinks when serving and re-checks confinement
// (§13.3), so edits are visible on the next request and a link to /etc/passwd
// is still a 404.
func (p *Pipeline) DevShim(opts Options, dataDir string) (*DevShim, error) {
	prep, err := p.prepare(opts)
	if err != nil {
		return nil, err
	}

	shimDir := filepath.Join(p.cfg.Dir, DevShimDirName)
	if err := os.RemoveAll(shimDir); err != nil {
		return nil, err
	}
	launcherDir := filepath.Join(shimDir, "launcher")
	if err := os.MkdirAll(launcherDir, 0o755); err != nil {
		return nil, err
	}

	binaryTarget := filepath.Join(launcherDir, prep.platform.Binary)
	if err := copyFile(prep.launcherBinary, binaryTarget); err != nil {
		return nil, Fail(EvStart, "the launcher binary could not be staged for dev",
			map[string]any{"reason": "copy"})
	}
	if err := os.Chmod(binaryTarget, 0o755); err != nil {
		return nil, err
	}
	if err := copyFile(prep.denyList, filepath.Join(launcherDir, "port-deny-list.json")); err != nil {
		return nil, Fail(EvStart, "the port deny list could not be staged for dev",
			map[string]any{"reason": "copy"})
	}
	if err := os.WriteFile(filepath.Join(launcherDir, "VERSION"), []byte(prep.launcherVersion+"\n"), 0o644); err != nil {
		return nil, err
	}

	// The release the developer is working on: the newest declared one.
	newest := prep.releases[len(prep.releases)-1]
	launcherCfg := make(map[string]any, len(prep.baseConfig))
	for k, v := range prep.baseConfig {
		launcherCfg[k] = v
	}
	launcherCfg["release"] = newest.Release
	if err := WriteJSON(filepath.Join(launcherDir, "launcher.config.json"), launcherCfg); err != nil {
		return nil, err
	}

	gameTarget := filepath.Join(prep.pkgRoot, "game")
	if st, err := os.Stat(gameTarget); err != nil || !st.IsDir() {
		return nil, Fail(EvStart, "pkgroot has no game/ directory to serve",
			map[string]any{"reason": "no_game"})
	}
	if err := os.Symlink(gameTarget, filepath.Join(shimDir, "game")); err != nil {
		return nil, Fail(EvStart, "the shim's game/ symlink could not be created",
			map[string]any{"reason": "symlink"})
	}

	resolvedData := dataDir
	if resolvedData == "" {
		// A real directory beside the shim. Saves must never land in the source
		// tree, which is what makes `git status` stay clean.
		if err := os.MkdirAll(filepath.Join(shimDir, "data"), 0o755); err != nil {
			return nil, err
		}
	}

	return &DevShim{
		Dir:        shimDir,
		GameTarget: gameTarget,
		Launcher:   binaryTarget,
		DataDir:    resolvedData,
	}, nil
}

// VerifyDirectory re-verifies a built dist/ without the launcher (§13.2).
//
// It is the same code path the build runs as its last gate, so a user, a mirror
// operator or a support engineer gets the packaging pipeline's own answer
// rather than a second, weaker implementation.
func VerifyDirectory(dir string, log *Logger) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Fail(EvVerifyFail, "the directory could not be read", map[string]any{"reason": "unreadable"})
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return Fail(EvVerifyFail, "the directory holds no artefacts", map[string]any{"reason": "empty"})
	}
	if err := VerifyOutput(dir, names, log); err != nil {
		return err
	}

	// A distribution ZIP must ship only data/.keep under data/ (§2.4).
	for _, name := range names {
		if filepath.Ext(name) != ".zip" {
			continue
		}
		strays, err := zipDataStrays(filepath.Join(dir, name))
		if err != nil {
			return Fail(EvVerifyFail, "a distribution ZIP could not be read", map[string]any{"file": name})
		}
		if len(strays) > 0 {
			log.Error(EvVerifyFail, map[string]any{"file": name, "reason": "data_entry", "paths": len(strays)})
			return Fail(EvVerifyFail,
				fmt.Sprintf("%d file(s) under data/ in a distribution ZIP (only data/.keep may ship)", len(strays)),
				map[string]any{"file": name})
		}
	}
	log.Info(EvVerifyOK, map[string]any{"dir": dir, "files": len(names)})
	return nil
}

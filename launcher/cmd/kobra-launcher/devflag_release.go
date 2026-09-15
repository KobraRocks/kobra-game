//go:build !kobra_dev

package main

import (
	"flag"

	"kobragames.local/launcher/internal/paths"
)

// This file is compiled into every non-development build. It provides the same
// symbols as devflag_dev.go with no override behaviour at all: there is no flag
// to register, no notice to print, and the game folder always comes from the
// executable path as FR-LNCH-1 requires.
//
// A release binary given --game-dir therefore fails flag parsing with "flag
// provided but not defined", which is the intended behaviour.

// registerDevFlags is a no-op in a release build.
func registerDevFlags(fs *flag.FlagSet) {}

// devNotice never prints in a release build.
func devNotice() {}

// devResolve is the plain §5.1 resolution.
func devResolve(exe, dataDir string) (paths.Root, error) {
	return paths.Resolve(exe, dataDir)
}

// devGameFolderForRecovery uses the executable-derived folder only.
func devGameFolderForRecovery(exe string) (string, error) {
	return paths.GameFolderFromExecutable(exe)
}

// hasDevGameDir is always false in a release build: the flag does not exist.
func hasDevGameDir() bool { return false }

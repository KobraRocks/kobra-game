//go:build kobra_dev

package main

import (
	"flag"
	"fmt"
	"os"

	"kobragames.local/launcher/internal/paths"
)

// This file is compiled only when the kobra_dev build tag is set, which is what
// keeps --game-dir out of a release binary. The tag is set by `make build-dev`.
//
// FR-LNCH-1 requires the game folder to come from the launcher's own executable
// path. --game-dir overrides that, so it exists for development only: the
// release Makefile does not set the tag, and therefore a shipped binary has no
// code path that can accept a game folder from the command line. This is the
// spec-clean way to weaken FR-LNCH-1 for developers without weakening it for
// users.

// devGameDir holds the --game-dir value once the flag has been parsed.
var devGameDir string

// registerDevFlags adds the development-only flags to fs.
func registerDevFlags(fs *flag.FlagSet) {
	fs.StringVar(&devGameDir, "game-dir", "",
		"DEVELOPMENT BUILD ONLY: serve the game folder at this path instead of deriving it from the launcher's location")
}

// devNotice is printed to stderr before the server starts, so a developer can
// never mistake a dev binary for a release one.
func devNotice() {
	fmt.Fprintln(os.Stderr, "note: this is a development build (kobra_dev); --game-dir is available and FR-LNCH-1 is not enforced.")
}

// devPendingRecovery reports the §19.4 recovery outcome for the dev path. It
// mirrors pendingRecovery in main.go, which is only populated on the strict
// path.
func devResolve(exe, dataDir string) (paths.Root, error) {
	if devGameDir == "" {
		return paths.Resolve(exe, dataDir)
	}
	return paths.ResolveWithOverride(devGameDir, dataDir)
}

// devGameFolderForRecovery returns the folder the §19.4 recovery pass should
// examine: the override when one was given, otherwise the folder derived from
// the executable.
func devGameFolderForRecovery(exe string) (string, error) {
	if devGameDir != "" {
		return devGameDir, nil
	}
	return paths.GameFolderFromExecutable(exe)
}

// devRecoveryOutcome is stored across the two phases of startup so the
// pre-layout recovery pass can be reported once a logger exists.
var devRecoveryOutcome string

// hasDevGameDir reports whether the override flag was supplied. It is defined
// here because only a dev build has the flag.
func hasDevGameDir() bool { return devGameDir != "" }

package update

import "os"

// spacePath picks the path whose filesystem is the binding constraint
// (spec R10.4): the game folder's, because that is where the staging copy is
// created and where the swap happens. The sidecar may live on another volume, in
// which case its own space is not what limits the install.
//
// It has no platform dependency, so it lives here rather than being duplicated
// in the per-OS file (it used to be byte-identical in space_unix.go and
// space_windows.go).
func spacePath(gameFolder string) string {
	if st, err := os.Stat(gameFolder); err == nil && st.IsDir() {
		return gameFolder
	}
	return ""
}

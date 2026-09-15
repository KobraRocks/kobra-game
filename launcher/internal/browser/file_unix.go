//go:build !windows

package browser

import "os"

// isExecutableFile reports whether p is a regular file with at least one execute
// bit set. Symlinks installed by snap and flatpak are followed by os.Stat, which
// is what makes the export wrappers usable.
func isExecutableFile(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	return st.Mode().Perm()&0o111 != 0
}

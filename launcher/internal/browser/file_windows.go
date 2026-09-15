//go:build windows

package browser

import "os"

// isExecutableFile reports whether p is a regular file. Windows does not track
// POSIX execute bits, so the mode check in the Unix implementation would reject
// every browser; the per-candidate path lists already pin the ".exe" name.
func isExecutableFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

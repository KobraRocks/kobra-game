//go:build !windows

package browser

import "os/exec"

// applySysProcAttr is a no-op on this platform. §20.3 only requires SysProcAttr
// on Windows (HideWindow); POSIX launches inherit the launcher's session so the
// browser behaves like any other terminal-spawned GUI process.
func applySysProcAttr(cmd *exec.Cmd) {}

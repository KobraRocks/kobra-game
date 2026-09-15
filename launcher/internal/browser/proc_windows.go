//go:build windows

package browser

import (
	"os/exec"
	"syscall"
)

// applySysProcAttr hides the console window for the launched browser (§20.3).
func applySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

//go:build unix

package port

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// controlReuseAddr sets SO_REUSEADDR — and only SO_REUSEADDR — on the socket
// the listener is about to use. This is the POSIX half of §10.1:
//
//   - SO_REUSEADDR lets a restart rebind a port whose previous listener is in
//     TIME_WAIT, which is the normal case after a clean shutdown;
//   - SO_REUSEPORT is never set, so two live listeners cannot share the port
//     and a second bind fails with EADDRINUSE as §8.3 requires.
func controlReuseAddr(network, address string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return sockErr
}

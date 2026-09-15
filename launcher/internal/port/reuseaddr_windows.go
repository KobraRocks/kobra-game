//go:build windows

package port

import "syscall"

// controlReuseAddr is deliberately a no-op on Windows. Windows' SO_REUSEADDR
// means "allow another socket to hijack an address already in use", which is
// the opposite of what §10.1 requires; Windows' default exclusive bind already
// gives the launcher the behaviour it wants (a second bind of a live port
// fails). The launcher therefore sets no socket option here, and never
// SO_REUSEPORT.
func controlReuseAddr(network, address string, c syscall.RawConn) error {
	return nil
}

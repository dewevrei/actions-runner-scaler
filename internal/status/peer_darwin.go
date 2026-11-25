package status

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID returns the pid the kernel records for the other end of c
// (getsockopt SOL_LOCAL, LOCAL_PEERPID).
func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var optErr error
	if err := raw.Control(func(fd uintptr) {
		pid, optErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	if optErr != nil {
		return 0, optErr
	}
	return pid, nil
}

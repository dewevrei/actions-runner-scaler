//go:build !darwin

package status

import (
	"errors"
	"net"
)

// peerPID is implemented only on macOS, the platform the scaler runs on.
func peerPID(*net.UnixConn) (int, error) {
	return 0, errors.New("reading the peer pid of a unix socket is implemented only on macOS")
}

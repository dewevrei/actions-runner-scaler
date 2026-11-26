package main

import "golang.org/x/sys/unix"

// isTerminal reports whether fd is a terminal: only a terminal answers the
// request for its attributes (0003 SPEC R285).
func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

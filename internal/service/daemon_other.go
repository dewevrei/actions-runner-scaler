//go:build !darwin

package service

import (
	"context"
	"errors"

	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

var errNotDarwin = errors.New("stopping the scaler is implemented only on macOS")

// SocketDaemon is implemented only on macOS, the platform the scaler runs
// on. Elsewhere every method fails.
type SocketDaemon struct{}

// NewSocketDaemon returns a SocketDaemon whose methods fail.
func NewSocketDaemon(string) *SocketDaemon { return &SocketDaemon{} }

// Find fails off macOS.
func (*SocketDaemon) Find(context.Context) (status.Report, int, bool, error) {
	return status.Report{}, 0, false, errNotDarwin
}

// Terminate fails off macOS.
func (*SocketDaemon) Terminate(int) error { return errNotDarwin }

// Watch fails off macOS.
func (*SocketDaemon) Watch(int) (ExitWatch, error) { return nil, errNotDarwin }

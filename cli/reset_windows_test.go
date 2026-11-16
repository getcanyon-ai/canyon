//go:build windows

package cli

import (
	"net"
	"os"
	"syscall"
	"testing"
)

// On Windows a reset arrives as Winsock's WSAECONNRESET, and it is an interruption to wait out there too (#2730).
func TestAWindowsResetIsAnInterruption(t *testing.T) {
	err := &net.OpError{Op: "wsarecv", Net: "tcp", Err: os.NewSyscallError("wsarecv", syscall.WSAECONNRESET)}
	if !interruption(err) {
		t.Errorf("%v is not read as an interruption", err)
	}
}

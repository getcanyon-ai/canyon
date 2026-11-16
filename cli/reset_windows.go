//go:build windows

package cli

import (
	"errors"
	"syscall"
)

// resetByPeer is a connection the host reset, as it does while it restarts (#2730). Winsock says so as WSAECONNRESET
// (10054), which the ECONNRESET Go invents for Windows never matches.
func resetByPeer(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, syscall.ECONNRESET)
}

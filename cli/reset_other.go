//go:build !windows

package cli

import (
	"errors"
	"syscall"
)

// resetByPeer is a connection the host reset, as it does while it restarts (#2730).
func resetByPeer(err error) bool { return errors.Is(err, syscall.ECONNRESET) }

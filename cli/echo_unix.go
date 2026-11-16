//go:build darwin || linux

package cli

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

// echoOff clears the echo bit on the terminal the prompt will read and hands back the restore. It is
// stdlib on purpose: `stty -echo` through os/exec resolves a name on PATH, and a hijacked copy that
// leaves echo on is the exact failure this verb prevents; x/term is a dependency for one call.
// Anything it cannot prove is errEchoOn — not a file, a failed ioctl, or the case a pty can produce: a kernel that takes the write and keeps echoing, which is why the bit is read back.
func echoOff(in io.Reader) (func(), error) {
	f, ok := in.(*os.File)
	if !ok {
		return nil, errEchoOn
	}
	var before, quiet, now syscall.Termios
	if ioctl(f, readTermios, &before) != nil {
		return nil, errEchoOn
	}
	quiet = before
	quiet.Lflag &^= syscall.ECHO
	if ioctl(f, writeTermios, &quiet) != nil {
		return nil, errEchoOn
	}
	restore := func() { _ = ioctl(f, writeTermios, &before) }
	if ioctl(f, readTermios, &now) != nil || now.Lflag&syscall.ECHO != 0 {
		restore()
		return nil, errEchoOn
	}
	return restore, nil
}

func ioctl(f *os.File, request uintptr, t *syscall.Termios) error {
	//nolint:gosec // G103: the documented way to hand a termios to ioctl — the conversion sits inside the call expression, where the runtime keeps the struct alive, and Termios is fixed-size and holds no pointers
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), request, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

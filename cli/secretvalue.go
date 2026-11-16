package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// maxValue is routes.md row 175's cap, compiled in the way CLI-3 compiled row 55's.
const maxValue = 65536

var (
	errEchoOn = errors.New("this terminal will not stop echoing, so the value would be on screen: pipe it in instead, printf %s VALUE | canyon secret set NAME")
	errEmpty  = errors.New("an empty value is not a secret, so nothing was set")
	errPasted = errors.New("the prompt takes one line, and more than one arrived: redirect a multi-line value in instead, canyon secret set NAME < key.pem")
)

// value is the only way a value enters this process. Argv is refused by the caller, because the shell
// has already recorded it. A stdin that is not a terminal is read whole. A terminal that can be asked
// is asked with the echo bit down. One that cannot — a harness, or --format ai — is told to pipe and never read: a read there hangs it for ever.
func (a *app) value(name string) (string, error) {
	switch {
	case !a.stdinIsTerminal():
		return piped(a.in)
	case !a.canPrompt():
		return "", fmt.Errorf("nothing here can answer a prompt, so pipe the value in: printf %%s VALUE | canyon secret set %s", cell(name))
	}
	return a.typed()
}

func piped(in io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(in, maxValue+2)) // two past the cap: a value at the cap may still arrive with a CRLF, and one byte more than that is long
	if err != nil {
		return "", fmt.Errorf("stdin could not be read: %s", plainly(err))
	}
	return oneValue(string(raw))
}

func (a *app) typed() (string, error) {
	restore, err := echoOff(a.in)
	if err != nil {
		return "", err
	}
	defer restore()
	defer holdSignals(restore)()
	a.errf("\n  Value: ") // stderr, where everything the user is asked lives; nothing appears as it is typed
	line, err := oneLine(bufio.NewReader(io.LimitReader(a.in, maxValue+2)))
	a.errf("\n") // the Enter was not echoed either, so the next line would start beside the prompt
	if err != nil {
		return "", err
	}
	return oneValue(line)
}

// oneLine is the prompt's read. A pasted key arrives whole, and storing its first line under a ✓
// leaves the app failing at run time with nothing on screen — so anything still buffered behind the
// first line is that paste, and it is refused rather than silently cut.
func oneLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	switch {
	case err != nil && !errors.Is(err, io.EOF):
		return "", errors.New("the terminal could not be read")
	case r.Buffered() > 0:
		return "", errPasted
	}
	return line, nil
}

// oneValue strips exactly the newline the shell, a heredoc or the Enter key added, with the carriage
// return that came with it, and nothing else: without it every echo and $(...) stores a trailing
// newline; with any more, a PEM key loses its lines. No error here ever formats the value.
func oneValue(raw string) (string, error) {
	if strings.HasSuffix(raw, "\n") {
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
	}
	switch {
	case raw == "":
		return "", errEmpty
	case len(raw) > maxValue:
		return "", fmt.Errorf("a value is at most %d bytes, and this one is longer", maxValue)
	}
	return raw, nil
}

// holdSignals puts the echo bit back on a Ctrl-C: the default handler would otherwise kill the process between the clear and the restore, leaving the terminal mute.
func holdSignals(restore func()) func() {
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, os.Interrupt, syscall.SIGTERM)
	go func() {
		if _, open := <-caught; open {
			restore()
			os.Exit(exitError)
		}
	}()
	return func() { signal.Stop(caught); close(caught) } // Stop returns with no send in flight, so the close cannot race one
}

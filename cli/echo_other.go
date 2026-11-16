//go:build !darwin && !linux

package cli

import "io"

func echoOff(io.Reader) (func(), error) { return nil, errEchoOn } // no termios here: the value is piped in instead

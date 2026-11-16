package cli

import "syscall"

const readTermios, writeTermios = syscall.TIOCGETA, syscall.TIOCSETA

package cli

import "syscall"

const readTermios, writeTermios = syscall.TCGETS, syscall.TCSETS

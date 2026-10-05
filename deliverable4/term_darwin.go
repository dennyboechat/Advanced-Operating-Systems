package main

import "syscall"

// ioctl requests for reading and writing the terminal attributes on macOS/BSD.
const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)

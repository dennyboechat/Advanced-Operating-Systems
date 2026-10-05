package main

import "syscall"

// ioctl requests for reading and writing the terminal attributes on Linux.
const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)

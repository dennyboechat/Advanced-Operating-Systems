package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Terminal / process-group plumbing.
//
// A Unix kernel gives every terminal exactly one *foreground process group*.
// Only that group may read from the terminal, and only that group receives the
// signals the terminal driver generates (SIGINT on Ctrl-C, SIGTSTP on Ctrl-Z).
// Job control is therefore nothing more than handing that privilege back and
// forth between the shell and the job the user is currently interacting with.
//
// The two operations below are the C library's tcgetpgrp(3)/tcsetpgrp(3); Go's
// standard library does not export them, so they are issued here directly as
// ioctl(2) system calls.

// ioctlPtr performs ioctl(fd, req, arg). The unsafe.Pointer is converted to a
// uintptr inside the syscall expression itself, which is the only form the Go
// garbage collector considers safe.
func ioctlPtr(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// tcgetpgrp returns the process group currently in the foreground of fd.
func tcgetpgrp(fd uintptr) (int, error) {
	var pgrp int32
	if err := ioctlPtr(fd, syscall.TIOCGPGRP, unsafe.Pointer(&pgrp)); err != nil {
		return 0, err
	}
	return int(pgrp), nil
}

// tcsetpgrp makes pgid the foreground process group of the terminal fd.
// The caller must be ignoring or catching SIGTTOU, otherwise the kernel stops
// it whenever it performs this call from the background.
func tcsetpgrp(fd uintptr, pgid int) error {
	pgrp := int32(pgid)
	return ioctlPtr(fd, syscall.TIOCSPGRP, unsafe.Pointer(&pgrp))
}

// isTerminal reports whether f is a terminal.
//
// Two checks are needed. A terminal is a character device, which rules out
// regular files and pipes - on macOS a pipe answers TIOCGPGRP too, because it
// keeps a process group of its own for SIGIO. And it must answer TIOCGPGRP,
// which rules out character devices such as /dev/null that have no foreground
// process group at all.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	_, err = tcgetpgrp(f.Fd())
	return err == nil
}

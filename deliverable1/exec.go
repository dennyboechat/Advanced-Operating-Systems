package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// runExternal creates a new process for a command that is not built in.
//
// This is the classic Unix sequence:
//
//	fork()  - syscall.ForkExec duplicates this process,
//	exec()  - and immediately replaces the copy with the new program,
//	wait()  - the parent blocks on the child (foreground) or carries on (background),
//	exit()  - the child's status is collected here and becomes $?.
//
// Every job is placed in its own process group so that terminal signals and
// SIGCONT can be delivered to the whole job with a single kill(-pgid, sig).
func (sh *Shell) runExternal(cmd *Command) int {
	path, err := exec.LookPath(cmd.Argv[0])
	if err != nil {
		fmt.Fprintf(sh.errw, "gosh: %s: command not found\n", cmd.Argv[0])
		return 127
	}

	// Wire up the child's standard file descriptors. Anything the shell opened
	// for a redirection is closed again in the parent once the fork is done:
	// the descriptor now lives on in the child.
	files := []uintptr{0, 1, 2}
	var toClose []*os.File

	if cmd.InFile != "" {
		f, err := os.Open(cmd.InFile)
		if err != nil {
			fmt.Fprintf(sh.errw, "gosh: %s: %v\n", cmd.InFile, errNoPath(err))
			return 1
		}
		files[0] = f.Fd()
		toClose = append(toClose, f)
	}
	if cmd.OutFile != "" {
		flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if cmd.Append {
			flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		}
		f, err := os.OpenFile(cmd.OutFile, flags, 0o644)
		if err != nil {
			closeAll(toClose)
			fmt.Fprintf(sh.errw, "gosh: %s: %v\n", cmd.OutFile, errNoPath(err))
			return 1
		}
		files[1] = f.Fd()
		toClose = append(toClose, f)
	}

	attr := &syscall.ProcAttr{
		Env:   os.Environ(),
		Files: files,
		Sys: &syscall.SysProcAttr{
			Setpgid: true, // the child becomes the leader of a new process group
			Pgid:    0,    // 0 means "use the child's own pid as the group id"
		},
	}

	pid, err := syscall.ForkExec(path, cmd.Argv, attr)
	closeAll(toClose)
	if err != nil {
		fmt.Fprintf(sh.errw, "gosh: %s: %v\n", cmd.Argv[0], err)
		return 1
	}

	// The child sets its own process group before exec(), but the parent may
	// reach the next line first. Repeating the call here removes that race; one
	// of the two calls always wins and the second one fails harmlessly.
	_ = syscall.Setpgid(pid, pid)

	job := &Job{
		PID:     pid,
		PGID:    pid,
		Cmd:     cmd.Text,
		State:   StateRunning,
		Started: time.Now(),
		Bg:      cmd.Background,
	}
	sh.jobs.add(job)

	if cmd.Background {
		fmt.Fprintf(sh.out, "[%d] %d\n", job.ID, job.PID)
		return 0
	}

	sh.waitForeground(job)
	return sh.lastStatus
}

// setTerminal makes pgid the foreground process group of the controlling
// terminal.
//
// SIGTTOU has to be ignored for the duration. Reclaiming the terminal is by
// definition done from the background - the job the shell is taking it away
// from is still the foreground group - and the kernel's response to a
// background process touching the terminal is to stop it with SIGTTOU, or to
// fail the call outright when the group cannot be stopped (EIO on an orphaned
// process group). Ignoring the signal is what tells the kernel this is
// deliberate; a merely *caught* signal is not enough.
func (sh *Shell) setTerminal(pgid int) error {
	signal.Ignore(syscall.SIGTTOU)
	defer signal.Notify(sh.sigs, syscall.SIGTTOU)
	return tcsetpgrp(sh.ttyFD, pgid)
}

// giveTerminal hands the terminal to a job, so it can read from the keyboard
// and receive Ctrl-C / Ctrl-Z.
func (sh *Shell) giveTerminal(pgid int) {
	if !sh.interactive {
		return
	}
	if err := sh.setTerminal(pgid); err != nil {
		fmt.Fprintf(sh.errw, "gosh: cannot hand over the terminal: %v\n", err)
	}
}

// takeTerminal claims the terminal back for the shell.
func (sh *Shell) takeTerminal() {
	if !sh.interactive {
		return
	}
	if err := sh.setTerminal(sh.pgid); err != nil {
		fmt.Fprintf(sh.errw, "gosh: cannot reclaim the terminal: %v\n", err)
	}
}

func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// errNoPath strips the redundant "open /some/path:" prefix os errors carry, so
// messages read like a shell's rather than like a Go stack trace.
func errNoPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

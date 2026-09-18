// Deliverable 1 - gosh, a custom shell that simulates a Unix-like operating
// system environment.
//
// The shell owns a small process table and drives the four system calls that
// every Unix process life cycle is built from:
//
//	fork()  a new process is created as a copy of the shell,
//	exec()  the copy is overwritten with the requested program,
//	wait()  the shell collects the child's termination or stop status,
//	exit()  that status becomes the shell's $? and the job leaves the table.
//
// Built-in commands are deliberately *not* forked: they run inside the shell so
// that they can change its own state (cd, exit, export) or inspect its process
// table (jobs, fg, bg, ps).
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Shell is the whole interpreter: its input source, its process table and the
// terminal it arbitrates between itself and its jobs.
type Shell struct {
	in   *bufio.Reader
	out  io.Writer
	errw io.Writer

	stdin io.Reader // what a built-in reads; redirected by '<'

	jobs *JobTable

	pid     int       // the shell's own process id
	pgid    int       // the shell's process group; owns the terminal when idle
	started time.Time //

	ttyFD       uintptr        // controlling terminal, when there is one
	interactive bool           // prompt, job control and terminal handover are enabled
	sigs        chan os.Signal // terminal signals the shell swallows

	lastStatus int    // $?, the exit status of the last command
	prevDir    string // $OLDPWD, the target of `cd -`

	exiting   bool // `exit` was executed
	exitCode  int  //
	exitAsked bool // an exit was refused because jobs were stopped
}

func main() {
	sh := &Shell{
		out:     os.Stdout,
		errw:    os.Stderr,
		stdin:   os.Stdin,
		jobs:    NewJobTable(),
		pid:     os.Getpid(),
		started: time.Now(),
		ttyFD:   os.Stdin.Fd(),
	}

	source := os.Stdin
	if len(os.Args) > 1 {
		f, err := os.Open(os.Args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "gosh: %s: %v\n", os.Args[1], errNoPath(err))
			os.Exit(1)
		}
		defer f.Close()
		source = f
	} else {
		sh.interactive = isTerminal(os.Stdin)
	}
	sh.in = bufio.NewReader(source)

	sh.setup()
	os.Exit(sh.Run())
}

// setup arranges for the shell to survive the signals a terminal generates and,
// when interactive, makes it the owner of that terminal.
func (sh *Shell) setup() {
	// These signals are *caught and discarded* rather than ignored. The
	// difference matters: exec() resets caught signals to their default in the
	// new program, while an ignored signal stays ignored across exec() and
	// would leave every child immune to Ctrl-C.
	sh.sigs = make(chan os.Signal, 8)
	signal.Notify(sh.sigs,
		syscall.SIGINT,  // Ctrl-C: belongs to the foreground job, never to us
		syscall.SIGQUIT, // Ctrl-\
		syscall.SIGTSTP, // Ctrl-Z: the shell itself must not stop
		syscall.SIGTTIN, // reading from the terminal while in the background
		syscall.SIGTTOU) // writing/tcsetpgrp while in the background
	go func() {
		for range sh.sigs {
			// Nothing to do: the point is only to keep the default action
			// (terminate or stop the shell) from happening.
		}
	}()

	if !sh.interactive {
		sh.pgid = syscall.Getpgrp()
		return
	}

	// Wait until the shell really is in the foreground. If it was started in
	// the background, stop ourselves and let the user resume us; on the way
	// back the loop runs again and the test finally succeeds.
	for i := 0; i < 100; i++ {
		fg, err := tcgetpgrp(sh.ttyFD)
		if err != nil || fg == syscall.Getpgrp() {
			break
		}
		syscall.Kill(-syscall.Getpgrp(), syscall.SIGTTIN)
	}

	// Become a process group of our own and claim the terminal, so that jobs
	// can later be given it and taken off it again. A session leader already
	// leads its own group, and asking again would fail with EPERM.
	if syscall.Getpgrp() != sh.pid {
		if err := syscall.Setpgid(0, 0); err != nil {
			fmt.Fprintf(sh.errw, "gosh: cannot create a process group: %v\n", err)
			sh.interactive = false
		}
	}
	sh.pgid = syscall.Getpgrp()
	if sh.interactive {
		if err := tcsetpgrp(sh.ttyFD, sh.pgid); err != nil {
			fmt.Fprintf(sh.errw, "gosh: no job control in this shell: %v\n", err)
			sh.interactive = false
		}
	}
}

// Run is the read-eval-print loop. It returns the shell's exit status.
func (sh *Shell) Run() int {
	if sh.interactive {
		fmt.Fprintf(sh.out, "gosh - a Unix-like shell in Go (pid %d). Type 'help' for the built-ins.\n", sh.pid)
	}

	for {
		// Report background jobs that finished or stopped while the user was
		// typing, which is when a real shell prints "[1]+ Done ...".
		if sh.interactive {
			sh.reap(sh.out)
		} else {
			sh.reap(nil)
		}

		if sh.interactive {
			fmt.Fprint(sh.out, sh.prompt())
		}

		line, err := sh.in.ReadString('\n')
		if err != nil {
			if line == "" {
				// End of input: Ctrl-D, or the end of a script. There is no
				// one left to ask, so the exit cannot be refused - it still
				// goes through `exit` to hang up whatever jobs remain.
				if sh.interactive {
					fmt.Fprintln(sh.out, "exit")
				}
				sh.exitAsked = true
				sh.runLine("exit")
				break
			}
		}

		sh.runLine(line)
		if sh.exiting {
			break
		}
	}

	sh.reap(nil)
	return sh.exitCode
}

// runLine parses and executes a single input line.
func (sh *Shell) runLine(line string) {
	cmd, err := sh.parse(line)
	if err != nil {
		fmt.Fprintf(sh.errw, "gosh: %v\n", err)
		sh.lastStatus = 2
		return
	}
	if cmd == nil {
		return // blank line or comment
	}
	sh.lastStatus = sh.execute(cmd)
}

// execute dispatches one command, either into the built-in table or into a new
// process created with fork()/exec().
func (sh *Shell) execute(cmd *Command) int {
	if cmd.Argv[0] != "exit" {
		sh.exitAsked = false
	}

	builtin, ok := builtins[cmd.Argv[0]]
	if !ok {
		return sh.runExternal(cmd)
	}

	// Built-ins run in this process, so their redirections are applied by
	// swapping the writers they are handed rather than by touching fd 0/1/2.
	out, errw, restore, err := sh.redirect(cmd)
	if err != nil {
		fmt.Fprintf(sh.errw, "gosh: %v\n", err)
		return 1
	}
	defer restore()

	if cmd.Background {
		// A built-in changes the shell's own state, so it cannot meaningfully
		// run in a separate process. Run it now and say so.
		fmt.Fprintf(sh.errw, "gosh: %s: built-in commands always run in the foreground\n", cmd.Argv[0])
	}
	return builtin(sh, cmd.Argv, out, errw)
}

// redirect opens the files named by '<' and '>' and returns the writers a
// built-in should use, plus a function that closes them again.
func (sh *Shell) redirect(cmd *Command) (out, errw io.Writer, restore func(), err error) {
	out, errw = sh.out, sh.errw
	var opened []*os.File
	restore = func() {
		closeAll(opened)
		sh.stdin = os.Stdin
	}

	if cmd.InFile != "" {
		f, e := os.Open(cmd.InFile)
		if e != nil {
			restore()
			return nil, nil, func() {}, fmt.Errorf("%s: %v", cmd.InFile, errNoPath(e))
		}
		opened = append(opened, f)
		sh.stdin = f
	}

	if cmd.OutFile != "" {
		flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if cmd.Append {
			flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		}
		f, e := os.OpenFile(cmd.OutFile, flags, 0o644)
		if e != nil {
			restore()
			return nil, nil, func() {}, fmt.Errorf("%s: %v", cmd.OutFile, errNoPath(e))
		}
		opened = append(opened, f)
		out = f
	}
	return out, errw, restore, nil
}

// prompt renders "gosh:~/somewhere$ ", with the home directory abbreviated.
func (sh *Shell) prompt() string {
	wd, err := os.Getwd()
	if err != nil {
		wd = "?"
	}
	if home := homeDir(); home != "" && strings.HasPrefix(wd, home) {
		wd = "~" + strings.TrimPrefix(wd, home)
	}
	return fmt.Sprintf("gosh:%s$ ", wd)
}

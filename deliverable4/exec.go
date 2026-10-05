package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Running a pipeline.
//
//	cat file | grep error | sort
//
// For a pipeline of n commands the shell creates n-1 pipes with pipe(2). Stage
// i writes into pipe i and stage i+1 reads from it, so data flows left to right
// through the kernel's pipe buffers while every stage runs at the same time.
// A full pipe blocks its writer and an empty one blocks its reader; when the
// last writer closes a pipe the reader sees end-of-file, and when the reader
// goes away the writer gets SIGPIPE (or EPIPE). That is the whole protocol.
//
// Each stage is one of:
//
//	an external program  fork()+exec() with the pipe ends as fd 0 and fd 1.
//	                     All the processes of a pipeline share one process
//	                     group, so Ctrl-C, Ctrl-Z, fg and bg act on the job.
//	a built-in           run on a goroutine inside the shell, reading and
//	                     writing the same pipe ends. It runs in a *subshell*
//	                     (a copy of the shell's state), so a cd or export
//	                     inside a pipeline does not leak out - as in bash.
//	a script             interpreted by a subshell, on a goroutine.
//
// The shell then closes its own copies of the pipe ends - otherwise no reader
// would ever see end-of-file - and waits for every stage. The pipeline's exit
// status is the status of its last command.

// stage is one command of a pipeline while it is being started.
type stage struct {
	cmd *Command

	builtin Builtin  // set for a built-in
	script  string   // virtual path of a script to interpret
	path    string   // host path of an external program
	argv    []string // argv for the external program, paths translated

	in   io.Reader // what the stage reads
	out  io.Writer // where it writes
	inF  *os.File  // in, as a file descriptor an external program can inherit
	outF *os.File  // out, likewise

	owned []*os.File // descriptors this stage closes when it is done

	status int
	failed bool // could not be started; status says why
}

// noPipeline lists built-ins that prompt for a password or move jobs between
// the foreground and the background. They need the terminal and the shell's
// own state, so they only make sense as a command of their own.
var noPipeline = map[string]bool{
	"su": true, "logout": true, "passwd": true, "useradd": true, "fg": true, "bg": true,
}

// execute runs one parsed line and returns its exit status.
func (sh *Shell) execute(p *Pipeline) int {
	if p.Cmds[0].Argv[0] != "exit" && p.Cmds[0].Argv[0] != "logout" {
		sh.exitAsked = false
	}

	// A lone built-in runs in the shell itself, so that cd, export, su and
	// friends change the shell's state.
	if len(p.Cmds) == 1 {
		if b, ok := builtins[p.Cmds[0].Argv[0]]; ok {
			return sh.runBuiltin(p.Cmds[0], b, p.Background)
		}
	}
	if len(p.Cmds) > 1 {
		for _, c := range p.Cmds {
			if noPipeline[c.Argv[0]] {
				fmt.Fprintf(sh.errw, "aos: %s: cannot be used in a pipeline\n", c.Argv[0])
				return 2
			}
		}
	}
	return sh.runPipeline(p)
}

// runBuiltin runs a built-in inside the shell process, with its redirections
// applied by handing it different readers and writers.
func (sh *Shell) runBuiltin(cmd *Command, b Builtin, background bool) int {
	in, out := io.Reader(sh.stdin), sh.out
	if cmd.InFile != "" {
		f, err := sh.fs.Open(sh.user, sh.abs(cmd.InFile))
		if err != nil {
			fmt.Fprintf(sh.errw, "aos: %s: %v\n", cmd.InFile, err)
			return 1
		}
		defer f.Close()
		in = f
	}
	if cmd.OutFile != "" {
		f, err := sh.fs.Create(sh.user, sh.abs(cmd.OutFile), cmd.Append)
		if err != nil {
			fmt.Fprintf(sh.errw, "aos: %s: %v\n", cmd.OutFile, err)
			return 1
		}
		defer f.Close()
		out = f
	}
	if background {
		// A built-in changes the shell's own state, so it cannot meaningfully
		// run in a separate process. Run it now and say so.
		fmt.Fprintf(sh.errw, "aos: %s: built-in commands always run in the foreground\n", cmd.Argv[0])
	}
	return b(sh, cmd.Argv, in, out, sh.errw)
}

// runPipeline starts every stage, connects them with pipes and waits.
func (sh *Shell) runPipeline(p *Pipeline) int {
	n := len(p.Cmds)
	errw := &lockedWriter{w: sh.errw} // stages report errors concurrently
	stdout := &sink{w: sh.out}
	stderr := &sink{w: errw}
	defer stdout.close()
	defer stderr.close()

	stages := make([]*stage, n)
	for i, c := range p.Cmds {
		stages[i] = &stage{cmd: c, in: sh.stdin, out: sh.out}
		if i == 0 {
			if f, ok := sh.stdin.(*os.File); ok {
				stages[i].inF = f
			}
		}
	}

	// pipe(2) between each pair of neighbours.
	for i := 0; i < n-1; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			fmt.Fprintf(errw, "aos: pipe: %v\n", err)
			for _, st := range stages {
				closeAll(st.owned)
			}
			return 1
		}
		stages[i].out, stages[i].outF = w, w
		stages[i].owned = append(stages[i].owned, w)
		stages[i+1].in, stages[i+1].inF = r, r
		stages[i+1].owned = append(stages[i+1].owned, r)
	}

	// Redirections override the pipe ends, as in sh: "a > f | b" gives b an
	// empty input.
	for _, st := range stages {
		sh.prepare(st, errw)
	}

	var (
		job     *Job
		wg      sync.WaitGroup
		bgStage bool // a built-in or script stage is running on a goroutine
	)

	for _, st := range stages {
		switch {
		case st.failed:
			closeAll(st.owned)

		case st.builtin != nil || st.script != "":
			// The subshell is a copy taken now, on the main goroutine.
			sub := sh.subshellCopy()
			bgStage = true
			wg.Add(1)
			go func(st *stage) {
				defer wg.Done()
				defer closeAll(st.owned) // our end of the pipe: the neighbour sees EOF / EPIPE
				if st.builtin != nil {
					st.status = st.builtin(sub, st.cmd.Argv, st.in, st.out, errw)
				} else {
					st.status = sub.runScript(st.script, st.cmd.Argv, st.in, st.out, errw)
				}
			}(st)

		default:
			if st.outF == nil {
				f, err := stdout.file()
				if err != nil {
					fmt.Fprintf(errw, "aos: %v\n", err)
					st.failed, st.status = true, 1
					closeAll(st.owned)
					continue
				}
				st.outF = f
			}
			errF, err := stderr.file()
			if err != nil {
				fmt.Fprintf(errw, "aos: %v\n", err)
				st.failed, st.status = true, 1
				closeAll(st.owned)
				continue
			}

			pgid := 0
			if job != nil {
				pgid = job.PGID
			}
			pid, err := sh.spawn(st, errF, pgid)
			// The child has its own copies of the descriptors now. Closing
			// ours is what lets the next stage see end-of-file.
			closeAll(st.owned)
			if err != nil {
				fmt.Fprintf(errw, "aos: %s: %v\n", st.cmd.Argv[0], err)
				st.failed, st.status = true, 126
				continue
			}
			if job == nil {
				job = &Job{PGID: pid, Cmd: p.Text, Owner: sh.user.Name, State: StateRunning,
					Started: time.Now(), Bg: p.Background}
			}
			job.Procs = append(job.Procs, &Proc{PID: pid, Cmd: st.cmd.Text, State: StateRunning})
		}
	}
	// Every external program has its stdout/stderr now; the shell's copies of
	// the capture pipes must go, or the capture never sees end-of-file.
	stdout.close()
	stderr.close()

	last := stages[n-1]
	if job != nil {
		sh.jobs.add(job)
		if p.Background {
			fmt.Fprintf(sh.out, "[%d] %d\n", job.ID, job.PID())
			return 0
		}
		status := sh.waitForeground(job)
		if job.State == StateStopped {
			// Built-in stages keep running (or stay blocked on their pipe)
			// while the external part of the job is stopped.
			return status
		}
		if !last.failed && last.builtin == nil && last.script == "" {
			last.status = status
		}
	} else if p.Background && bgStage {
		fmt.Fprintf(sh.errw, "aos: pipelines of built-in commands always run in the foreground\n")
	}

	wg.Wait()
	stdout.wait()
	stderr.wait()
	return last.status
}

// prepare resolves what a stage runs and opens its redirections. Failures are
// reported and leave the stage marked as failed; the rest of the pipeline
// still runs, as in sh.
func (sh *Shell) prepare(st *stage, errw io.Writer) {
	fail := func(status int, format string, args ...any) {
		fmt.Fprintf(errw, "aos: "+format+"\n", args...)
		st.failed, st.status = true, status
	}

	if st.cmd.InFile != "" {
		f, err := sh.fs.Open(sh.user, sh.abs(st.cmd.InFile))
		if err != nil {
			fail(1, "%s: %v", st.cmd.InFile, err)
			return
		}
		st.in, st.inF = f, f
		st.owned = append(st.owned, f)
	}
	if st.cmd.OutFile != "" {
		f, err := sh.fs.Create(sh.user, sh.abs(st.cmd.OutFile), st.cmd.Append)
		if err != nil {
			fail(1, "%s: %v", st.cmd.OutFile, err)
			return
		}
		st.out, st.outF = f, f
		st.owned = append(st.owned, f)
	}

	name := st.cmd.Argv[0]
	if b, ok := builtins[name]; ok {
		st.builtin = b
		return
	}

	// A name with a slash is a file in the shell's file system: a script,
	// which needs read and execute permission.
	if strings.Contains(name, "/") {
		vp := sh.abs(name)
		if err := sh.fs.CheckExec(sh.user, vp); err != nil {
			if errors.Is(err, ErrNotExist) {
				fail(127, "%s: %v", name, err)
			} else {
				fail(126, "%s: %v", name, err)
			}
			return
		}
		st.script = vp
		return
	}

	path, argv, status, err := sh.resolveProgram(st.cmd.Argv)
	if err != nil {
		fail(status, "%v", err)
		return
	}
	st.path, st.argv = path, argv
}

// spawn starts an external program: fork() + exec(), with the stage's pipe ends
// or files as its standard input and output. pgid 0 makes the process the
// leader of a new process group; later stages join the leader's group.
func (sh *Shell) spawn(st *stage, errF *os.File, pgid int) (int, error) {
	files := []uintptr{0, st.outF.Fd(), errF.Fd()}
	if st.inF != nil {
		files[0] = st.inF.Fd()
	}

	attr := &syscall.ProcAttr{
		Dir:   sh.fs.Real(sh.cwd),
		Env:   sh.environ(),
		Files: files,
		Sys: &syscall.SysProcAttr{
			Setpgid: true, // the child joins (or leads) the job's process group
			Pgid:    pgid, // 0 means "use the child's own pid as the group id"
		},
	}
	pid, err := syscall.ForkExec(st.path, st.argv, attr)
	if err != nil {
		return 0, err
	}
	// The child sets its own process group before exec(), but the parent may
	// reach the next line first. Repeating the call here removes that race; one
	// of the two calls always wins and the second one fails harmlessly.
	if pgid == 0 {
		pgid = pid
	}
	_ = syscall.Setpgid(pid, pgid)
	return pid, nil
}

// sink turns the shell's output writer into a file descriptor a child process
// can inherit. Normally that writer is the terminal (os.Stdout) and is used
// directly. When it is something else - a buffer in the tests, or the pipe of
// a script running inside a pipeline - a pipe is created and a goroutine copies
// whatever the children write into the writer.
type sink struct {
	w    io.Writer
	pw   *os.File      // write end handed to children
	done chan struct{} // closed when the copy has finished
}

func (s *sink) file() (*os.File, error) {
	if f, ok := s.w.(*os.File); ok {
		return f, nil
	}
	if s.pw == nil {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		s.pw, s.done = w, make(chan struct{})
		go func() {
			io.Copy(s.w, r)
			r.Close()
			close(s.done)
		}()
	}
	return s.pw, nil
}

// close drops the shell's copy of the write end.
func (s *sink) close() {
	if s.pw != nil {
		s.pw.Close()
		s.pw = nil
	}
}

// wait blocks until every child holding the write end has exited and the
// output has been copied.
func (s *sink) wait() {
	if s.done != nil {
		<-s.done
	}
}

// lockedWriter serializes writes from concurrent pipeline stages.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// lookPath searches the shell's $PATH for an executable.
func (sh *Shell) lookPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(sh.getenv("PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: command not found", name)
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
	if !sh.jobControl {
		return
	}
	if err := sh.setTerminal(pgid); err != nil {
		fmt.Fprintf(sh.errw, "aos: cannot hand over the terminal: %v\n", err)
	}
}

// takeTerminal claims the terminal back for the shell.
func (sh *Shell) takeTerminal() {
	if !sh.jobControl {
		return
	}
	if err := sh.setTerminal(sh.pgid); err != nil {
		fmt.Fprintf(sh.errw, "aos: cannot reclaim the terminal: %v\n", err)
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

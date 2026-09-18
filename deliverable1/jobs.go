package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProcState is the high-level state the shell keeps for every process it has
// created, mirroring the state a real kernel keeps in its process table.
type ProcState int

const (
	StateRunning    ProcState = iota // scheduled, or ready to be scheduled
	StateStopped                     // suspended by SIGTSTP/SIGSTOP, resumable
	StateDone                        // exited on its own
	StateTerminated                  // killed by a signal
)

func (s ProcState) String() string {
	switch s {
	case StateRunning:
		return "Running"
	case StateStopped:
		return "Stopped"
	case StateDone:
		return "Done"
	default:
		return "Terminated"
	}
}

// Job is one entry of the shell's process table.
type Job struct {
	ID      int            // small user-visible job number, %1, %2, ...
	PID     int            // process id returned by fork()
	PGID    int            // process group, the unit job control acts on
	Cmd     string         // the command line as typed
	State   ProcState      //
	Exit    int            // exit status once State == StateDone
	Signal  syscall.Signal // the signal that killed it, once StateTerminated
	Started time.Time      //
	Bg      bool           // started with '&' or resumed with bg
	seq     uint64         // activation order, used to pick %+ and %-
}

// JobTable holds every live job. The shell is single threaded, so no locking is
// required: entries are only touched from the main read-eval-print loop.
type JobTable struct {
	jobs map[int]*Job
	seq  uint64
}

func NewJobTable() *JobTable {
	return &JobTable{jobs: make(map[int]*Job)}
}

// add inserts a job, giving it the lowest free job number.
func (t *JobTable) add(j *Job) {
	id := 1
	for {
		if _, taken := t.jobs[id]; !taken {
			break
		}
		id++
	}
	j.ID = id
	t.jobs[id] = j
	t.touch(j)
}

// touch marks a job as the most recently active one, which is what the '+' and
// '-' markers in `jobs` and the default argument of fg/bg refer to.
func (t *JobTable) touch(j *Job) {
	t.seq++
	j.seq = t.seq
}

func (t *JobTable) remove(id int) { delete(t.jobs, id) }

func (t *JobTable) byPID(pid int) *Job {
	for _, j := range t.jobs {
		if j.PID == pid {
			return j
		}
	}
	return nil
}

// list returns the jobs sorted by job number.
func (t *JobTable) list() []*Job {
	out := make([]*Job, 0, len(t.jobs))
	for _, j := range t.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// current and previous return the jobs marked '+' and '-'.
func (t *JobTable) current() *Job  { return t.nth(0) }
func (t *JobTable) previous() *Job { return t.nth(1) }

func (t *JobTable) nth(n int) *Job {
	byAge := t.list()
	sort.SliceStable(byAge, func(a, b int) bool { return byAge[a].seq > byAge[b].seq })
	if n < len(byAge) {
		return byAge[n]
	}
	return nil
}

// marker returns "+", "-" or " " for the job listing.
func (t *JobTable) marker(j *Job) string {
	switch {
	case t.current() == j:
		return "+"
	case t.previous() == j:
		return "-"
	default:
		return " "
	}
}

// status is the human readable state used when reporting a job.
func (j *Job) status() string {
	switch j.State {
	case StateDone:
		if j.Exit != 0 {
			return fmt.Sprintf("Exit %d", j.Exit)
		}
		return "Done"
	case StateTerminated:
		return fmt.Sprintf("Terminated (%s)", signalName(j.Signal))
	default:
		return j.State.String()
	}
}

// report prints one line of the `jobs` listing, e.g.
//
//	[1]+  Running                 sleep 100 &
func (t *JobTable) report(w io.Writer, j *Job) {
	suffix := ""
	if j.Bg && j.State == StateRunning {
		suffix = " &"
	}
	fmt.Fprintf(w, "[%d]%s  %-22s %s%s\n", j.ID, t.marker(j), j.status(), j.Cmd, suffix)
}

// find resolves a job specification: "1", "%1", "%%", "%+" (current job),
// "%-" (previous job) or an empty string (also the current job).
func (t *JobTable) find(spec string) (*Job, error) {
	spec = strings.TrimPrefix(spec, "%")
	switch spec {
	case "", "%", "+":
		j := t.current()
		if j == nil {
			return nil, fmt.Errorf("current: no such job")
		}
		return j, nil
	case "-":
		j := t.previous()
		if j == nil {
			return nil, fmt.Errorf("previous: no such job")
		}
		return j, nil
	}

	id, err := strconv.Atoi(spec)
	if err != nil {
		return nil, fmt.Errorf("%s: no such job", spec)
	}
	j, ok := t.jobs[id]
	if !ok {
		return nil, fmt.Errorf("%%%d: no such job", id)
	}
	return j, nil
}

// ---------------------------------------------------------------------------
// Waiting on children
// ---------------------------------------------------------------------------

// applyStatus copies a wait status returned by wait4() into the process table.
func (sh *Shell) applyStatus(j *Job, ws syscall.WaitStatus) {
	switch {
	case ws.Stopped():
		j.State = StateStopped
		j.Bg = true
		sh.jobs.touch(j)
	case ws.Continued():
		j.State = StateRunning
	case ws.Signaled():
		j.State = StateTerminated
		j.Signal = ws.Signal()
	default:
		j.State = StateDone
		j.Exit = ws.ExitStatus()
	}
}

// waitForeground blocks until the job exits or is stopped, with the terminal
// handed over to it for the duration. This is wait(2) with WUNTRACED, the flag
// that makes the kernel report a stopped child instead of waiting for its death.
func (sh *Shell) waitForeground(j *Job) {
	sh.giveTerminal(j.PGID)
	defer sh.takeTerminal()

	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(j.PID, &ws, syscall.WUNTRACED, nil)
		if err == syscall.EINTR {
			continue // interrupted by a signal handler, ask again
		}
		if err != nil {
			fmt.Fprintf(sh.errw, "gosh: wait: %v\n", err)
			sh.jobs.remove(j.ID)
			return
		}

		sh.applyStatus(j, ws)
		switch j.State {
		case StateStopped:
			// Keep it in the table so it can be resumed with fg/bg.
			fmt.Fprintln(sh.out)
			sh.jobs.report(sh.out, j)
			sh.lastStatus = 128 + int(syscall.SIGTSTP)
		case StateTerminated:
			// bash stays silent about Ctrl-C; anything else is worth saying.
			if j.Signal != syscall.SIGINT && j.Signal != syscall.SIGPIPE {
				fmt.Fprintf(sh.out, "%s\n", signalName(j.Signal))
			} else {
				fmt.Fprintln(sh.out)
			}
			sh.lastStatus = 128 + int(j.Signal)
			sh.jobs.remove(j.ID)
		default:
			sh.lastStatus = j.Exit
			sh.jobs.remove(j.ID)
		}
		return
	}
}

// reap collects background children that changed state since it last ran and
// reports them on w, or silently when w is nil. wait4() with WNOHANG never
// blocks: it reports whatever is ready and returns 0 when nothing is.
func (sh *Shell) reap(w io.Writer) {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG|syscall.WUNTRACED, nil)
		if err != nil || pid <= 0 {
			return // ECHILD (no children left) or nothing to report
		}

		j := sh.jobs.byPID(pid)
		if j == nil {
			continue // not one of ours
		}
		sh.applyStatus(j, ws)
		if w != nil {
			sh.jobs.report(w, j)
		}
		if j.State == StateDone || j.State == StateTerminated {
			sh.jobs.remove(j.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Job control built-ins
// ---------------------------------------------------------------------------

// builtinJobs lists the process table entries the user still owns.
func builtinJobs(sh *Shell, args []string, out, errw io.Writer) int {
	long := false
	for _, a := range args[1:] {
		if a == "-l" {
			long = true
		}
	}

	// Collect anything that finished while the user was typing, so the listing
	// reflects the present rather than the last prompt.
	sh.reap(out)

	for _, j := range sh.jobs.list() {
		if long {
			fmt.Fprintf(out, "[%d]%s %6d %6d  %-22s %s\n",
				j.ID, sh.jobs.marker(j), j.PID, j.PGID, j.status(), j.Cmd)
			continue
		}
		sh.jobs.report(out, j)
	}
	return 0
}

// builtinFg moves a job back into the foreground: the terminal is handed to its
// process group, SIGCONT restarts it if it was stopped, and the shell blocks.
func builtinFg(sh *Shell, args []string, out, errw io.Writer) int {
	spec := ""
	if len(args) > 1 {
		spec = args[1]
	}
	j, err := sh.jobs.find(spec)
	if err != nil {
		fmt.Fprintf(errw, "fg: %v\n", err)
		return 1
	}

	fmt.Fprintln(out, j.Cmd)
	sh.jobs.touch(j)
	j.Bg = false
	if j.State == StateStopped {
		if err := syscall.Kill(-j.PGID, syscall.SIGCONT); err != nil {
			fmt.Fprintf(errw, "fg: cannot continue job %d: %v\n", j.ID, err)
			return 1
		}
		j.State = StateRunning
	}
	sh.waitForeground(j)
	return sh.lastStatus
}

// builtinBg restarts a stopped job, but leaves it running in the background so
// the shell keeps the terminal.
func builtinBg(sh *Shell, args []string, out, errw io.Writer) int {
	spec := ""
	if len(args) > 1 {
		spec = args[1]
	}
	j, err := sh.jobs.find(spec)
	if err != nil {
		fmt.Fprintf(errw, "bg: %v\n", err)
		return 1
	}
	if j.State != StateStopped {
		fmt.Fprintf(errw, "bg: job %d is already running\n", j.ID)
		return 1
	}

	if err := syscall.Kill(-j.PGID, syscall.SIGCONT); err != nil {
		fmt.Fprintf(errw, "bg: cannot continue job %d: %v\n", j.ID, err)
		return 1
	}
	j.State = StateRunning
	j.Bg = true
	sh.jobs.touch(j)
	sh.jobs.report(out, j)
	return 0
}

// builtinPs prints the shell's own view of the process table, including the
// shell itself. It is the "high level operating system" view of what exists.
func builtinPs(sh *Shell, args []string, out, errw io.Writer) int {
	sh.reap(out)
	fmt.Fprintf(out, "%6s %6s %-12s %8s  %s\n", "PID", "PGID", "STATE", "TIME", "COMMAND")
	fmt.Fprintf(out, "%6d %6d %-12s %8s  %s\n",
		sh.pid, sh.pgid, "Running", elapsed(sh.started), "gosh (this shell)")
	for _, j := range sh.jobs.list() {
		fmt.Fprintf(out, "%6d %6d %-12s %8s  %s\n",
			j.PID, j.PGID, j.status(), elapsed(j.Started), j.Cmd)
	}
	return 0
}

func elapsed(since time.Time) string {
	d := time.Since(since).Round(time.Second)
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

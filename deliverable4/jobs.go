package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// Proc is one process of a job. A pipeline of external programs is one job
// with one Proc per program, all in the same process group.
type Proc struct {
	PID    int
	Cmd    string         // this stage's text
	State  ProcState      //
	Exit   int            // exit status once StateDone
	Signal syscall.Signal // the signal that killed it, once StateTerminated
}

// status is what $? would be for this process: the exit code, or 128 plus the
// number of the signal that ended it.
func (p *Proc) status() int {
	if p.State == StateTerminated {
		return 128 + int(p.Signal)
	}
	return p.Exit
}

// Job is one entry of the shell's process table.
type Job struct {
	ID      int       // small user-visible job number, %1, %2, ...
	PGID    int       // process group, the unit job control acts on
	Procs   []*Proc   // one per external program in the pipeline
	Cmd     string    // the command line as typed
	Owner   string    // the user who started it
	State   ProcState //
	Started time.Time //
	Bg      bool      // started with '&' or resumed with bg
	seq     uint64    // activation order, used to pick %+ and %-
}

// PID is the process group leader, the pid printed by "[1] 12345".
func (j *Job) PID() int { return j.PGID }

// last is the final process of the pipeline: its status is the job's status.
func (j *Job) last() *Proc { return j.Procs[len(j.Procs)-1] }

func (j *Job) finished() bool {
	return j.State == StateDone || j.State == StateTerminated
}

// recompute derives the job's state from its processes: stopped if any process
// is stopped, finished when all are, running otherwise. A finished pipeline
// reports the outcome of its last command, as in every Unix shell.
func (j *Job) recompute() {
	done := true
	for _, p := range j.Procs {
		if p.State == StateStopped {
			j.State = StateStopped
			return
		}
		if p.State == StateRunning {
			done = false
		}
	}
	if !done {
		j.State = StateRunning
		return
	}
	j.State = j.last().State
}

// JobTable holds every live job. Pipeline stages that are built-ins run on
// their own goroutines and may read the table (jobs | grep ...), so it is
// guarded by a mutex.
type JobTable struct {
	mu   sync.Mutex
	jobs map[int]*Job
	seq  uint64
}

func NewJobTable() *JobTable {
	return &JobTable{jobs: make(map[int]*Job)}
}

// add inserts a job, giving it the lowest free job number.
func (t *JobTable) add(j *Job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := 1
	for {
		if _, taken := t.jobs[id]; !taken {
			break
		}
		id++
	}
	j.ID = id
	t.jobs[id] = j
	t.touchLocked(j)
}

// touch marks a job as the most recently active one, which is what the '+' and
// '-' markers in `jobs` and the default argument of fg/bg refer to.
func (t *JobTable) touch(j *Job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.touchLocked(j)
}

func (t *JobTable) touchLocked(j *Job) {
	t.seq++
	j.seq = t.seq
}

func (t *JobTable) remove(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.jobs, id)
}

// update runs f with the table locked, for changes to a job's fields.
func (t *JobTable) update(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f()
}

// byPID finds the job and process a pid belongs to.
func (t *JobTable) byPID(pid int) (*Job, *Proc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, j := range t.jobs {
		for _, p := range j.Procs {
			if p.PID == pid {
				return j, p
			}
		}
	}
	return nil, nil
}

// list returns the jobs sorted by job number.
func (t *JobTable) list() []*Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.listLocked()
}

func (t *JobTable) listLocked() []*Job {
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
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nthLocked(n)
}

func (t *JobTable) nthLocked(n int) *Job {
	byAge := t.listLocked()
	sort.SliceStable(byAge, func(a, b int) bool { return byAge[a].seq > byAge[b].seq })
	if n < len(byAge) {
		return byAge[n]
	}
	return nil
}

// marker returns "+", "-" or " " for the job listing.
func (t *JobTable) marker(j *Job) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch j {
	case t.nthLocked(0):
		return "+"
	case t.nthLocked(1):
		return "-"
	default:
		return " "
	}
}

// status is the human readable state used when reporting a job.
func (j *Job) status() string {
	switch j.State {
	case StateDone:
		if j.last().Exit != 0 {
			return fmt.Sprintf("Exit %d", j.last().Exit)
		}
		return "Done"
	case StateTerminated:
		return fmt.Sprintf("Terminated (%s)", signalName(j.last().Signal))
	default:
		return j.State.String()
	}
}

// report prints one line of the `jobs` listing, e.g.
//
//	[1]+  Running                 sleep 100 &
func (t *JobTable) report(w io.Writer, j *Job) {
	t.mu.Lock()
	marker := " "
	switch j {
	case t.nthLocked(0):
		marker = "+"
	case t.nthLocked(1):
		marker = "-"
	}
	suffix := ""
	if j.Bg && j.State == StateRunning {
		suffix = " &"
	}
	line := fmt.Sprintf("[%d]%s  %-22s %s%s\n", j.ID, marker, j.status(), j.Cmd, suffix)
	t.mu.Unlock()
	io.WriteString(w, line)
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
	t.mu.Lock()
	j, ok := t.jobs[id]
	t.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%%%d: no such job", id)
	}
	return j, nil
}

// ---------------------------------------------------------------------------
// Waiting on children
// ---------------------------------------------------------------------------

// applyStatus copies a wait status returned by wait4() into the process table.
func (sh *Shell) applyStatus(j *Job, p *Proc, ws syscall.WaitStatus) {
	sh.jobs.update(func() {
		switch {
		case ws.Stopped():
			p.State = StateStopped
			j.Bg = true
		case ws.Continued():
			p.State = StateRunning
		case ws.Signaled():
			p.State = StateTerminated
			p.Signal = ws.Signal()
		default:
			p.State = StateDone
			p.Exit = ws.ExitStatus()
		}
		j.recompute()
	})
	if j.State == StateStopped {
		sh.jobs.touch(j)
	}
}

// waitForeground blocks until every process of the job has exited, or one of
// them is stopped, with the terminal handed over to the job for the duration.
// This is wait(2) with WUNTRACED, the flag that makes the kernel report a
// stopped child instead of waiting for its death. It returns the status of the
// job's last process.
func (sh *Shell) waitForeground(j *Job) int {
	sh.giveTerminal(j.PGID)
	defer sh.takeTerminal()

	for _, p := range j.Procs {
		for p.State == StateRunning {
			var ws syscall.WaitStatus
			_, err := syscall.Wait4(p.PID, &ws, syscall.WUNTRACED, nil)
			if err == syscall.EINTR {
				continue // interrupted by a signal handler, ask again
			}
			if err != nil {
				fmt.Fprintf(sh.errw, "aos: wait: %v\n", err)
				sh.jobs.update(func() { p.State = StateDone; p.Exit = 1; j.recompute() })
				break
			}
			sh.applyStatus(j, p, ws)
			if j.State == StateStopped {
				// Ctrl-Z stops the whole process group. Keep the job in the
				// table so it can be resumed with fg/bg.
				fmt.Fprintln(sh.out)
				sh.jobs.report(sh.out, j)
				return 128 + int(syscall.SIGTSTP)
			}
		}
	}

	sh.jobs.remove(j.ID)
	last := j.last()
	if last.State == StateTerminated {
		// bash stays silent about Ctrl-C and a broken pipe; anything else is
		// worth saying.
		if last.Signal != syscall.SIGINT && last.Signal != syscall.SIGPIPE {
			fmt.Fprintf(sh.out, "%s\n", signalName(last.Signal))
		} else if last.Signal == syscall.SIGINT {
			fmt.Fprintln(sh.out)
		}
	}
	return last.status()
}

// reap collects background children that changed state since it last ran and
// reports them on w, or silently when w is nil. wait4() with WNOHANG never
// blocks: it reports whatever is ready and returns 0 when nothing is.
//
// A subshell (a built-in running as a pipeline stage on its own goroutine)
// never reaps: wait4(-1) there could steal the status of a process the main
// goroutine is waiting for.
func (sh *Shell) reap(w io.Writer) {
	if sh.subshell {
		return
	}
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG|syscall.WUNTRACED, nil)
		if err != nil || pid <= 0 {
			return // ECHILD (no children left) or nothing to report
		}

		j, p := sh.jobs.byPID(pid)
		if j == nil {
			continue // not one of ours
		}
		before := j.State
		sh.applyStatus(j, p, ws)
		if w != nil && j.State != before {
			sh.jobs.report(w, j)
		}
		if j.finished() {
			sh.jobs.remove(j.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Job control built-ins
// ---------------------------------------------------------------------------

// builtinJobs lists the process table entries the user still owns.
func builtinJobs(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
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
			pids := make([]string, len(j.Procs))
			for i, p := range j.Procs {
				pids[i] = strconv.Itoa(p.PID)
			}
			fmt.Fprintf(out, "[%d]%s %-12s %6d  %-22s %s\n",
				j.ID, sh.jobs.marker(j), strings.Join(pids, ","), j.PGID, j.status(), j.Cmd)
			continue
		}
		sh.jobs.report(out, j)
	}
	return 0
}

// builtinFg moves a job back into the foreground: the terminal is handed to its
// process group, SIGCONT restarts it if it was stopped, and the shell blocks.
func builtinFg(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	spec := ""
	if len(args) > 1 {
		spec = args[1]
	}
	j, err := sh.jobs.find(spec)
	if err != nil {
		fmt.Fprintf(errw, "fg: %v\n", err)
		return 1
	}

	if err := sh.mayControl(j); err != nil {
		fmt.Fprintf(errw, "fg: %v\n", err)
		return 1
	}

	fmt.Fprintln(out, j.Cmd)
	sh.jobs.touch(j)
	if j.State == StateStopped {
		if err := syscall.Kill(-j.PGID, syscall.SIGCONT); err != nil {
			fmt.Fprintf(errw, "fg: cannot continue job %d: %v\n", j.ID, err)
			return 1
		}
	}
	sh.jobs.update(func() {
		j.Bg = false
		resume(j)
	})
	return sh.waitForeground(j)
}

// builtinBg restarts a stopped job, but leaves it running in the background so
// the shell keeps the terminal.
func builtinBg(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	spec := ""
	if len(args) > 1 {
		spec = args[1]
	}
	j, err := sh.jobs.find(spec)
	if err != nil {
		fmt.Fprintf(errw, "bg: %v\n", err)
		return 1
	}
	if err := sh.mayControl(j); err != nil {
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
	sh.jobs.update(func() {
		j.Bg = true
		resume(j)
	})
	sh.jobs.touch(j)
	sh.jobs.report(out, j)
	return 0
}

// resume marks the stopped processes of a job as running again after SIGCONT.
// The caller holds the table lock.
func resume(j *Job) {
	for _, p := range j.Procs {
		if p.State == StateStopped {
			p.State = StateRunning
		}
	}
	j.recompute()
}

// mayControl enforces process ownership: a standard user may only signal,
// resume or stop the jobs they started themselves. Administrators may control
// any job.
func (sh *Shell) mayControl(j *Job) error {
	if sh.user.Role == RoleAdmin || j.Owner == sh.user.Name {
		return nil
	}
	sh.audit("DENIED %s signal job %%%d owned by %s", sh.user.Name, j.ID, j.Owner)
	return fmt.Errorf("%%%d: Operation not permitted (job belongs to %s)", j.ID, j.Owner)
}

// builtinPs prints the shell's own view of the process table, including the
// shell itself: one line per process, so a pipeline shows all of its members
// sharing one process group.
func builtinPs(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	sh.reap(out)
	fmt.Fprintf(out, "%6s %6s %-8s %-12s %8s  %s\n", "PID", "PGID", "USER", "STATE", "TIME", "COMMAND")
	fmt.Fprintf(out, "%6d %6d %-8s %-12s %8s  %s\n",
		sh.pid, sh.pgid, sh.user.Name, "Running", elapsed(sh.started), "aos shell")
	for _, j := range sh.jobs.list() {
		for _, p := range j.Procs {
			state := p.State.String()
			if p.State == StateDone && p.Exit != 0 {
				state = fmt.Sprintf("Exit %d", p.Exit)
			}
			fmt.Fprintf(out, "%6d %6d %-8s %-12s %8s  %s\n",
				p.PID, j.PGID, j.Owner, state, elapsed(j.Started), p.Cmd)
		}
	}
	return 0
}

func elapsed(since time.Time) string {
	d := time.Since(since).Round(time.Second)
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

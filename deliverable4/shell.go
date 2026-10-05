package main

// The shell (Deliverable 1's gosh, extended).
//
// The shell owns a small process table and drives the four system calls that
// every Unix process life cycle is built from:
//
//	fork()  a new process is created as a copy of the shell,
//	exec()  the copy is overwritten with the requested program,
//	wait()  the shell collects the child's termination or stop status,
//	exit()  that status becomes the shell's $? and the job leaves the table.
//
// Deliverable 4 adds pipelines (exec.go) and security: a login, user accounts
// with permission levels (users.go, security.go) and a file system with
// simulated owners and rwx permissions (vfs.go).

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Shell is the whole interpreter: its input, the logged-in user, its process
// table and the terminal it arbitrates between itself and its jobs.
type Shell struct {
	in    *bufio.Reader // where commands come from
	tin   *bufio.Reader // where passwords come from (the terminal, or stdin)
	out   io.Writer
	errw  io.Writer
	stdin io.Reader // what a built-in reads when nothing is redirected or piped

	fs       *FileSystem
	accounts *Accounts
	jobs     *JobTable

	pid     int       // the shell's own process id
	pgid    int       // the shell's process group; owns the terminal when idle
	started time.Time //

	ttyFD       uintptr        // controlling terminal, when there is one
	interactive bool           // print prompts and messages
	jobControl  bool           // hand the terminal to foreground jobs
	tinIsTTY    bool           // passwords can be read with echo turned off
	script      bool           // commands come from a script file
	sigs        chan os.Signal // terminal signals the shell swallows

	// Session state. A subshell gets a copy of all of it.
	user     *User
	sessions []*savedSession   // users stacked up by su; exit returns to them
	cwd      string            // working directory, a virtual path
	prevDir  string            // $OLDPWD, the target of `cd -`
	env      map[string]string // variables, exported to every child process
	args     []string          // $0, $1, ... inside a script

	lastStatus int // $?, the exit status of the last command

	exiting   bool // `exit` or `logout` was executed
	loggedOut bool // ... and it was `logout`: show the login prompt again
	exitCode  int  //
	exitAsked bool // an exit was refused because jobs were stopped
	subshell  bool // a copy running a pipeline stage or a script
}

// savedSession is what su pushes, so exit can bring the previous user back.
type savedSession struct {
	user    *User
	cwd     string
	prevDir string
	env     map[string]string
}

const hostname = "aos"

// maxLoginAttempts consecutive failures end the login program.
const maxLoginAttempts = 3

// loginFailDelay slows down password guessing. Tests set it to zero.
var loginFailDelay = 2 * time.Second

func defaultDataDir() string {
	if d := os.Getenv("AOS_ROOT"); d != "" {
		return d
	}
	return "aosfs"
}

// runShell is the "shell" subcommand, and what aos does with no arguments.
func runShell(args []string) {
	fl := flag.NewFlagSet("shell", flag.ExitOnError)
	dataDir := fl.String("root", defaultDataDir(), "directory holding the shell's file system (created on first use)")
	user := fl.String("user", "", "log in as this user (the password is still asked for)")
	reset := fl.Bool("reset", false, "delete the file system and start again from the default one")
	fl.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s shell [-root dir] [-user name] [-reset] [script]\n\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(os.Stderr, "Starts the shell. It asks for a user name and password, then reads commands")
		fmt.Fprintln(os.Stderr, "from the terminal, or from the script file when one is given.")
		fmt.Fprintln(os.Stderr)
		fl.PrintDefaults()
	}
	fl.Parse(args)

	if *reset {
		if err := resetDataDir(*dataDir); err != nil {
			fail(err)
		}
	}
	fsys, acct, created, err := openSystem(*dataDir)
	if err != nil {
		fail(err)
	}
	if created {
		fmt.Fprint(os.Stderr, seedBanner(*dataDir))
	}

	sh := newShell(fsys, acct, os.Stdout, os.Stderr)
	sh.ttyFD = os.Stdin.Fd()
	stdin := bufio.NewReader(os.Stdin)
	sh.tin = stdin
	sh.tinIsTTY = isTerminal(os.Stdin)

	if fl.NArg() > 0 {
		f, err := os.Open(fl.Arg(0))
		if err != nil {
			fail(fmt.Errorf("%s: %v", fl.Arg(0), errNoPath(err)))
		}
		defer f.Close()
		sh.in = bufio.NewReader(f)
		sh.script = true
	} else {
		sh.in = stdin
		sh.interactive = sh.tinIsTTY
		sh.jobControl = sh.interactive
	}

	sh.setup()
	os.Exit(sh.Main(*user))
}

// resetDataDir removes a data directory, after checking it looks like one.
func resetDataDir(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "inodes.json")); err != nil {
		return fmt.Errorf("-reset: %s does not look like an aos file system, not deleting it", dir)
	}
	return os.RemoveAll(dir)
}

func newShell(fsys *FileSystem, acct *Accounts, out, errw io.Writer) *Shell {
	sh := &Shell{
		out:      out,
		errw:     errw,
		stdin:    os.Stdin,
		fs:       fsys,
		accounts: acct,
		jobs:     NewJobTable(),
		pid:      os.Getpid(),
		pgid:     syscall.Getpgrp(),
		started:  time.Now(),
		env:      map[string]string{},
		cwd:      "/",
	}
	fsys.audit = func(user, op, vp string, err error) {
		sh.audit("DENIED %s %s %s (%v)", user, op, vp, err)
	}
	return sh
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

	if !sh.jobControl {
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
			fmt.Fprintf(sh.errw, "aos: cannot create a process group: %v\n", err)
			sh.jobControl = false
		}
	}
	sh.pgid = syscall.Getpgrp()
	if sh.jobControl {
		if err := tcsetpgrp(sh.ttyFD, sh.pgid); err != nil {
			fmt.Fprintf(sh.errw, "aos: no job control in this shell: %v\n", err)
			sh.jobControl = false
		}
	}
}

// Main is the login loop: log a user in, run their session, and when they log
// out show the login prompt again, the way getty and login do on a console.
// preset, when not empty, is the user name of the first login.
func (sh *Shell) Main(preset string) int {
	if sh.interactive {
		fmt.Fprintf(sh.out, "AOS shell (pid %d). Log in to continue.\n\n", sh.pid)
	}
	for {
		u, err := sh.login(preset)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			fmt.Fprintf(sh.errw, "aos: %v\n", err)
			return 1
		}
		preset = ""

		sh.startSession(u)
		sh.Run()
		sh.audit("LOGOUT %s", u.Name)
		if !sh.loggedOut || sh.script {
			return sh.exitCode
		}
		sh.loggedOut, sh.exiting = false, false
		if sh.interactive {
			fmt.Fprintln(sh.out)
		}
	}
}

var errTooManyAttempts = errors.New("too many failed login attempts")

// login asks for a user name and password until they match an account, at
// most maxLoginAttempts times.
func (sh *Shell) login(preset string) (*User, error) {
	for attempt := 1; attempt <= maxLoginAttempts; attempt++ {
		name := preset
		if name == "" {
			var err error
			name, err = sh.readLine(fmt.Sprintf("%s login: ", hostname))
			if err != nil {
				if sh.tinIsTTY {
					fmt.Fprintln(sh.out)
				}
				return nil, io.EOF
			}
			name = strings.TrimSpace(name)
			if name == "" {
				attempt--
				continue
			}
		}
		password, err := sh.readPassword("Password: ")
		if err != nil {
			return nil, io.EOF
		}

		u, err := sh.accounts.Authenticate(name, password)
		if err == nil {
			sh.audit("LOGIN %s", name)
			return u, nil
		}
		sh.audit("LOGIN FAILED for %q (attempt %d)", name, attempt)
		if sh.tinIsTTY {
			time.Sleep(loginFailDelay)
		}
		fmt.Fprintf(sh.errw, "%v\n", err)
	}
	sh.audit("LOGIN LOCKED OUT after %d failed attempts", maxLoginAttempts)
	return nil, errTooManyAttempts
}

// startSession sets the shell up for a freshly logged-in user.
func (sh *Shell) startSession(u *User) {
	sh.sessions = nil
	sh.exiting, sh.exitAsked = false, false
	sh.lastStatus = 0
	sh.become(u, true)
	if sh.interactive {
		if motd, err := sh.fs.SystemRead("/etc/motd"); err == nil {
			fmt.Fprintf(sh.out, "\n%s", motd)
		}
		fmt.Fprintf(sh.out, "Logged in as %s (%s).\n\n", u.Name, roleLabel(u.Role))
	}
}

// become switches the shell to user u with a fresh environment. With home set
// the working directory moves to u's home directory (falling back to "/" when
// it is missing or not accessible).
func (sh *Shell) become(u *User, home bool) {
	sh.user = u
	sh.env = baseEnv()
	sh.env["HOME"] = u.Home
	sh.env["USER"] = u.Name
	sh.env["LOGNAME"] = u.Name
	sh.env["SHELL"] = "aos"
	sh.env["HOSTNAME"] = hostname
	if home {
		sh.prevDir = ""
		sh.cwd = "/"
		if sh.fs.CanEnter(u, u.Home) {
			sh.cwd = u.Home
		}
	} else if !sh.fs.CanEnter(u, sh.cwd) {
		sh.cwd = "/"
	}
	sh.env["PWD"] = sh.cwd
}

// baseEnv is the host environment the shell starts from. PATH is what lets the
// shell find external programs; the rest is set per user.
func baseEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for _, k := range []string{"OLDPWD", "MAIL", "SUDO_USER", "SUDO_UID", "SUDO_GID", "SUDO_COMMAND"} {
		delete(env, k)
	}
	return env
}

func roleLabel(r Role) string {
	switch r {
	case RoleAdmin:
		return "administrator"
	case RoleGuest:
		return "guest, read-only"
	default:
		return "standard user"
	}
}

// pushUser starts an su session on top of the current one.
func (sh *Shell) pushUser(u *User, login bool) {
	sh.sessions = append(sh.sessions, &savedSession{
		user: sh.user, cwd: sh.cwd, prevDir: sh.prevDir, env: sh.env,
	})
	sh.become(u, login)
}

// popUser ends an su session and returns to the user who ran su.
func (sh *Shell) popUser() {
	s := sh.sessions[len(sh.sessions)-1]
	sh.sessions = sh.sessions[:len(sh.sessions)-1]
	sh.audit("SU END %s -> back to %s", sh.user.Name, s.user.Name)
	sh.user, sh.cwd, sh.prevDir, sh.env = s.user, s.cwd, s.prevDir, s.env
}

// subshellCopy returns a copy of the shell for a pipeline stage. It shares the
// file system, accounts and process table, but changes to its working
// directory, variables or user do not reach the parent.
func (sh *Shell) subshellCopy() *Shell {
	c := *sh
	c.env = maps.Clone(sh.env)
	c.sessions = slices.Clone(sh.sessions)
	c.subshell = true
	c.interactive = false
	c.jobControl = false
	c.exiting, c.loggedOut = false, false
	return &c
}

// Run is the read-eval-print loop. It returns when the user exits or logs out,
// or the input ends.
func (sh *Shell) Run() {
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
		if err != nil && line == "" {
			// End of input: Ctrl-D, or the end of a script. There is no one
			// left to ask, so the exit cannot be refused - it still goes
			// through `exit` to hang up whatever jobs remain. Inside su it
			// ends every stacked session.
			if sh.interactive {
				fmt.Fprintln(sh.out, "exit")
			}
			for len(sh.sessions) > 0 {
				sh.popUser()
			}
			sh.exitAsked = true
			sh.runLine("exit")
			break
		}

		sh.runLine(line)
		if sh.exiting {
			break
		}
	}
	sh.reap(nil)
}

// runLine parses and executes a single input line.
func (sh *Shell) runLine(line string) {
	p, err := sh.parse(line)
	if err != nil {
		fmt.Fprintf(sh.errw, "aos: %v\n", err)
		sh.lastStatus = 2
		return
	}
	if p == nil {
		return // blank line or comment
	}
	sh.lastStatus = sh.execute(p)
}

// runScript interprets a script file in a subshell: its cd, export and exit
// stay inside it. args[0] is the script name, the rest its arguments.
func (sh *Shell) runScript(vp string, args []string, in io.Reader, out, errw io.Writer) int {
	f, err := sh.fs.Open(sh.user, vp)
	if err != nil {
		fmt.Fprintf(errw, "aos: %s: %v\n", args[0], err)
		return 126
	}
	defer f.Close()

	sub := sh.subshellCopy()
	sub.jobControl = sh.jobControl && !sh.subshell
	sub.in = bufio.NewReader(f)
	sub.stdin, sub.out, sub.errw = in, out, errw
	sub.args = args
	sub.lastStatus = 0
	for {
		line, err := sub.in.ReadString('\n')
		if line != "" {
			sub.runLine(line)
			if sub.exiting {
				return sub.exitCode
			}
		}
		if err != nil {
			return sub.lastStatus
		}
	}
}

// prompt renders "alice@aos:~/somewhere$ ", with the home directory abbreviated
// and '#' instead of '$' for an administrator.
func (sh *Shell) prompt() string {
	wd := sh.cwd
	if home := sh.getenv("HOME"); home != "/" && (wd == home || strings.HasPrefix(wd, home+"/")) {
		wd = "~" + strings.TrimPrefix(wd, home)
	}
	mark := "$"
	if sh.user.Role == RoleAdmin {
		mark = "#"
	}
	return fmt.Sprintf("%s@%s:%s%s ", sh.user.Name, hostname, wd, mark)
}

// readLine prints a prompt (when interactive) and reads one line of input from
// the terminal.
func (sh *Shell) readLine(prompt string) (string, error) {
	if sh.tinIsTTY {
		fmt.Fprint(sh.out, prompt)
	}
	line, err := sh.tin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readPassword reads a password from the terminal with echo turned off, so it
// never appears on the screen. Without a terminal (a script, a test, a pipe) it
// reads the next line of standard input.
func (sh *Shell) readPassword(prompt string) (string, error) {
	if !sh.tinIsTTY {
		return sh.readLine(prompt)
	}
	fmt.Fprint(sh.out, prompt)
	restore, err := disableEcho(sh.ttyFD)
	if err == nil {
		defer restore()
	}
	line, err := sh.tin.ReadString('\n')
	fmt.Fprintln(sh.out)
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// audit appends a line to /var/log/auth.log, which only administrators can
// read.
func (sh *Shell) audit(format string, args ...any) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + fmt.Sprintf(format, args...) + "\n"
	sh.fs.SystemAppend(authLog, line)
}

// abs turns a path the user typed into an absolute virtual path.
func (sh *Shell) abs(p string) string { return sh.fs.Abs(sh.cwd, p) }

func (sh *Shell) getenv(name string) string { return sh.env[name] }

// environ is the environment handed to child processes.
func (sh *Shell) environ() []string {
	env := make([]string, 0, len(sh.env))
	for k, v := range sh.env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

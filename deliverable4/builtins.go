package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Builtin is a command implemented inside the shell process itself. Run as a
// command of its own it changes the shell's state (`cd` has to change *this*
// shell's directory); run as a pipeline stage it gets a subshell copy and reads
// and writes the pipe ends it is handed.
type Builtin func(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int

// builtins is the shell's command table. It is populated in init() because
// several entries (help, for one) refer back to the table itself.
var builtins map[string]Builtin

// helpSections controls the listing printed by `help`.
var helpSections = []struct {
	title string
	names []string
}{
	{"Files", []string{"cd", "pwd", "ls", "cat", "touch", "mkdir", "rmdir", "rm", "cp", "mv", "stat", "chmod", "chown"}},
	{"Text filters (for pipelines)", []string{"echo", "grep", "sort", "uniq", "wc", "head", "tail"}},
	{"Users and security", []string{"whoami", "id", "users", "who", "su", "logout", "passwd", "useradd", "userdel"}},
	{"Processes", []string{"jobs", "fg", "bg", "kill", "ps"}},
	{"Shell", []string{"export", "env", "clear", "help", "exit"}},
}

func init() {
	builtins = map[string]Builtin{
		"cd": builtinCd, "pwd": builtinPwd, "exit": builtinExit, "echo": builtinEcho,
		"clear": builtinClear, "ls": builtinLs, "cat": builtinCat, "mkdir": builtinMkdir,
		"rmdir": builtinRmdir, "rm": builtinRm, "touch": builtinTouch, "cp": builtinCp,
		"mv": builtinMv, "kill": builtinKill, "jobs": builtinJobs, "fg": builtinFg,
		"bg": builtinBg, "ps": builtinPs, "export": builtinExport, "env": builtinEnv,
		"help": builtinHelp,

		"grep": builtinGrep, "sort": builtinSort, "uniq": builtinUniq, "wc": builtinWc,
		"head": builtinHead, "tail": builtinTail,

		"whoami": builtinWhoami, "id": builtinId, "users": builtinUsers, "who": builtinWho,
		"su": builtinSu, "logout": builtinLogout, "passwd": builtinPasswd,
		"useradd": builtinUseradd, "userdel": builtinUserdel,
		"chmod": builtinChmod, "chown": builtinChown, "stat": builtinStat,
	}
}

var builtinHelpText = map[string]string{
	"cd":      "cd [dir]                  change the working directory (no argument: $HOME, '-': previous)",
	"pwd":     "pwd                       print the working directory",
	"exit":    "exit [status]             leave the shell (inside su: return to the previous user)",
	"echo":    "echo [-n] [text ...]      write text to standard output",
	"clear":   "clear                     clear the terminal screen",
	"ls":      "ls [-l] [-a] [path ...]   list directory contents (-l shows owner, group and mode)",
	"cat":     "cat [file ...]            print files (no argument: copy standard input)",
	"mkdir":   "mkdir [-p] dir ...        create directories",
	"rmdir":   "rmdir dir ...             remove empty directories",
	"rm":      "rm [-r] [-f] file ...     remove files, -r for directories",
	"touch":   "touch file ...            create a file or update its timestamp",
	"cp":      "cp src ... dest           copy files (the copy belongs to you)",
	"mv":      "mv src ... dest           move or rename files",
	"stat":    "stat file ...             show owner, group, mode and size",
	"chmod":   "chmod mode file ...       change permissions: 640, u+x, go-w, a=r (owner or admin)",
	"chown":   "chown user[:group] file   change the owner (administrators only)",
	"grep":    "grep [-ivnc] pattern [file ...]  print lines matching a regular expression",
	"sort":    "sort [-rnu] [file ...]    sort lines (-r reverse, -n numeric, -u unique)",
	"uniq":    "uniq [-c] [file]          collapse repeated adjacent lines (-c count them)",
	"wc":      "wc [-lwc] [file ...]      count lines, words and bytes",
	"head":    "head [-n N] [file ...]    first N lines (default 10)",
	"tail":    "tail [-n N] [file ...]    last N lines (default 10)",
	"whoami":  "whoami                    print the current user name",
	"id":      "id [user]                 print uid, group and role",
	"users":   "users                     list the accounts",
	"who":     "who                       show the login session and any su sessions on top",
	"su":      "su [-] [user]             become another user (default admin); '-' starts in their home",
	"logout":  "logout                    end the session and return to the login prompt",
	"passwd":  "passwd [user]             change your password (admins: anyone's)",
	"useradd": "useradd [-r role] name    create an account: role admin, standard or guest (admins)",
	"userdel": "userdel [-r] name         delete an account, -r also its home directory (admins)",
	"kill":    "kill [-SIG] pid|%job      send a signal to one of your jobs (default TERM)",
	"jobs":    "jobs [-l]                 list the background and stopped jobs",
	"fg":      "fg [%job]                 resume a job in the foreground",
	"bg":      "bg [%job]                 resume a stopped job in the background",
	"ps":      "ps                        show the shell's process table",
	"export":  "export NAME=value         set an environment variable for child processes",
	"env":     "env                       list the environment",
	"help":    "help [command]            describe the built-in commands",
}

// ---------------------------------------------------------------------------
// Directory and environment
// ---------------------------------------------------------------------------

func builtinCd(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	target := sh.getenv("HOME")
	switch {
	case len(args) > 2:
		fmt.Fprintln(errw, "cd: too many arguments")
		return 1
	case len(args) == 2 && args[1] == "-":
		if sh.prevDir == "" {
			fmt.Fprintln(errw, "cd: OLDPWD not set")
			return 1
		}
		target = sh.prevDir
		fmt.Fprintln(out, target)
	case len(args) == 2:
		target = args[1]
	}

	vp := sh.abs(target)
	if err := sh.fs.Chdir(sh.user, vp); err != nil {
		fmt.Fprintf(errw, "cd: %s: %v\n", target, err)
		return 1
	}
	sh.prevDir, sh.cwd = sh.cwd, vp
	sh.env["OLDPWD"] = sh.prevDir
	sh.env["PWD"] = sh.cwd
	return 0
}

func builtinPwd(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	fmt.Fprintln(out, sh.cwd)
	return 0
}

func builtinExit(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	code := sh.lastStatus
	if len(args) > 1 {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintf(errw, "exit: %s: numeric argument required\n", args[1])
			return 1
		}
		code = n & 0xff
	}
	if sh.subshell {
		// Ends a script or pipeline stage, never the shell around it.
		sh.exiting, sh.exitCode = true, code
		return code
	}
	if len(sh.sessions) > 0 {
		sh.popUser()
		return 0
	}
	if !sh.leaveSession(errw, "exit") {
		return 1
	}
	sh.exitCode = code
	return code
}

// leaveSession ends the login session: it refuses once while jobs are stopped,
// the way an interactive shell does, and then hangs up every job.
func (sh *Shell) leaveSession(errw io.Writer, name string) bool {
	if !sh.exitAsked {
		for _, j := range sh.jobs.list() {
			if j.State == StateStopped {
				fmt.Fprintf(errw, "%s: there are stopped jobs\n", name)
				sh.exitAsked = true
				return false
			}
		}
	}

	// Nothing may be left behind: every job still in the table gets SIGHUP,
	// which is what a kernel-backed session leader does on the way out.
	for _, j := range sh.jobs.list() {
		syscall.Kill(-j.PGID, syscall.SIGHUP)
		syscall.Kill(-j.PGID, syscall.SIGCONT)
	}
	sh.exiting = true
	return true
}

func builtinExport(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) == 1 {
		return builtinEnv(sh, args, in, out, errw)
	}
	status := 0
	for _, arg := range args[1:] {
		name, value, ok := strings.Cut(arg, "=")
		if !ok || name == "" {
			fmt.Fprintf(errw, "export: %s: not a valid assignment\n", arg)
			status = 1
			continue
		}
		switch name {
		case "USER", "LOGNAME", "HOME":
			// The shell decides who you are from your login, never from a
			// variable, but programs read these: do not let them lie.
			fmt.Fprintf(errw, "export: %s: read-only variable\n", name)
			status = 1
			continue
		}
		sh.env[name] = value
	}
	return status
}

func builtinEnv(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	for _, e := range sh.environ() {
		fmt.Fprintln(out, e)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

func builtinEcho(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	args = args[1:]
	newline := true
	if len(args) > 0 && args[0] == "-n" {
		newline = false
		args = args[1:]
	}
	fmt.Fprint(out, strings.Join(args, " "))
	if newline {
		fmt.Fprintln(out)
	}
	return 0
}

// builtinClear emits the ANSI sequences that move the cursor home, erase the
// screen and drop the scrollback buffer.
func builtinClear(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	fmt.Fprint(out, "\033[H\033[2J\033[3J")
	return 0
}

func builtinCat(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	return eachInput(sh, "cat", args[1:], in, errw, func(name string, r io.Reader) {
		io.Copy(out, r)
	})
}

// eachInput calls f for each file operand, opened with a read-permission check,
// or once for standard input when there are none ("-" also means standard
// input). It returns 1 if any operand could not be read.
func eachInput(sh *Shell, cmd string, operands []string, in io.Reader, errw io.Writer, f func(name string, r io.Reader)) int {
	if len(operands) == 0 {
		f("", in)
		return 0
	}
	status := 0
	for _, name := range operands {
		if name == "-" {
			f(name, in)
			continue
		}
		file, err := sh.fs.Open(sh.user, sh.abs(name))
		if err != nil {
			fmt.Fprintf(errw, "%s: %s: %v\n", cmd, name, err)
			status = 1
			continue
		}
		f(name, file)
		file.Close()
	}
	return status
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

func builtinLs(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	long, all, paths, status := false, false, []string(nil), 0
	for _, a := range args[1:] {
		if len(a) > 1 && a[0] == '-' {
			for _, flag := range a[1:] {
				switch flag {
				case 'l':
					long = true
				case 'a':
					all = true
				default:
					fmt.Fprintf(errw, "ls: invalid option -- '%c'\n", flag)
					return 1
				}
			}
			continue
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	// Like ls(1): columns for a person at a terminal, one name per line for a
	// pipe, so that `ls | grep txt` filters names rather than rows of names.
	columns := false
	if f, ok := out.(*os.File); ok && isTerminal(f) {
		columns = true
	}

	for i, p := range paths {
		vp := sh.abs(p)
		e, err := sh.fs.Stat(sh.user, vp)
		if err != nil {
			fmt.Fprintf(errw, "ls: %s: %v\n", p, err)
			status = 1
			continue
		}

		// A plain file is simply echoed back, like ls(1) does.
		if !e.Info.IsDir() {
			if long {
				fmt.Fprintln(out, longFormat(e, p))
			} else {
				fmt.Fprintln(out, p)
			}
			continue
		}

		entries, err := sh.fs.ReadDir(sh.user, vp)
		if err != nil {
			fmt.Fprintf(errw, "ls: %s: %v\n", p, err)
			status = 1
			continue
		}
		if len(paths) > 1 {
			if i > 0 {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "%s:\n", p)
		}

		var shown []*Entry
		for _, e := range entries {
			if all || !strings.HasPrefix(path.Base(e.Path), ".") {
				shown = append(shown, e)
			}
		}
		sort.Slice(shown, func(a, b int) bool { return shown[a].Path < shown[b].Path })

		switch {
		case long:
			for _, e := range shown {
				fmt.Fprintln(out, longFormat(e, path.Base(e.Path)))
			}
		case columns:
			names := make([]string, len(shown))
			for i, e := range shown {
				names[i] = path.Base(e.Path)
			}
			printColumns(out, names)
		default:
			for _, e := range shown {
				fmt.Fprintln(out, path.Base(e.Path))
			}
		}
	}
	return status
}

// longFormat renders one `ls -l` line: the simulated mode, owner and group,
// then size and modification time.
//
//	-rw-r-----  alice    users         42 Oct  5 19:10 notes.txt
func longFormat(e *Entry, name string) string {
	info := e.Info
	stamp := info.ModTime().Format("Jan _2 15:04")
	if time.Since(info.ModTime()) > 180*24*time.Hour {
		stamp = info.ModTime().Format("Jan _2  2006")
	}
	display := name
	if info.IsDir() && !strings.HasSuffix(display, "/") {
		display += "/"
	}
	return fmt.Sprintf("%s  %-8s %-8s %8d %s %s",
		modeString(info, e.Inode.Mode), e.Inode.Owner, e.Inode.Group, info.Size(), stamp, display)
}

// printColumns lays names out down-then-across inside an 80 column terminal,
// the way ls(1) does when its output is a terminal.
func printColumns(out io.Writer, names []string) {
	if len(names) == 0 {
		return
	}

	width := 0
	for _, n := range names {
		if len(n) > width {
			width = len(n)
		}
	}
	width += 2

	cols := 80 / width
	if cols < 1 {
		cols = 1
	}
	rows := (len(names) + cols - 1) / cols

	for r := 0; r < rows; r++ {
		var line strings.Builder
		for c := 0; c < cols; c++ {
			i := c*rows + r
			if i >= len(names) {
				break
			}
			fmt.Fprintf(&line, "%-*s", width, names[i])
		}
		fmt.Fprintln(out, strings.TrimRight(line.String(), " "))
	}
}

// ---------------------------------------------------------------------------
// File system mutation
// ---------------------------------------------------------------------------

func builtinMkdir(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	parents, dirs := false, []string(nil)
	for _, a := range args[1:] {
		if a == "-p" {
			parents = true
			continue
		}
		dirs = append(dirs, a)
	}
	if len(dirs) == 0 {
		fmt.Fprintln(errw, "mkdir: missing operand")
		return 1
	}

	status := 0
	for _, d := range dirs {
		var err error
		if parents {
			err = sh.fs.MkdirAll(sh.user, sh.abs(d))
		} else {
			err = sh.fs.Mkdir(sh.user, sh.abs(d))
		}
		if err != nil {
			fmt.Fprintf(errw, "mkdir: %s: %v\n", d, err)
			status = 1
		}
	}
	return status
}

func builtinRmdir(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) == 1 {
		fmt.Fprintln(errw, "rmdir: missing operand")
		return 1
	}
	status := 0
	for _, d := range args[1:] {
		if err := sh.fs.Remove(sh.user, sh.abs(d), true); err != nil {
			fmt.Fprintf(errw, "rmdir: %s: %v\n", d, err)
			status = 1
		}
	}
	return status
}

func builtinRm(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	recursive, force, targets := false, false, []string(nil)
	for _, a := range args[1:] {
		if len(a) > 1 && a[0] == '-' {
			for _, flag := range a[1:] {
				switch flag {
				case 'r', 'R':
					recursive = true
				case 'f':
					force = true
				default:
					fmt.Fprintf(errw, "rm: invalid option -- '%c'\n", flag)
					return 1
				}
			}
			continue
		}
		targets = append(targets, a)
	}
	if len(targets) == 0 {
		if force {
			return 0
		}
		fmt.Fprintln(errw, "rm: missing operand")
		return 1
	}

	status := 0
	for _, t := range targets {
		vp := sh.abs(t)
		var err error
		if recursive {
			err = sh.fs.RemoveAll(sh.user, vp)
		} else {
			err = sh.fs.Remove(sh.user, vp, false)
		}
		if err != nil {
			if force && errors.Is(err, ErrNotExist) {
				continue
			}
			fmt.Fprintf(errw, "rm: %s: %v\n", t, err)
			status = 1
		}
	}
	return status
}

func builtinTouch(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) == 1 {
		fmt.Fprintln(errw, "touch: missing operand")
		return 1
	}
	status := 0
	for _, name := range args[1:] {
		if err := sh.fs.Touch(sh.user, sh.abs(name)); err != nil {
			fmt.Fprintf(errw, "touch: %s: %v\n", name, err)
			status = 1
		}
	}
	return status
}

// builtinCp copies files. Reading the source takes read permission, and the
// copy is a new file owned by whoever made it - so copying a readable system
// file into your home directory gives you your own copy, not the original.
func builtinCp(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) < 3 {
		fmt.Fprintln(errw, "cp: usage: cp src ... dest")
		return 1
	}
	srcs, dest := args[1:len(args)-1], args[len(args)-1]
	destVP := sh.abs(dest)
	destIsDir := false
	if e, err := sh.fs.Stat(sh.user, destVP); err == nil && e.Info.IsDir() {
		destIsDir = true
	}
	if len(srcs) > 1 && !destIsDir {
		fmt.Fprintf(errw, "cp: %s: %v\n", dest, ErrNotDir)
		return 1
	}

	status := 0
	for _, s := range srcs {
		target := destVP
		if destIsDir {
			target = path.Join(destVP, path.Base(sh.abs(s)))
		}
		if err := copyFile(sh, sh.abs(s), target); err != nil {
			fmt.Fprintf(errw, "cp: %v\n", err)
			status = 1
		}
	}
	return status
}

func copyFile(sh *Shell, from, to string) error {
	if from == to {
		return fmt.Errorf("%s and %s are the same file", from, to)
	}
	src, err := sh.fs.Open(sh.user, from)
	if err != nil {
		return fmt.Errorf("%s: %v", from, err)
	}
	defer src.Close()
	dst, err := sh.fs.Create(sh.user, to, false)
	if err != nil {
		return fmt.Errorf("%s: %v", to, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return fmt.Errorf("%s: %v", to, err)
	}
	return dst.Close()
}

func builtinMv(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) < 3 {
		fmt.Fprintln(errw, "mv: usage: mv src ... dest")
		return 1
	}
	srcs, dest := args[1:len(args)-1], args[len(args)-1]
	if len(srcs) > 1 {
		if e, err := sh.fs.Stat(sh.user, sh.abs(dest)); err != nil || !e.Info.IsDir() {
			fmt.Fprintf(errw, "mv: %s: %v\n", dest, ErrNotDir)
			return 1
		}
	}
	status := 0
	for _, s := range srcs {
		if err := sh.fs.Rename(sh.user, sh.abs(s), sh.abs(dest)); err != nil {
			fmt.Fprintf(errw, "mv: %s: %v\n", s, err)
			status = 1
		}
	}
	return status
}

// ---------------------------------------------------------------------------
// Signals
// ---------------------------------------------------------------------------

// signalNames maps the names kill accepts to their numbers.
var signalNames = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT,
	"KILL": syscall.SIGKILL, "TERM": syscall.SIGTERM, "STOP": syscall.SIGSTOP,
	"TSTP": syscall.SIGTSTP, "CONT": syscall.SIGCONT, "USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2, "ALRM": syscall.SIGALRM, "PIPE": syscall.SIGPIPE,
}

func signalName(s syscall.Signal) string {
	for name, sig := range signalNames {
		if sig == s {
			return "SIG" + name
		}
	}
	return s.String()
}

// builtinKill sends a signal to a process, or to every process of a job when
// the target is written as %n. Only jobs started from this shell can be
// signalled, and a standard user only their own: the host processes behind
// the shell all belong to one host user, so without this check alice could
// kill bob's jobs - or anything else that host user runs.
func builtinKill(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	sig, targets := syscall.SIGTERM, []string(nil)

	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-l":
			names := make([]string, 0, len(signalNames))
			for n, s := range signalNames {
				names = append(names, fmt.Sprintf("%2d) SIG%s", int(s), n))
			}
			sort.Strings(names)
			fmt.Fprintln(out, strings.Join(names, "  "))
			return 0
		case a == "-s" && i+1 < len(args):
			i++
			s, err := parseSignal(args[i])
			if err != nil {
				fmt.Fprintf(errw, "kill: %v\n", err)
				return 1
			}
			sig = s
		case len(a) > 1 && a[0] == '-':
			s, err := parseSignal(a[1:])
			if err != nil {
				fmt.Fprintf(errw, "kill: %v\n", err)
				return 1
			}
			sig = s
		default:
			targets = append(targets, a)
		}
	}

	if len(targets) == 0 {
		fmt.Fprintln(errw, "kill: usage: kill [-signal] pid | %job")
		return 1
	}

	status := 0
	for _, t := range targets {
		var j *Job
		target := 0
		if strings.HasPrefix(t, "%") {
			// %n addresses a whole job: the signal goes to the process group,
			// so every process of the pipeline receives it.
			found, err := sh.jobs.find(t)
			if err != nil {
				fmt.Fprintf(errw, "kill: %v\n", err)
				status = 1
				continue
			}
			j, target = found, -found.PGID
		} else {
			pid, err := strconv.Atoi(t)
			if err != nil {
				fmt.Fprintf(errw, "kill: %s: arguments must be process ids or job specs\n", t)
				status = 1
				continue
			}
			if j, _ = sh.jobs.byPID(pid); j == nil {
				fmt.Fprintf(errw, "kill: (%d): Operation not permitted (not a job of this shell)\n", pid)
				status = 1
				continue
			}
			target = pid
		}

		if err := sh.mayControl(j); err != nil {
			fmt.Fprintf(errw, "kill: %v\n", err)
			status = 1
			continue
		}
		if err := syscall.Kill(target, sig); err != nil {
			fmt.Fprintf(errw, "kill: %s: %v\n", t, err)
			status = 1
			continue
		}
		// A stopped process cannot act on a pending signal until it runs.
		if sig != syscall.SIGSTOP && sig != syscall.SIGTSTP && j.State == StateStopped {
			syscall.Kill(target, syscall.SIGCONT)
		}
	}
	return status
}

// parseSignal accepts "9", "KILL" or "SIGKILL".
func parseSignal(spec string) (syscall.Signal, error) {
	if n, err := strconv.Atoi(spec); err == nil {
		return syscall.Signal(n), nil
	}
	name := strings.TrimPrefix(strings.ToUpper(spec), "SIG")
	if sig, ok := signalNames[name]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("%s: invalid signal specification", spec)
}

// ---------------------------------------------------------------------------
// Help
// ---------------------------------------------------------------------------

func builtinHelp(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) > 1 {
		status := 0
		for _, name := range args[1:] {
			text, ok := builtinHelpText[name]
			if !ok {
				if text, ok = programHelpText[name]; !ok {
					fmt.Fprintf(errw, "help: no help topic for '%s'\n", name)
					status = 1
					continue
				}
			}
			fmt.Fprintln(out, text)
		}
		return status
	}

	fmt.Fprintln(out, "AOS shell commands:")
	for _, s := range helpSections {
		fmt.Fprintf(out, "\n%s:\n", s.title)
		for _, name := range s.names {
			fmt.Fprintf(out, "  %s\n", builtinHelpText[name])
		}
	}
	fmt.Fprintf(out, "\nOperating system simulations (run as programs, so they can be piped):\n")
	for _, name := range []string{"sched", "mem", "sync"} {
		fmt.Fprintf(out, "  %s\n", programHelpText[name])
	}
	fmt.Fprintln(out, "\nJoin commands with '|' to pipe the output of one into the next:")
	fmt.Fprintln(out, "  ls | grep txt        cat /var/log/app.log | grep error | sort | uniq -c")
	fmt.Fprintln(out, "Redirect with < > >>, append '&' to run in the background; Ctrl-Z stops the")
	fmt.Fprintln(out, "foreground job, 'bg' resumes it in the background and 'fg' brings it back.")
	switch sh.user.Role {
	case RoleAdmin:
		fmt.Fprintln(out, "\nAs an administrator you can run any program in $PATH.")
	case RoleStandard:
		fmt.Fprintf(out, "\nStandard users can also run these host programs: %s\n", strings.Join(sortedKeys(standardPrograms), " "))
	default:
		fmt.Fprintln(out, "\nGuest accounts are read-only and cannot run host programs.")
	}
	return 0
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

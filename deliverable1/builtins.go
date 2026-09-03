package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Builtin is a command implemented inside the shell process itself. Built-ins
// are never forked: `cd` has to change *this* process's working directory, and
// `exit` has to terminate *this* process, which a child could not do.
type Builtin func(sh *Shell, args []string, out, errw io.Writer) int

// builtins is the shell's command table. It is populated in init() because
// several entries (help, for one) refer back to the table itself.
var builtins map[string]Builtin

// order controls the listing printed by `help`.
var builtinOrder = []string{
	"cd", "pwd", "exit", "echo", "clear", "ls", "cat",
	"mkdir", "rmdir", "rm", "touch", "kill",
	"jobs", "fg", "bg", "ps", "export", "env", "help",
}

func init() {
	builtins = map[string]Builtin{
		"cd":     builtinCd,
		"pwd":    builtinPwd,
		"exit":   builtinExit,
		"echo":   builtinEcho,
		"clear":  builtinClear,
		"ls":     builtinLs,
		"cat":    builtinCat,
		"mkdir":  builtinMkdir,
		"rmdir":  builtinRmdir,
		"rm":     builtinRm,
		"touch":  builtinTouch,
		"kill":   builtinKill,
		"jobs":   builtinJobs,
		"fg":     builtinFg,
		"bg":     builtinBg,
		"ps":     builtinPs,
		"export": builtinExport,
		"env":    builtinEnv,
		"help":   builtinHelp,
	}
}

var builtinHelpText = map[string]string{
	"cd":     "cd [dir]              change the working directory (no argument: $HOME, '-': previous)",
	"pwd":    "pwd                   print the working directory",
	"exit":   "exit [status]         terminate the shell",
	"echo":   "echo [-n] [text ...]  write text to standard output",
	"clear":  "clear                 clear the terminal screen",
	"ls":     "ls [-l] [-a] [path]   list directory contents",
	"cat":    "cat [file ...]        print files (no argument: copy standard input)",
	"mkdir":  "mkdir [-p] dir ...    create directories",
	"rmdir":  "rmdir dir ...         remove empty directories",
	"rm":     "rm [-r] [-f] file ... remove files, -r for directories",
	"touch":  "touch file ...        create a file or update its timestamp",
	"kill":   "kill [-SIG] pid|%job  send a signal to a process (default TERM)",
	"jobs":   "jobs [-l]             list the background and stopped jobs",
	"fg":     "fg [%job]             resume a job in the foreground",
	"bg":     "bg [%job]             resume a stopped job in the background",
	"ps":     "ps                    show the shell's process table",
	"export": "export NAME=value     set an environment variable for child processes",
	"env":    "env                   list the environment",
	"help":   "help [command]        describe the built-in commands",
}

// ---------------------------------------------------------------------------
// Directory and environment
// ---------------------------------------------------------------------------

func builtinCd(sh *Shell, args []string, out, errw io.Writer) int {
	target := homeDir()
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

	current, _ := os.Getwd()
	if err := os.Chdir(target); err != nil {
		fmt.Fprintf(errw, "cd: %s: %v\n", target, errNoPath(err))
		return 1
	}

	sh.prevDir = current
	wd, _ := os.Getwd()
	os.Setenv("OLDPWD", current)
	os.Setenv("PWD", wd)
	return 0
}

func builtinPwd(sh *Shell, args []string, out, errw io.Writer) int {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(errw, "pwd: %v\n", err)
		return 1
	}
	fmt.Fprintln(out, wd)
	return 0
}

func builtinExit(sh *Shell, args []string, out, errw io.Writer) int {
	code := sh.lastStatus
	if len(args) > 1 {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintf(errw, "exit: %s: numeric argument required\n", args[1])
			return 1
		}
		code = n & 0xff
	}

	// Refuse to walk away from stopped children on the first attempt, the way
	// an interactive shell does; a second exit goes through regardless.
	if !sh.exitAsked {
		for _, j := range sh.jobs.list() {
			if j.State == StateStopped {
				fmt.Fprintln(errw, "exit: there are stopped jobs")
				sh.exitAsked = true
				return 1
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
	sh.exitCode = code
	return code
}

func builtinExport(sh *Shell, args []string, out, errw io.Writer) int {
	if len(args) == 1 {
		return builtinEnv(sh, args, out, errw)
	}
	status := 0
	for _, arg := range args[1:] {
		name, value, ok := strings.Cut(arg, "=")
		if !ok || name == "" {
			fmt.Fprintf(errw, "export: %s: not a valid assignment\n", arg)
			status = 1
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintf(errw, "export: %s: %v\n", name, err)
			status = 1
		}
	}
	return status
}

func builtinEnv(sh *Shell, args []string, out, errw io.Writer) int {
	env := os.Environ()
	sort.Strings(env)
	for _, e := range env {
		fmt.Fprintln(out, e)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

func builtinEcho(sh *Shell, args []string, out, errw io.Writer) int {
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
func builtinClear(sh *Shell, args []string, out, errw io.Writer) int {
	fmt.Fprint(out, "\033[H\033[2J\033[3J")
	return 0
}

func builtinCat(sh *Shell, args []string, out, errw io.Writer) int {
	if len(args) == 1 {
		// No operand: copy standard input, which '<' may have redirected.
		io.Copy(out, sh.stdin)
		return 0
	}

	status := 0
	for _, name := range args[1:] {
		f, err := os.Open(name)
		if err != nil {
			fmt.Fprintf(errw, "cat: %s: %v\n", name, errNoPath(err))
			status = 1
			continue
		}
		info, err := f.Stat()
		if err == nil && info.IsDir() {
			fmt.Fprintf(errw, "cat: %s: is a directory\n", name)
			status = 1
			f.Close()
			continue
		}
		if _, err := io.Copy(out, f); err != nil {
			fmt.Fprintf(errw, "cat: %s: %v\n", name, err)
			status = 1
		}
		f.Close()
	}
	return status
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

func builtinLs(sh *Shell, args []string, out, errw io.Writer) int {
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

	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(errw, "ls: %s: %v\n", path, errNoPath(err))
			status = 1
			continue
		}

		// A plain file is simply echoed back, like ls(1) does.
		if !info.IsDir() {
			if long {
				fmt.Fprintln(out, longFormat(info, path))
			} else {
				fmt.Fprintln(out, path)
			}
			continue
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			fmt.Fprintf(errw, "ls: %s: %v\n", path, errNoPath(err))
			status = 1
			continue
		}
		if len(paths) > 1 {
			if i > 0 {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "%s:\n", path)
		}

		names := []string{}
		if all {
			names = append(names, ".", "..")
		}
		for _, e := range entries {
			if !all && strings.HasPrefix(e.Name(), ".") {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)

		if long {
			for _, n := range names {
				fi, err := os.Lstat(filepath.Join(path, n))
				if err != nil {
					fmt.Fprintf(errw, "ls: %s: %v\n", n, errNoPath(err))
					status = 1
					continue
				}
				fmt.Fprintln(out, longFormat(fi, n))
			}
			continue
		}
		printColumns(out, names)
	}
	return status
}

// longFormat renders one `ls -l` line: permissions, size, modification time.
func longFormat(info os.FileInfo, name string) string {
	stamp := info.ModTime().Format("Jan _2 15:04")
	if time.Since(info.ModTime()) > 180*24*time.Hour {
		stamp = info.ModTime().Format("Jan _2  2006")
	}
	display := filepath.Base(name)
	if info.IsDir() {
		display += "/"
	}
	return fmt.Sprintf("%s %8d %s %s", info.Mode().String(), info.Size(), stamp, display)
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

func builtinMkdir(sh *Shell, args []string, out, errw io.Writer) int {
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
			err = os.MkdirAll(d, 0o755)
		} else {
			err = os.Mkdir(d, 0o755)
		}
		if err != nil {
			fmt.Fprintf(errw, "mkdir: %s: %v\n", d, errNoPath(err))
			status = 1
		}
	}
	return status
}

func builtinRmdir(sh *Shell, args []string, out, errw io.Writer) int {
	if len(args) == 1 {
		fmt.Fprintln(errw, "rmdir: missing operand")
		return 1
	}

	status := 0
	for _, d := range args[1:] {
		info, err := os.Stat(d)
		if err != nil {
			fmt.Fprintf(errw, "rmdir: %s: %v\n", d, errNoPath(err))
			status = 1
			continue
		}
		if !info.IsDir() {
			fmt.Fprintf(errw, "rmdir: %s: not a directory\n", d)
			status = 1
			continue
		}
		// os.Remove refuses a non-empty directory, which is exactly rmdir(1).
		if err := os.Remove(d); err != nil {
			fmt.Fprintf(errw, "rmdir: %s: directory not empty\n", d)
			status = 1
		}
	}
	return status
}

func builtinRm(sh *Shell, args []string, out, errw io.Writer) int {
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
		info, err := os.Lstat(t)
		if err != nil {
			if !force {
				fmt.Fprintf(errw, "rm: %s: %v\n", t, errNoPath(err))
				status = 1
			}
			continue
		}
		if info.IsDir() && !recursive {
			fmt.Fprintf(errw, "rm: %s: is a directory\n", t)
			status = 1
			continue
		}

		if recursive {
			err = os.RemoveAll(t)
		} else {
			err = os.Remove(t)
		}
		if err != nil && !force {
			fmt.Fprintf(errw, "rm: %s: %v\n", t, errNoPath(err))
			status = 1
		}
	}
	return status
}

func builtinTouch(sh *Shell, args []string, out, errw io.Writer) int {
	if len(args) == 1 {
		fmt.Fprintln(errw, "touch: missing operand")
		return 1
	}

	status, now := 0, time.Now()
	for _, name := range args[1:] {
		if _, err := os.Stat(name); err == nil {
			// The file exists: only its access and modification times change.
			if err := os.Chtimes(name, now, now); err != nil {
				fmt.Fprintf(errw, "touch: %s: %v\n", name, errNoPath(err))
				status = 1
			}
			continue
		}
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(errw, "touch: %s: %v\n", name, errNoPath(err))
			status = 1
			continue
		}
		f.Close()
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

// builtinKill sends a signal to a process id, or to every process in a job when
// the target is written as %n.
func builtinKill(sh *Shell, args []string, out, errw io.Writer) int {
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
		// %n addresses a whole job: the signal goes to the process group, so
		// every process the job created receives it.
		if strings.HasPrefix(t, "%") {
			j, err := sh.jobs.find(t)
			if err != nil {
				fmt.Fprintf(errw, "kill: %v\n", err)
				status = 1
				continue
			}
			if err := syscall.Kill(-j.PGID, sig); err != nil {
				fmt.Fprintf(errw, "kill: %s: %v\n", t, err)
				status = 1
				continue
			}
			// A stopped process cannot act on a pending signal until it runs.
			if sig != syscall.SIGSTOP && sig != syscall.SIGTSTP && j.State == StateStopped {
				syscall.Kill(-j.PGID, syscall.SIGCONT)
			}
			continue
		}

		pid, err := strconv.Atoi(t)
		if err != nil {
			fmt.Fprintf(errw, "kill: %s: arguments must be process ids or job specs\n", t)
			status = 1
			continue
		}
		if err := syscall.Kill(pid, sig); err != nil {
			fmt.Fprintf(errw, "kill: (%d): %v\n", pid, err)
			status = 1
			continue
		}
		if j := sh.jobs.byPID(pid); j != nil && j.State == StateStopped &&
			sig != syscall.SIGSTOP && sig != syscall.SIGTSTP {
			syscall.Kill(pid, syscall.SIGCONT)
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

func builtinHelp(sh *Shell, args []string, out, errw io.Writer) int {
	if len(args) > 1 {
		status := 0
		for _, name := range args[1:] {
			text, ok := builtinHelpText[name]
			if !ok {
				fmt.Fprintf(errw, "help: no help topic for '%s'\n", name)
				status = 1
				continue
			}
			fmt.Fprintln(out, text)
		}
		return status
	}

	fmt.Fprintln(out, "gosh built-in commands:")
	for _, name := range builtinOrder {
		fmt.Fprintf(out, "  %s\n", builtinHelpText[name])
	}
	fmt.Fprintln(out, "\nAnything else is looked up in $PATH and run in a new process.")
	fmt.Fprintln(out, "Append '&' to run in the background; Ctrl-Z stops the foreground job,")
	fmt.Fprintln(out, "'bg' resumes it in the background and 'fg' brings it back.")
	return 0
}

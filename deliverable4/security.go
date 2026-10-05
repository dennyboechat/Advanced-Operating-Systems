package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Security commands: who you are, becoming someone else, managing accounts,
// changing permissions - and which programs each kind of user may run.

func builtinWhoami(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	fmt.Fprintln(out, sh.user.Name)
	return 0
}

func builtinId(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	u := sh.user
	if len(args) > 1 {
		found, err := sh.accounts.Lookup(args[1])
		if err != nil {
			fmt.Fprintf(errw, "id: %v\n", err)
			return 1
		}
		u = found
	}
	fmt.Fprintf(out, "uid=%d(%s) gid=%s role=%s home=%s\n", u.UID, u.Name, u.Group, u.Role, u.Home)
	return 0
}

func builtinUsers(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	users, err := sh.accounts.List()
	if err != nil {
		fmt.Fprintf(errw, "users: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "%-10s %5s  %-7s %-9s %s\n", "USER", "UID", "GROUP", "ROLE", "HOME")
	for _, u := range users {
		fmt.Fprintf(out, "%-10s %5d  %-7s %-9s %s\n", u.Name, u.UID, u.Group, u.Role, u.Home)
	}
	return 0
}

// builtinWho shows the login session and the su sessions stacked on top of it.
func builtinWho(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	for i, s := range sh.sessions {
		how := "su"
		if i == 0 {
			how = "login"
		}
		fmt.Fprintf(out, "%-10s %-6s %s\n", s.user.Name, how, s.user.Role)
	}
	how := "su"
	if len(sh.sessions) == 0 {
		how = "login"
	}
	fmt.Fprintf(out, "%-10s %-6s %s   <- current\n", sh.user.Name, how, sh.user.Role)
	return 0
}

// builtinSu runs a session as another user on top of the current one, until
// exit. It asks for the *target* user's password, except when an administrator
// runs it. With "-" the new session starts in the target's home directory.
func builtinSu(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	login, name := false, "admin"
	for _, a := range args[1:] {
		switch a {
		case "-", "-l", "--login":
			login = true
		default:
			name = a
		}
	}

	target, err := sh.accounts.Lookup(name)
	if err != nil {
		fmt.Fprintf(errw, "su: %v\n", err)
		return 1
	}
	if sh.user.Role != RoleAdmin {
		password, err := sh.readPassword("Password: ")
		if err != nil {
			fmt.Fprintln(errw, "su: no password given")
			return 1
		}
		if _, err := sh.accounts.Authenticate(name, password); err != nil {
			sh.audit("SU FAILED %s -> %s", sh.user.Name, name)
			fmt.Fprintln(errw, "su: Authentication failure")
			return 1
		}
	}
	sh.audit("SU %s -> %s", sh.user.Name, name)
	sh.pushUser(target, login)
	if sh.interactive {
		fmt.Fprintf(out, "Now %s (%s). Type exit to return to %s.\n",
			target.Name, roleLabel(target.Role), sh.sessions[len(sh.sessions)-1].user.Name)
	}
	return 0
}

// builtinLogout ends the current su session, or the login session itself,
// after which the login prompt appears again.
func builtinLogout(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(sh.sessions) > 0 {
		sh.popUser()
		return 0
	}
	if !sh.leaveSession(errw, "logout") {
		return 1
	}
	sh.loggedOut = true
	sh.exitCode = 0
	return 0
}

// readNewPassword asks for a password twice and checks it.
func (sh *Shell) readNewPassword(errw io.Writer) (string, error) {
	pw, err := sh.readPassword("New password: ")
	if err != nil {
		return "", errors.New("no password given")
	}
	if err := checkPasswordStrength(pw); err != nil {
		return "", err
	}
	again, err := sh.readPassword("Retype new password: ")
	if err != nil || again != pw {
		return "", errors.New("passwords do not match")
	}
	return pw, nil
}

// requireAdmin refuses a command to anyone but an administrator.
func (sh *Shell) requireAdmin(cmd string, errw io.Writer) bool {
	if sh.user.Role == RoleAdmin {
		return true
	}
	sh.audit("DENIED %s %s (administrators only)", sh.user.Name, cmd)
	fmt.Fprintf(errw, "%s: Permission denied: only administrators can do this\n", cmd)
	return false
}

func builtinPasswd(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	name := sh.user.Name
	if len(args) > 1 {
		name = args[1]
	}
	if name != sh.user.Name && !sh.requireAdmin("passwd", errw) {
		return 1
	}
	if _, err := sh.accounts.Lookup(name); err != nil {
		fmt.Fprintf(errw, "passwd: %v\n", err)
		return 1
	}
	if sh.user.Role != RoleAdmin {
		// Proving you know the current password stops anyone who finds an
		// unlocked terminal from taking the account over.
		current, err := sh.readPassword("Current password: ")
		if err != nil {
			return 1
		}
		if _, err := sh.accounts.Authenticate(name, current); err != nil {
			sh.audit("PASSWD FAILED %s", name)
			fmt.Fprintln(errw, "passwd: Authentication failure")
			return 1
		}
	}
	pw, err := sh.readNewPassword(errw)
	if err != nil {
		fmt.Fprintf(errw, "passwd: %v\n", err)
		return 1
	}
	if err := sh.accounts.SetPassword(name, pw); err != nil {
		fmt.Fprintf(errw, "passwd: %v\n", err)
		return 1
	}
	sh.audit("PASSWD %s changed by %s", name, sh.user.Name)
	fmt.Fprintf(out, "passwd: password updated for %s\n", name)
	return 0
}

func builtinUseradd(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if !sh.requireAdmin("useradd", errw) {
		return 1
	}
	role, name := RoleStandard, ""
	for i := 1; i < len(args); i++ {
		switch {
		case (args[i] == "-r" || args[i] == "--role") && i+1 < len(args):
			i++
			r, err := parseRole(args[i])
			if err != nil {
				fmt.Fprintf(errw, "useradd: %v\n", err)
				return 1
			}
			role = r
		case strings.HasPrefix(args[i], "-"):
			fmt.Fprintf(errw, "useradd: unknown option %s\n", args[i])
			return 1
		default:
			name = args[i]
		}
	}
	if name == "" {
		fmt.Fprintln(errw, "useradd: usage: useradd [-r admin|standard|guest] name")
		return 1
	}
	if _, err := sh.accounts.Lookup(name); err == nil {
		fmt.Fprintf(errw, "useradd: %s: %v\n", name, errUserExists)
		return 1
	}
	if !validUserName.MatchString(name) {
		fmt.Fprintf(errw, "useradd: %s: invalid user name\n", name)
		return 1
	}

	pw, err := sh.readNewPassword(errw)
	if err != nil {
		fmt.Fprintf(errw, "useradd: %v\n", err)
		return 1
	}
	u, err := sh.accounts.Add(name, role, pw)
	if err != nil {
		fmt.Fprintf(errw, "useradd: %v\n", err)
		return 1
	}
	if err := sh.fs.SystemMkdir(u.Home, Inode{u.Name, u.Group, 0o700}); err != nil {
		fmt.Fprintf(errw, "useradd: home directory: %v\n", err)
	}
	sh.audit("USERADD %s (%s) by %s", u.Name, u.Role, sh.user.Name)
	fmt.Fprintf(out, "useradd: created %s (uid %d, group %s, role %s, home %s)\n", u.Name, u.UID, u.Group, u.Role, u.Home)
	return 0
}

func builtinUserdel(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if !sh.requireAdmin("userdel", errw) {
		return 1
	}
	removeHome, name := false, ""
	for _, a := range args[1:] {
		if a == "-r" {
			removeHome = true
		} else {
			name = a
		}
	}
	if name == "" {
		fmt.Fprintln(errw, "userdel: usage: userdel [-r] name")
		return 1
	}
	if name == "root" || name == sh.user.Name {
		fmt.Fprintf(errw, "userdel: %s: refusing to delete this account\n", name)
		return 1
	}
	for _, s := range sh.sessions {
		if s.user.Name == name {
			fmt.Fprintf(errw, "userdel: %s: user is logged in\n", name)
			return 1
		}
	}
	u, err := sh.accounts.Lookup(name)
	if err != nil {
		fmt.Fprintf(errw, "userdel: %v\n", err)
		return 1
	}
	if err := sh.accounts.Delete(name); err != nil {
		fmt.Fprintf(errw, "userdel: %v\n", err)
		return 1
	}
	if removeHome && strings.HasPrefix(u.Home, "/home/") {
		if err := sh.fs.SystemRemoveAll(u.Home); err != nil {
			fmt.Fprintf(errw, "userdel: %s: %v\n", u.Home, err)
		}
	}
	sh.audit("USERDEL %s by %s", name, sh.user.Name)
	fmt.Fprintf(out, "userdel: deleted %s\n", name)
	return 0
}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

func builtinChmod(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) < 3 {
		fmt.Fprintln(errw, "chmod: usage: chmod mode file ...  (mode: 640, u+x, go-w, a=r, ...)")
		return 1
	}
	change, err := parseMode(args[1])
	if err != nil {
		fmt.Fprintf(errw, "chmod: %v\n", err)
		return 1
	}
	status := 0
	for _, f := range args[2:] {
		if err := sh.fs.Chmod(sh.user, sh.abs(f), change); err != nil {
			fmt.Fprintf(errw, "chmod: %s: %v\n", f, err)
			status = 1
		}
	}
	return status
}

// parseMode accepts an octal mode ("640") or a comma-separated list of
// symbolic clauses ("u+x", "go-w", "a=r", "+x", "u=rwx,g=rx,o=").
func parseMode(spec string) (func(uint32) uint32, error) {
	if n, err := strconv.ParseUint(spec, 8, 32); err == nil {
		if n > 0o777 {
			return nil, fmt.Errorf("%s: invalid mode", spec)
		}
		return func(uint32) uint32 { return uint32(n) }, nil
	}

	type clause struct {
		who  uint32 // mask of the affected classes
		op   byte
		bits uint32 // rwx as one octal digit
	}
	var clauses []clause
	for _, part := range strings.Split(spec, ",") {
		i := 0
		var who uint32
		for i < len(part) && strings.IndexByte("ugoa", part[i]) >= 0 {
			switch part[i] {
			case 'u':
				who |= 0o700
			case 'g':
				who |= 0o070
			case 'o':
				who |= 0o007
			case 'a':
				who |= 0o777
			}
			i++
		}
		if who == 0 {
			who = 0o777
		}
		if i >= len(part) || strings.IndexByte("+-=", part[i]) < 0 {
			return nil, fmt.Errorf("%s: invalid mode", spec)
		}
		op := part[i]
		var bits uint32
		for _, c := range part[i+1:] {
			switch c {
			case 'r':
				bits |= 4
			case 'w':
				bits |= 2
			case 'x':
				bits |= 1
			default:
				return nil, fmt.Errorf("%s: invalid mode", spec)
			}
		}
		clauses = append(clauses, clause{who, op, bits})
	}

	return func(mode uint32) uint32 {
		for _, c := range clauses {
			mask := (c.bits<<6 | c.bits<<3 | c.bits) & c.who
			switch c.op {
			case '+':
				mode |= mask
			case '-':
				mode &^= mask
			case '=':
				mode = mode&^c.who | mask
			}
		}
		return mode
	}, nil
}

func builtinChown(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) < 3 {
		fmt.Fprintln(errw, "chown: usage: chown user[:group] file ...")
		return 1
	}
	owner, group, _ := strings.Cut(args[1], ":")
	if owner != "" {
		if _, err := sh.accounts.Lookup(owner); err != nil {
			fmt.Fprintf(errw, "chown: %v\n", err)
			return 1
		}
	}
	if group != "" && !sh.accounts.Groups()[group] {
		fmt.Fprintf(errw, "chown: %s: no such group\n", group)
		return 1
	}
	status := 0
	for _, f := range args[2:] {
		if err := sh.fs.Chown(sh.user, sh.abs(f), owner, group); err != nil {
			fmt.Fprintf(errw, "chown: %s: %v\n", f, err)
			status = 1
		}
	}
	return status
}

// builtinStat shows a file's inode, and what the current user may do with it.
func builtinStat(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(errw, "stat: missing operand")
		return 1
	}
	status := 0
	for _, f := range args[1:] {
		e, err := sh.fs.Stat(sh.user, sh.abs(f))
		if err != nil {
			fmt.Fprintf(errw, "stat: %s: %v\n", f, err)
			status = 1
			continue
		}
		kind := "regular file"
		if e.Info.IsDir() {
			kind = "directory"
		}
		var you string
		switch {
		case sh.user.Role == RoleAdmin:
			you = "rw" + map[bool]string{true: "x", false: "-"}[e.Info.IsDir() || e.Inode.Mode&0o111 != 0] + " (administrator)"
		default:
			class := "other"
			if sh.user.Name == e.Inode.Owner {
				class = "owner"
			} else if sh.user.Group == e.Inode.Group {
				class = "group"
			}
			bits := classBits(sh.user, e.Inode)
			if sh.user.Role == RoleGuest {
				bits &^= PermWrite
			}
			you = fmt.Sprintf("%s (as %s)", bits, class)
		}
		fmt.Fprintf(out, "  File: %s\n  Type: %s\n  Size: %d\n  Mode: %04o (%s)\n Owner: %s\n Group: %s\nModify: %s\n   You: %s\n",
			e.Path, kind, e.Info.Size(), e.Inode.Mode, modeString(e.Info, e.Inode.Mode), e.Inode.Owner, e.Inode.Group,
			e.Info.ModTime().Format("2006-01-02 15:04:05"), you)
	}
	return status
}

// ---------------------------------------------------------------------------
// Which programs a user may run
// ---------------------------------------------------------------------------

// Programs outside the shell read and write files through the host kernel,
// which does not know about the shell's users. So the shell decides up front:
//
//	administrator  any program in $PATH
//	standard user  the operating-system simulations below, plus a short list
//	               of host programs that only read their input
//	guest          the simulations only
//
// Before any program starts, every operand that names an existing file is
// checked for read permission, and absolute paths are translated into the
// shell's file system - "/etc/motd" means the shell's /etc/motd, not the
// host's.

// selfCommands are this program's own subcommands, run as child processes so
// their output can be piped like any other program's.
var selfCommands = map[string]bool{"sched": true, "mem": true, "sync": true}

var programHelpText = map[string]string{
	"sched": "sched [flags]             CPU scheduling: round robin / priority (sched -h)",
	"mem":   "mem [flags]               paging with FIFO / LRU replacement (mem -h)",
	"sync":  "sync race|pc|dining       synchronization: race, producer-consumer, philosophers",
}

// standardPrograms are the host programs a standard user may run. Each one
// only reads the files it is given, which the shell checks first.
var standardPrograms = map[string]bool{
	"date": true, "cal": true, "uname": true, "hostname": true, "sleep": true,
	"true": true, "false": true, "seq": true, "yes": true, "tr": true, "cut": true,
	"paste": true, "rev": true, "nl": true, "fold": true, "expr": true, "printf": true,
	"od": true, "comm": true, "diff": true, "cmp": true, "cksum": true, "column": true,
}

// resolveProgram decides whether the current user may run argv[0] and returns
// the host path and the argument vector to exec.
func (sh *Shell) resolveProgram(argv []string) (string, []string, int, error) {
	name := argv[0]
	path := ""
	switch {
	case selfCommands[name]:
		exe, err := os.Executable()
		if err != nil {
			return "", nil, 126, fmt.Errorf("%s: %v", name, err)
		}
		path = exe
	case sh.user.Role == RoleGuest:
		if _, err := sh.lookPath(name); err != nil {
			return "", nil, 127, err
		}
		sh.audit("DENIED %s run %s (guest)", sh.user.Name, name)
		return "", nil, 126, fmt.Errorf("%s: Permission denied: guest accounts cannot run programs", name)
	case sh.user.Role == RoleStandard && !standardPrograms[name]:
		if _, err := sh.lookPath(name); err != nil {
			return "", nil, 127, err
		}
		sh.audit("DENIED %s run %s (not allowed for standard users)", sh.user.Name, name)
		return "", nil, 126, fmt.Errorf("%s: Permission denied: only administrators may run this program (see 'help')", name)
	default:
		p, err := sh.lookPath(name)
		if err != nil {
			return "", nil, 127, err
		}
		path = p
	}

	args := make([]string, len(argv))
	args[0] = name
	if selfCommands[name] {
		// os.Executable() is aos itself: argv becomes "aos mem ...".
		args = append([]string{"aos"}, argv...)
	}
	offset := len(args) - len(argv)
	for i, a := range argv[1:] {
		translated, err := sh.hostArg(a)
		if err != nil {
			return "", nil, 1, fmt.Errorf("%s: %v", name, err)
		}
		args[offset+1+i] = translated
	}
	return path, args, 0, nil
}

// hostArg checks one program operand. If it names an existing file the user
// must be allowed to read it; an absolute path is rewritten to the host path
// inside the shell's file system. "-flag=value" is checked by its value.
func (sh *Shell) hostArg(a string) (string, error) {
	prefix, value := "", a
	if strings.HasPrefix(a, "-") {
		i := strings.Index(a, "=")
		if i < 0 {
			return a, nil
		}
		prefix, value = a[:i+1], a[i+1:]
	}
	if value == "" {
		return a, nil
	}
	vp := sh.abs(value)
	if sh.fs.Exists(vp) {
		if err := sh.fs.CheckRead(sh.user, vp); err != nil {
			return "", fmt.Errorf("%s: %v", value, err)
		}
	}
	if strings.HasPrefix(value, "/") {
		return prefix + sh.fs.Real(vp), nil
	}
	return a, nil
}

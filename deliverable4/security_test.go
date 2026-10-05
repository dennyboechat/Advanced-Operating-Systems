package main

import (
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

func TestLogin(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := session(t, fsys, acct, "alice\nwrong\nnobody\nalice123\nalice\nalice123\nwhoami\nid\n")
	wantOut(t, r, "alice\nuid=1001(alice) gid=users role=standard home=/home/alice\n")
	// A wrong password and an unknown user get the same answer.
	if strings.Count(r.err, "Login incorrect") != 2 || r.status != 0 {
		t.Errorf("stderr %q status %d", r.err, r.status)
	}
}

func TestLoginGivesUpAfterThreeFailures(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := session(t, fsys, acct, "alice\nx\nalice\ny\nalice\nz\nalice\nalice123\nwhoami\n")
	if r.status != 1 || r.out != "" || !strings.Contains(r.err, "too many failed login attempts") {
		t.Errorf("status %d out %q err %q", r.status, r.out, r.err)
	}
}

func TestLogoutReturnsToLogin(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := session(t, fsys, acct, "alice\nalice123\nwhoami\nlogout\nbob\nbob123\nwhoami\npwd\n")
	wantOut(t, r, "alice\nbob\n/home/bob\n")
}

func TestPasswordsAreSaltedHashes(t *testing.T) {
	fsys, _, _ := testSystem(t)
	shadow, err := os.ReadFile(fsys.Real(shadowFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range defaultUsers {
		if u.password != "" && strings.Contains(string(shadow), u.password) {
			t.Errorf("shadow contains the plain password of %s", u.name)
		}
	}
	if !strings.Contains(string(shadow), "root:!\n") {
		t.Errorf("root should have a locked password:\n%s", shadow)
	}

	h1, _ := hashPassword("secret99")
	h2, _ := hashPassword("secret99")
	if h1 == h2 {
		t.Error("two hashes of one password are equal: no salt")
	}
	if !verifyPassword("secret99", h1) || verifyPassword("secret98", h1) || verifyPassword("", "!") {
		t.Error("verifyPassword gives wrong answers")
	}
}

func TestLockedAccountCannotLogIn(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	if _, err := acct.Authenticate("root", ""); err == nil {
		t.Error("root logged in with an empty password")
	}
	r := session(t, fsys, acct, "root\n\nroot\n!\nroot\nroot\n")
	if r.status != 1 {
		t.Errorf("status %d, output %q", r.status, r.out)
	}
}

func TestSu(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice",
		"su bob", "wrongpw", "whoami",
		"su bob", "bob123", "whoami", "cat /home/bob/secret.txt", "pwd",
		"exit", "whoami",
		"su - bob", "bob123", "pwd", "su admin", "admin123", "who", "exit", "exit", "whoami")
	// bob may not enter alice's home directory, so his su session starts in /.
	wantOut(t, r, "alice\nbob\nbob's private notes: the answer is 42\n/\nalice\n/home/bob\n"+
		"alice      login  standard\nbob        su     standard\nadmin      su     admin   <- current\nalice\n")
	if !strings.Contains(r.err, "su: Authentication failure") {
		t.Errorf("stderr: %s", r.err)
	}
}

func TestAdminSuNeedsNoPassword(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	wantOut(t, as(t, fsys, acct, "admin", "su alice", "whoami", "exit", "whoami"), "alice\nadmin\n")
}

func TestAccountManagementIsAdminOnly(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "useradd mallory", "userdel bob", "passwd bob")
	if strings.Count(r.err, "only administrators") != 3 {
		t.Errorf("stderr: %s", r.err)
	}
	if _, err := acct.Lookup("bob"); err != nil {
		t.Error("bob was deleted by a standard user")
	}
}

func TestUseraddAndUserdel(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "admin",
		"useradd -r standard carol", "short", // too short: refused
		"useradd -r standard carol", "carol123", "carol123",
		"ls -l /home | grep carol")
	if !strings.Contains(r.out, "created carol (uid 1003, group users, role standard") ||
		!strings.Contains(r.out, "drwx------  carol    users") {
		t.Errorf("out: %s\nerr: %s", r.out, r.err)
	}
	if !strings.Contains(r.err, "at least 6 characters") {
		t.Errorf("stderr: %s", r.err)
	}

	r = session(t, fsys, acct, "carol\ncarol123\nwhoami\npwd\ntouch mine.txt\nls -l\n")
	if !strings.HasPrefix(r.out, "carol\n/home/carol\n-rw-r--r--  carol    users") {
		t.Errorf("carol's session: %q %q", r.out, r.err)
	}

	r = as(t, fsys, acct, "admin", "userdel -r carol", "id carol", "ls /home")
	if !strings.Contains(r.err, "carol: no such user") || strings.Contains(r.out, "carol\n") && !strings.Contains(r.out, "deleted carol") {
		t.Errorf("out %q err %q", r.out, r.err)
	}
	if fsys.Exists("/home/carol") {
		t.Error("userdel -r left the home directory")
	}
}

func TestPasswd(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "passwd", "nope", "passwd", "alice123", "newpass1", "newpass1")
	if !strings.Contains(r.err, "Authentication failure") || !strings.Contains(r.out, "password updated for alice") {
		t.Errorf("out %q err %q", r.out, r.err)
	}
	if _, err := acct.Authenticate("alice", "alice123"); err == nil {
		t.Error("old password still works")
	}
	if _, err := acct.Authenticate("alice", "newpass1"); err != nil {
		t.Error("new password does not work")
	}
	// An administrator may reset anyone's password without knowing it.
	as(t, fsys, acct, "admin", "passwd bob", "reset123", "reset123")
	if _, err := acct.Authenticate("bob", "reset123"); err != nil {
		t.Error("admin could not reset bob's password")
	}
}

// ---------------------------------------------------------------------------
// File permissions
// ---------------------------------------------------------------------------

func TestStandardUserCannotModifySystemFiles(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	before, _ := os.ReadFile(fsys.Real("/etc/system.conf"))
	r := as(t, fsys, acct, "alice",
		"cat /etc/motd | head -1",
		"echo hacked >> /etc/system.conf",
		"echo hacked > /etc/system.conf",
		"rm /etc/system.conf",
		"touch /etc/new.conf",
		"mkdir /etc/x",
		"mv /etc/motd /tmp/motd",
		"chmod 777 /etc/system.conf",
		"cat /etc/shadow",
		"cat /var/log/system.log")
	wantOut(t, r, "Welcome to AOS, the Advanced Operating Systems shell.\n")
	for _, want := range []string{
		"aos: /etc/system.conf: Permission denied",
		"rm: /etc/system.conf: Permission denied",
		"touch: /etc/new.conf: Permission denied",
		"mkdir: /etc/x: Permission denied",
		"mv: /etc/motd: Permission denied",
		"chmod: /etc/system.conf: Operation not permitted (not the owner)",
		"cat: /etc/shadow: Permission denied",
		"cat: /var/log/system.log: Permission denied",
	} {
		if !strings.Contains(r.err, want) {
			t.Errorf("missing %q in:\n%s", want, r.err)
		}
	}
	after, _ := os.ReadFile(fsys.Real("/etc/system.conf"))
	if string(before) != string(after) || fsys.Exists("/etc/new.conf") || !fsys.Exists("/etc/motd") {
		t.Error("a standard user changed /etc")
	}
}

func TestAdminCanModifySystemFiles(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "admin",
		"echo quantum=4 >> /etc/system.conf", "tail -n 1 /etc/system.conf",
		"cat /etc/shadow | grep -c alice", "cat /home/bob/secret.txt",
		"cat /var/log/system.log")
	wantOut(t, r, "quantum=4\n1\nbob's private notes: the answer is 42\nkernel: page frames 4, scheduler rr\n")
}

func TestHomeDirectoriesArePrivate(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "ls /home/bob", "cat /home/bob/secret.txt", "cd /home/bob", "pwd")
	wantOut(t, r, "/home/alice\n")
	if strings.Count(r.err, "Permission denied") != 3 {
		t.Errorf("stderr: %s", r.err)
	}
	wantOut(t, as(t, fsys, acct, "bob", "cat ~/secret.txt"), "bob's private notes: the answer is 42\n")
}

// /shared/team.txt is alice:users 664: alice (owner) and bob (same group) may
// write it, guest (other) may only read it.
func TestOwnerGroupOther(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	as(t, fsys, acct, "bob", "echo bob >> /shared/team.txt")
	r := as(t, fsys, acct, "guest", "cat /shared/team.txt", "echo guest >> /shared/team.txt")
	wantOut(t, r, "Team list:\nalice\nbob\n")
	if !strings.Contains(r.err, "Permission denied") {
		t.Errorf("stderr: %s", r.err)
	}

	// /shared is root:users 775: group members may create files, others not.
	r = as(t, fsys, acct, "bob", "touch /shared/bob.txt", "ls -l /shared | grep bob.txt")
	if !strings.Contains(r.out, "-rw-r--r--  bob      users") {
		t.Errorf("out %q err %q", r.out, r.err)
	}
}

func TestChmodControlsAccess(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	as(t, fsys, acct, "alice", "echo draft > /shared/plan.txt")
	wantOut(t, as(t, fsys, acct, "bob", "cat /shared/plan.txt"), "draft\n") // 644: others read

	as(t, fsys, acct, "alice", "chmod 600 /shared/plan.txt")
	r := as(t, fsys, acct, "bob", "cat /shared/plan.txt", "chmod 666 /shared/plan.txt")
	if !strings.Contains(r.err, "cat: /shared/plan.txt: Permission denied") ||
		!strings.Contains(r.err, "chmod: /shared/plan.txt: Operation not permitted (not the owner)") {
		t.Errorf("stderr: %s", r.err)
	}

	as(t, fsys, acct, "alice", "chmod g+rw /shared/plan.txt")
	wantOut(t, as(t, fsys, acct, "bob", "echo edit >> /shared/plan.txt", "cat /shared/plan.txt"), "draft\nedit\n")
	wantOut(t, as(t, fsys, acct, "alice", "ls -l /shared/plan.txt | cut -c1-10"), "-rw-rw----\n")
}

func TestDirectoryPermissions(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice",
		"mkdir box", "touch box/a", "chmod 300 box", // write+search, no read
		"ls box", "cat box/a", "echo $?",
		"chmod 600 box", // read+write, no search
		"cat box/a",
		"chmod 700 box", "rm -r box", "ls")
	if !strings.Contains(r.err, "ls: box: Permission denied") || !strings.Contains(r.err, "cat: box/a: Permission denied") {
		t.Errorf("stderr: %s", r.err)
	}
	if strings.Contains(r.out, "box") || !strings.HasPrefix(r.out, "0\n") {
		t.Errorf("out: %s", r.out)
	}
}

func TestGuestIsReadOnly(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "guest",
		"cat /etc/motd | wc -l", "touch /tmp/x", "echo x > ~/x.txt", "mkdir ~/d", "date")
	wantOut(t, r, "       2\n")
	if strings.Count(r.err, "guest accounts are read-only") != 3 ||
		!strings.Contains(r.err, "guest accounts cannot run programs") {
		t.Errorf("stderr: %s", r.err)
	}
}

func TestChownIsAdminOnly(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "chown bob notes.txt")
	if !strings.Contains(r.err, "administrators only") {
		t.Errorf("stderr: %s", r.err)
	}
	as(t, fsys, acct, "admin", "chown bob:users /shared/team.txt")
	r = as(t, fsys, acct, "alice", "stat /shared/team.txt | grep Owner", "chmod 600 /shared/team.txt")
	wantOut(t, r, " Owner: bob\n")
	if !strings.Contains(r.err, "not the owner") {
		t.Errorf("stderr: %s", r.err)
	}
}

func TestCopyBelongsToCopier(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "cp /etc/system.conf conf.copy", "echo mine >> conf.copy",
		"ls -l conf.copy | cut -c1-28", "tail -n 1 conf.copy")
	wantOut(t, r, "-rw-r--r--  alice    users  \nmine\n")
}

func TestPathsCannotLeaveTheFileSystem(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "cd ../../../../..", "pwd", "cat ../../../../etc/passwd | grep -c alice:1001")
	wantOut(t, r, "/\n1\n")

	// A symbolic link pointing out of the jail is refused.
	if err := os.Symlink("/", fsys.Real("/tmp/escape")); err != nil {
		t.Skip(err)
	}
	r = as(t, fsys, acct, "admin", "ls /tmp/escape", "cat /tmp/escape/etc/hosts")
	if strings.Count(r.err, "path leaves the file system") != 2 {
		t.Errorf("out %q err %q", r.out, r.err)
	}
}

func TestProgramPolicy(t *testing.T) {
	needProgram(t, "find", "diff", "uname")
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "find /", "uname -s | wc -l", "diff /home/bob/secret.txt /etc/motd")
	wantOut(t, r, "       1\n")
	if !strings.Contains(r.err, "find: Permission denied: only administrators") ||
		!strings.Contains(r.err, "diff: /home/bob/secret.txt: Permission denied") {
		t.Errorf("stderr: %s", r.err)
	}
	// Administrators may run anything; absolute paths mean the shell's files.
	r = as(t, fsys, acct, "admin", "diff /etc/motd /etc/motd", "echo $?")
	wantOut(t, r, "0\n")
}

func TestKillOnlyOwnJobs(t *testing.T) {
	needProgram(t, "sleep")
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice",
		"sleep 30 &", "su bob", "bob123", "kill %1", "exit", "kill %1", "echo $?")
	if !strings.Contains(r.err, "kill: %1: Operation not permitted (job belongs to alice)") || !strings.HasSuffix(r.out, "0\n") {
		t.Errorf("out %q err %q", r.out, r.err)
	}
	r = as(t, fsys, acct, "alice", "kill 1")
	if !strings.Contains(r.err, "not a job of this shell") {
		t.Errorf("stderr: %s", r.err)
	}
}

func TestIdentityVariablesAreReadOnly(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "export USER=admin", "whoami", "cat /etc/shadow")
	wantOut(t, r, "alice\n")
	if !strings.Contains(r.err, "read-only variable") || !strings.Contains(r.err, "Permission denied") {
		t.Errorf("stderr: %s", r.err)
	}
}

func TestAuthLog(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	session(t, fsys, acct, "bob\nguess\n")
	as(t, fsys, acct, "alice", "cat /etc/shadow")
	r := as(t, fsys, acct, "alice", "cat /var/log/auth.log")
	if !strings.Contains(r.err, "Permission denied") {
		t.Errorf("alice read the auth log: %q", r.out)
	}
	r = as(t, fsys, acct, "admin", "grep -c FAILED /var/log/auth.log", "grep DENIED /var/log/auth.log | grep -c shadow",
		"grep DENIED /var/log/auth.log | grep -c auth.log")
	wantOut(t, r, "1\n1\n1\n")
}

func TestPermissionsPersist(t *testing.T) {
	fsys, acct, dir := testSystem(t)
	as(t, fsys, acct, "alice", "echo hi > /tmp/a.txt", "chmod 640 /tmp/a.txt")

	reopened, acct2, created, err := openSystem(dir)
	if err != nil || created {
		t.Fatal(err, created)
	}
	e, err := reopened.Stat(&User{Name: "bob", Group: "users", Role: RoleStandard}, "/tmp/a.txt")
	if err != nil || e.Inode != (Inode{"alice", "users", 0o640}) {
		t.Errorf("inode after reopening: %+v %v", e, err)
	}
	wantOut(t, as(t, reopened, acct2, "bob", "cat /tmp/a.txt"), "hi\n") // group read
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		spec      string
		from, two uint32
	}{
		{"755", 0o000, 0o755},
		{"u+x", 0o644, 0o744},
		{"+x", 0o644, 0o755},
		{"go-w", 0o666, 0o644},
		{"a=r", 0o777, 0o444},
		{"u=rwx,g=rx,o=", 0o000, 0o750},
		{"g+w,o-r", 0o644, 0o660},
	}
	for _, c := range cases {
		f, err := parseMode(c.spec)
		if err != nil {
			t.Errorf("%s: %v", c.spec, err)
			continue
		}
		if got := f(c.from); got != c.two {
			t.Errorf("%s on %o = %o, want %o", c.spec, c.from, got, c.two)
		}
	}
	for _, bad := range []string{"999", "u+q", "x", "u", "1000"} {
		if _, err := parseMode(bad); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
}

func TestPermissionRules(t *testing.T) {
	alice := &User{Name: "alice", Group: "users", Role: RoleStandard}
	bob := &User{Name: "bob", Group: "users", Role: RoleStandard}
	eve := &User{Name: "eve", Group: "guest", Role: RoleGuest}
	admin := &User{Name: "admin", Group: "admin", Role: RoleAdmin}
	file := Inode{"alice", "users", 0o640}

	checks := []struct {
		u    *User
		want Perm
		ok   bool
	}{
		{alice, PermRead | PermWrite, true}, // owner rw-
		{alice, PermExec, false},
		{bob, PermRead, true}, // group r--
		{bob, PermWrite, false},
		{eve, PermRead, false}, // other ---
		{admin, PermRead | PermWrite, true},
		{admin, PermExec, false}, // even admins need some x bit to execute
	}
	for _, c := range checks {
		if got := allowed(c.u, file, false, c.want); got != c.ok {
			t.Errorf("%s %s on %o: got %v", c.u.Name, c.want, file.Mode, got)
		}
	}
	if !allowed(admin, Inode{"alice", "users", 0o744}, false, PermExec) {
		t.Error("admin cannot execute a file with an x bit")
	}
}

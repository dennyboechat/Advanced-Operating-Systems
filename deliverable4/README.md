# Deliverable 4 — Piping and Security (Go)

The shell from Deliverable 1, rebuilt into the `aos` program from Deliverables
2 and 3, with two new features:

| Feature | What it does |
|---------|--------------|
| **Piping** | `ls \| grep txt`, `cat file \| grep error \| sort`: the output of each command feeds the next through a kernel pipe, with any number of stages. Built-ins, host programs and scripts can be mixed in one pipeline. |
| **Security** | A **login** with user names and salted, hashed passwords. **Users with different permission levels**: administrator, standard user and guest. A file system where every file has an **owner, a group and rwx permissions**, and every access is checked, so a standard user cannot modify system files or read another user's home directory. |

The simulations from earlier deliverables are still here, as subcommands and
as commands inside the shell: `sched` (CPU scheduling, Deliverable 2), `mem`
(paging) and `sync` (synchronization, both Deliverable 3). Because they run as
programs, their output can be piped as well.

## Requirements

Go 1.24 or newer, on macOS or Linux. The shell calls `fork`/`exec`, `wait4`,
`setpgid` and terminal `ioctl`s directly, so it does not build on Windows. No
external dependencies.

## Build and run

```
go build -o aos .
./aos                                        # log in and use the shell
./aos shell -user admin demo.gosh            # the scripted tour (password admin123)
```

The first run creates the shell's file system in `./aosfs` (change it with
`-root dir` or `$AOS_ROOT`) and prints the default accounts:

| User    | Password   | Role       | Group   | Can do |
|---------|------------|------------|---------|--------|
| `admin` | `admin123` | admin      | `admin` | Everything: ignores file permissions, manages accounts, runs any program |
| `alice` | `alice123` | standard   | `users` | What the permission bits allow; a short list of host programs |
| `bob`   | `bob123`   | standard   | `users` | Same as alice. Same group, so he can use files alice shares with the group |
| `guest` | `guest123` | guest      | `guest` | Read only: every write is refused, no host programs |
| `root`  | (locked)   | admin      | `root`  | Owns the system files. Nobody can log in as root |

`./aos shell -reset` throws the file system away and starts over. Other
subcommands: `./aos sched ...`, `./aos mem ...`, `./aos sync ...`, `./aos help`.

A session:

```
$ ./aos
AOS shell (pid 4527). Log in to continue.

aos login: alice
Password:                                       <- not echoed

Welcome to AOS, the Advanced Operating Systems shell.
Logged in as alice (standard user).

alice@aos:~$ ls | grep txt
notes.txt
todo.txt
alice@aos:~$ cat /var/log/app.log | grep error | cut -c21- | sort | uniq -c | sort -rn
      2 error connection refused by db
      1 error disk quota exceeded for /home/bob
      1 error authentication failed for guest
alice@aos:~$ echo "max_processes=999" >> /etc/system.conf
aos: /etc/system.conf: Permission denied
alice@aos:~$ cat /home/bob/secret.txt
cat: /home/bob/secret.txt: Permission denied
alice@aos:~$ su
Password:
admin@aos:/home/alice# cat /home/bob/secret.txt
bob's private notes: the answer is 42
admin@aos:/home/alice# exit
alice@aos:~$ logout

aos login:
```

---

## Part 1 — Piping

### Syntax

```
cmd1 | cmd2 | ... | cmdN [< in] [> out | >> out] [&]
```

- Any number of stages. Each one is a built-in, a host program, or a script.
- `<`, `>` and `>>` work on any stage, as in `sh`. A redirection takes the
  place of the pipe on that side: in `a > f | b`, `b` reads nothing.
- `&` runs the whole pipeline in the background as **one job**.
- A `|` inside quotes is an ordinary character: `echo "a|b"`.
- Errors: `| ls`, `ls |`, `ls | | wc` and `ls || wc` are syntax errors.

```
ls | grep txt
cat /var/log/app.log | grep error | sort
cat /var/log/app.log | grep -v INFO | cut -c21- | sort | uniq -c | sort -rn | head -3
seq 1 1000 | grep 7 | wc -l
ls -l /etc | grep root | sort -r > /tmp/etc.txt
grep refused < /var/log/app.log | wc -l
yes | head -n 2
sleep 100 | cat &
mem -algo both -frames 3 -refs "7 0 1 2 0 3 0 4 2 3 0 3 2 1 2 0 1 7 0 1" | grep "Page faults"
cat /usr/share/examples/workload.txt | mem -algo lru -file - -quiet | tail -5
```

### How it works (`exec.go`)

For a pipeline of `n` commands the shell calls `pipe(2)` `n-1` times. Stage
`i` writes into pipe `i` and stage `i+1` reads from it. Every stage runs at
the same time. Data moves through the kernel's pipe buffer: a full pipe blocks
its writer, an empty one blocks its reader.

```
            pipe 0              pipe 1
  cat  ──w═══════════r──  grep  ──w═══════════r──  sort ──> terminal
 (fd 1)              (fd 0)    (fd 1)              (fd 0)
```

Each stage is started in one of three ways:

| Stage | How it runs |
|-------|-------------|
| Host program (`seq`, `tr`, `mem` ...) | `fork()` + `exec()` with the pipe ends as fd 0 and fd 1. All the processes of a pipeline join **one process group**, so Ctrl-C, Ctrl-Z, `fg`, `bg` and `kill %1` act on the whole job. |
| Built-in (`ls`, `grep`, `sort` ...) | Runs on a goroutine inside the shell, reading and writing the same pipe ends. It gets a **subshell**, a copy of the shell's state, so `cd /tmp \| echo` doesn't move the shell. Same as bash. |
| Script (`./x.gosh`) | Interpreted by a subshell on a goroutine. |

Three details matter for correctness, and the tests check each one:

- **End-of-file.** A reader only sees EOF once *every* copy of the write end
  is closed. After each `fork()`, the shell closes its own copy of that
  stage's pipe ends; built-in stages close theirs when they return. If
  this is missed, `cat file | sort` hangs forever.
- **Broken pipes.** When a reader exits early, the writer gets `SIGPIPE`. That
  is why `yes | head -n 2` finishes even though `yes` never stops on its own.
  The shell doesn't report the SIGPIPE, the same as bash.
- **Exit status.** A pipeline's `$?` is the status of its last command:
  `echo hi | grep nomatch` gives 1, `false | echo ok` gives 0.

A stage that cannot start (`command not found`, `Permission denied`) prints
its error. The rest of the pipeline still runs, and its neighbours see EOF.
`su`, `logout`, `passwd`, `useradd`, `fg` and `bg` are refused inside a
pipeline. They need the terminal and the shell's own state.

### Text filters

These built-ins are made for the middle of a pipeline. Each one reads
standard input when no file is named:

| Command | Options |
|---------|---------|
| `grep pattern [file...]` | `-i` ignore case, `-v` invert, `-n` line numbers, `-c` count. Go regular expressions. Exit status 1 when nothing matches. |
| `sort [file...]` | `-r` reverse, `-n` numeric, `-u` unique |
| `uniq [file]` | `-c` count, `-d` only repeated lines |
| `wc [file...]` | `-l` lines, `-w` words, `-c` bytes |
| `head` / `tail [file...]` | `-n N` or `-N` (default 10) |

They are built in, rather than taken from the host's `grep` and `sort`, so
that every file they open goes through the permission checks in Part 2.
`ls` prints one name per line when its output is not a terminal, so
`ls | grep txt` filters file names, not rows of columns.

---

## Part 2 — Security

### User authentication (`users.go`, `shell.go`)

The shell starts at a login prompt, the way `getty` and `login` do on a
console. The password is read **with terminal echo turned off**
(`tcsetattr` via `ioctl`, `tty.go`). Without a terminal, for example
`printf 'alice\nalice123\nls\n' | ./aos`, the password is read from the next
line of input.

Accounts are kept in two files inside the shell's file system, split the same
way as on Unix:

```
/etc/passwd   alice:1001:users:standard:/home/alice        mode 644, anyone may read it
/etc/shadow   alice:pbkdf2-sha256$100000$<salt>$<hash>     mode 600, only root
```

- **Passwords are never stored.** `shadow` holds a PBKDF2-SHA256 hash
  (100,000 iterations) with a random 16-byte salt for each account. Two
  users with the same password get different hashes. A login hashes the typed
  password with the stored salt and compares the result in **constant time**.
- **No account enumeration.** A wrong password and an unknown user both get
  `Login incorrect`. An unknown user is checked against a dummy hash, so the
  two cases also take the same time.
- **Limited attempts.** After 3 failures in a row the login program exits.
  At a terminal each failure also waits 2 seconds.
- **Locked accounts.** `root` has `!` instead of a hash, which matches no
  password.
- **Sessions.** `logout` returns to the login prompt. `exit` leaves the
  program. Both hang up any jobs still running, with `SIGHUP`.

### Users and permission levels

| Role | Files | Accounts | Host programs |
|------|-------|----------|---------------|
| **admin** | Ignores read and write permission, like root. Still cannot execute a file with no `x` bit at all. | `useradd`, `userdel`, `passwd <anyone>`, `chown` | Any program in `$PATH` |
| **standard** | Bound by the owner/group/other bits | Own password only (needs the current one) | Only an allowlist of programs that just read their input: `date cal uname seq tr cut rev nl diff ...` (see `help`) |
| **guest** | Read only. Every write is refused, whatever the mode bits say | none | none (the `sched`/`mem`/`sync` simulations only) |

Commands:

| Command | Description |
|---------|-------------|
| `whoami`, `id [user]` | Who you are: uid, group, role, home |
| `users` | List the accounts (reads `/etc/passwd`) |
| `who` | The login session and any `su` sessions stacked on top of it |
| `su [-] [user]` | Become another user (default `admin`). Asks for *that user's* password, unless an administrator runs it. `exit` returns to the previous user. With `-`, start in their home directory |
| `passwd [user]` | Change a password (at least 6 characters, typed twice) |
| `useradd [-r admin\|standard\|guest] name` | Create an account and a private home directory (admins only) |
| `userdel [-r] name` | Delete an account; `-r` also deletes its home directory (admins only) |

`$USER`, `$LOGNAME` and `$HOME` are read-only. The shell decides who you are
from the login, never from a variable.

### File permissions (`vfs.go`)

Every user of the shell is the same user to the host kernel, so the host's
permission bits cannot tell alice from bob. The shell therefore keeps a file
system of its own, much like a kernel does:

- **A jail.** A host directory (`aosfs/disk`) becomes `/`. Paths are
  cleaned, so `..` at the top stays at the top, and a symbolic link that
  leads out of the directory is refused.
- **An inode table** (`aosfs/inodes.json`) records the owner, group and
  9-bit `rwx` mode of every file. A file the table doesn't know about, for
  example one a host program created, gets the owner of its directory and
  the default mode.
- **Every built-in that touches a file goes through the checks.** That is
  `cat ls cd cp mv rm touch mkdir rmdir grep sort ...`, plus `<`, `>` and `>>`
  redirections, scripts, and the file operands of host programs.

The checks are the Unix ones. The **owner** digit applies to the owner,
the **group** digit to members of the file's group, and **other** to everyone
else:

| Operation | Needs |
|-----------|-------|
| Read a file (`cat`, `grep`, `< f`, `cp` source) | `r` on the file |
| Write a file (`> f`, `>> f`) | `w` on the file |
| Create or delete an entry (`touch`, `mkdir`, `rm`, `mv`, `cp` target) | `w` and `x` on the **directory** |
| List a directory (`ls`) | `r` on the directory |
| Enter a directory (`cd`, and any path through it) | `x` on every directory on the path |
| Run a script (`./x.gosh`) | `r` and `x` on the file |
| `rm -r` a tree | `rwx` on every directory in it |
| `chmod` | Being the owner, or an administrator |
| `chown` | Being an administrator |

| Command | Description |
|---------|-------------|
| `ls -l` | Shows mode, owner and group: `-rw-r-----  alice    users  ...` |
| `stat file` | The inode, plus what *you* may do with the file and why (owner, group or other) |
| `chmod mode file...` | Octal (`640`) or symbolic (`u+x`, `go-w`, `a=r`, `u=rwx,g=rx,o=`) |
| `chown user[:group] file...` | Change the owner and/or group (admins only) |
| `cp src dest` | The copy is a new file **owned by whoever made the copy** |

New files are `644` and new directories `755` (umask `022`).

The file system starts like this:

```
/                        root  root   755
├── etc                  root  root   755   system configuration
│   ├── passwd           root  root   644   accounts (readable by all)
│   ├── shadow           root  root   600   password hashes (root only)
│   ├── motd             root  root   644
│   └── system.conf      root  root   644   a system file: users can read it, not change it
├── home
│   ├── admin            admin admin  700
│   ├── alice            alice users  700   notes.txt todo.txt data.csv photo.jpg hello.gosh
│   ├── bob              bob   users  700   secret.txt (600)
│   └── guest            guest guest  755
├── shared               root  users  775   group-writable: alice and bob, not guest
│   ├── readme.txt       root  root   644
│   └── team.txt         alice users  664
├── tmp                  root  root   777   anyone may create files (except guests)
├── usr/share/examples   root  root   755   processes.txt, workload.txt for sched / mem
└── var/log
    ├── app.log          root  root   644   sample log for the grep / sort pipelines
    ├── system.log       root  admin  640   administrators only
    └── auth.log         root  root   600   the audit log
```

What that gives, for example:

```
alice$ echo x >> /etc/system.conf      Permission denied           (644 root: other has r-- only)
alice$ rm /etc/motd                    Permission denied           (no w on /etc)
alice$ cat /etc/shadow                 Permission denied           (600 root)
alice$ cat /home/bob/secret.txt        Permission denied           (no x on /home/bob, 700)
alice$ ./hello.gosh                    Permission denied           (644: no x)
alice$ chmod u+x hello.gosh; ./hello.gosh    runs
bob$   echo hi >> /shared/team.txt     ok                          (664, bob is in group users)
guest$ echo hi >> /shared/team.txt     Permission denied           (guest is "other": r--)
guest$ touch /tmp/x                    Permission denied (guest accounts are read-only)
admin# echo x >> /etc/system.conf      ok                          (administrators bypass r/w)
```

### Programs and processes

Host programs read and write files through the host kernel, which knows
nothing about the shell's users. So the shell decides *before* `exec()`:

- Which program may run depends on the role (see the table above). A
  refused program gives `Permission denied: only administrators may run this
  program`.
- Every operand that names an existing file is checked for read permission.
  `diff /home/bob/secret.txt x` is refused for alice.
- Absolute paths are translated into the shell's file system. `/etc/motd`
  means the shell's `/etc/motd`, not the host's.

`kill`, `fg` and `bg` only act on jobs of this shell. A standard user can only
act on their own jobs, never another user's (each job records who started it,
shown by `ps`).

### Audit log

`/var/log/auth.log` (root only) records every login, failed login, lockout,
logout, `su`, `useradd`, `userdel`, `passwd`, and **every refused access**:

```
admin# grep DENIED /var/log/auth.log | tail -n 3
2026-10-05 19:38:10 DENIED bob write /shared/plan.txt (Permission denied)
2026-10-05 19:38:10 DENIED guest write /tmp/guest.txt (Permission denied (guest accounts are read-only))
2026-10-05 19:38:10 DENIED guest run date (guest)
```

---

## Everything else in the shell (from Deliverable 1)

| Command | Description |
|---------|-------------|
| `cd [dir]`, `pwd` | Change and print the directory. `cd` alone goes home, `cd -` goes back |
| `ls [-l] [-a]`, `cat`, `touch`, `mkdir [-p]`, `rmdir`, `rm [-r] [-f]`, `cp`, `mv` | Files, all permission-checked |
| `echo [-n]`, `clear` | Output |
| `jobs [-l]`, `fg [%n]`, `bg [%n]`, `kill [-SIG] pid\|%n`, `ps` | Job control. A pipeline is one job; `ps` lists every process of it with the shared PGID and the owner |
| `export NAME=value`, `env` | Variables passed to every program |
| `help [command]`, `exit [n]` | |

Also: quoting (`'...'`, `"..."`, `\`), `$VAR`, `${VAR}`, `$?`, `~`, `#`
comments, and inside scripts `$0`–`$9` and `$#`. Ctrl-Z stops the
foreground job, and Ctrl-C goes to the job, never to the shell.

## Tests

```
go test ./...          # everything, including the Deliverable 2 and 3 tests
go test -race ./...    # pipeline stages run concurrently; this is clean
go test -short ./...   # skips the real-time scheduler tests
```

- **Piping**: parsing (stages, redirections, quoted `|`, syntax errors); `ls |
  grep txt`; three- and seven-stage chains of built-ins; host program to host
  program (`seq | tr`); mixed (`seq | grep | wc`, `echo | rev | tr`); `yes |
  head` finishing through SIGPIPE; `$?` is the last command's status;
  redirections inside pipelines; built-in stages run in subshells; stages that
  fail don't hang the pipeline; `mem` piped into `grep`; scripts in pipelines.
- **Authentication**: login, wrong password, unknown user (same message),
  lockout after 3 attempts, logout and a second login, salted hashes with no
  plain-text passwords in `shadow`, locked `root`, `su` with and without the
  right password, admins `su` without one, `passwd` (needs the old password;
  admins can reset), `useradd` / `userdel` admin-only, and new users can log in.
- **File permissions**: standard users cannot modify, delete, chmod or create
  in `/etc`, or read `shadow`, and the files really are unchanged afterwards.
  Also: admins can; private home directories; owner/group/other on
  `/shared`; chmod granting and revoking access; directory `r` vs `x`; guests
  are read-only; chown is admin-only; copies belong to the copier; `..` and
  symbolic links cannot leave the file system; the program allowlist and
  operand checks; users cannot kill each other's jobs; `export USER=admin`
  changes nothing; the audit log; permissions persist across restarts; the
  `chmod` parser; and the permission rule table.

## Design notes and limitations

- **This is a simulation of multi-user security**, enforced by the shell. To
  the host it is still one user. Anyone with a host shell can edit
  `aosfs/` directly, just as anyone with physical access can read a disk.
- Administrators may run any host program, and a host program uses host
  permissions. An admin running `find /` sees host paths inside `aosfs/disk`.
  That is why standard users only get programs that read the files they are
  given, and guests get none.
- Pipeline stages that are built-ins run as goroutines, not processes, so
  Ctrl-C does not interrupt them. A pipeline made only of built-ins finishes
  when its input does (or Ctrl-D, when reading the terminal).
- No `;`, `&&`, `||`, globbing or command substitution. `2>` is not
  supported. The umask is fixed at `022`. There is no sticky bit, so anyone
  who may write to `/tmp` may delete files in it, as on Unix without `+t`.
- The `sched`, `mem` and `sync` simulations are unchanged. See
  `../deliverable2/README.md` and `../deliverable3/README.md`.

## Files

| File | Purpose |
|------|---------|
| `main.go` | Subcommand dispatch: shell (default), `sched`, `mem`, `sync` |
| `shell.go` | Shell state, login loop, sessions and `su` stack, read-eval-print loop, scripts |
| `parser.go` | Tokenizer, quoting, expansion, pipelines and redirections |
| `exec.go` | **Pipelines**: `pipe()`, `fork()`/`exec()`, built-in stages on goroutines, process groups |
| `jobs.go` | Process table (a job is a pipeline of processes), `wait()`, `jobs`/`fg`/`bg`/`ps` |
| `builtins.go` | File, process and shell built-ins |
| `textutils.go` | `grep`, `sort`, `uniq`, `wc`, `head`, `tail` |
| `security.go` | `whoami`, `id`, `su`, `passwd`, `useradd`, `chmod`, `chown`, `stat`, ...; the program policy |
| `users.go` | Accounts, roles, `/etc/passwd` and `/etc/shadow`, PBKDF2 password hashing |
| `vfs.go` | **The file system**: jail, inode table, owner/group/other permission checks |
| `seed.go` | The initial file system and accounts |
| `tty.go`, `term_*.go` | Terminal process groups, and echo off for passwords |
| `shell_test.go`, `security_test.go` | Piping and security tests |
| `demo.gosh` | Scripted tour: `./aos shell -user admin demo.gosh` |
| `memory*.go`, `semaphore.go`, `syncsim.go`, `sync_cmd.go`, `workload.txt` | Deliverable 3 simulations |
| `sched_cmd.go`, `scheduler.go`, `priority.go`, `live.go`, `processes.txt` | Deliverable 2 scheduler |

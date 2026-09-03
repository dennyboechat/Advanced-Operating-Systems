# Deliverable 1 — gosh, a Unix-like shell in Go

A custom shell that simulates a Unix-like operating system environment. It keeps
its own **process table**, creates processes with `fork()`/`exec()`, collects
their status with `wait()`/`exit()`, and arbitrates the terminal between itself
and the job the user is talking to — which is all that job control really is.

## Requirements

Go 1.21+ (developed on 1.24) on a Unix-like system (macOS or Linux). The shell
issues `ioctl`, `fork`, `execve`, `wait4`, `setpgid` and `kill` directly, so it
does not build on Windows.

## Run

```
go build -o gosh .
./gosh
```

Or without building:

```
go run .
```

It also runs scripts non-interactively, which is the easiest way to see
everything at once:

```
./gosh demo.gosh
```

## Built-in commands

| Command | Description |
|---------|-------------|
| `cd [dir]` | Change the working directory. No argument means `$HOME`, `-` means the previous directory. |
| `pwd` | Print the working directory. |
| `exit [status]` | Terminate the shell. Refuses once if jobs are stopped, then hangs up every remaining job with `SIGHUP`. |
| `echo [-n] [text ...]` | Print text. `-n` suppresses the trailing newline. |
| `clear` | Clear the screen and the scrollback. |
| `ls [-l] [-a] [path ...]` | List directory contents. `-l` adds mode, size and time; `-a` includes dotfiles. |
| `cat [file ...]` | Print files; with no operand it copies standard input. |
| `mkdir [-p] dir ...` | Create directories. |
| `rmdir dir ...` | Remove **empty** directories. |
| `rm [-r] [-f] file ...` | Remove files; `-r` for directory trees, `-f` to ignore what is missing. |
| `touch file ...` | Create a file, or update the timestamps of an existing one. |
| `kill [-SIG] pid \| %job` | Send a signal (default `TERM`). `-l` lists the names. |
| `jobs [-l]` | List background and stopped jobs. `-l` adds pids. |
| `fg [%job]` | Resume a job in the foreground. |
| `bg [%job]` | Resume a stopped job in the background. |
| `ps` | Show the shell's process table, including the shell itself. |
| `export NAME=value` | Set a variable inherited by every child process. |
| `env` | List the environment. |
| `help [command]` | Describe the built-ins. |

Anything not in that table is looked up in `$PATH` and run as a new process.

Built-ins deliberately run **inside** the shell rather than in a forked child:
`cd` has to change *this* process's working directory, `exit` has to terminate
*this* process, and `jobs`/`fg`/`bg`/`ps` have to read *this* process's job
table. A child could do none of those things — its changes would die with it.

## Process management

Every external command follows the same four system calls:

| Step | Call | What happens |
|------|------|--------------|
| 1 | `fork()` | `syscall.ForkExec` duplicates the shell process. |
| 2 | `exec()` | The copy is immediately overwritten by the requested program. |
| 3 | `wait()` | `syscall.Wait4` collects the child's exit or stop status. |
| 4 | `exit()` | That status becomes `$?`, and the job leaves the process table. |

**Foreground** (`sleep 5`): the shell blocks in `wait4(pid, WUNTRACED)` until the
child exits or is stopped.

**Background** (`sleep 5 &`): the shell prints `[1] 30556` and returns to the
prompt at once. Finished background jobs are collected with
`wait4(-1, WNOHANG)` before the next prompt, which is when
`[1]+ Done sleep 5` appears.

### Job control

Each job is placed in a **process group of its own** (`setpgid`), because that is
the unit the kernel signals. Only one process group at a time may own the
terminal, and that group is the one that receives `SIGINT` (Ctrl-C) and
`SIGTSTP` (Ctrl-Z). Job control is therefore just handing that ownership back and
forth with `tcsetpgrp`:

```
gosh$ sleep 100          # the shell hands the terminal to the job and waits
^Z
[1]+  Stopped                sleep 100
gosh$ bg                 # SIGCONT, but the shell keeps the terminal
[1]+  Running                sleep 100 &
gosh$ jobs
[1]+  Running                sleep 100 &
gosh$ fg                 # terminal handed back, shell waits again
sleep 100
gosh$ kill -KILL %1
[1]+  Terminated (SIGKILL)   sleep 100
```

Job specifications are `%1`, `1`, `%+` / `%%` (the current job, marked `+`) and
`%-` (the previous job, marked `-`). `fg` and `bg` with no argument mean the
current job.

Two details that a shell has to get right, and that this one does:

* **Terminal signals are caught, not ignored.** The shell must not die on Ctrl-C
  or stop on Ctrl-Z, but `exec()` preserves an *ignored* signal while resetting a
  *caught* one to its default. Ignoring `SIGINT` in the shell would therefore
  make every child immune to Ctrl-C as well. `gosh` installs handlers that
  discard the signal instead.
* **`SIGTTOU` is suppressed around `tcsetpgrp`.** Taking the terminal back is by
  definition done from the background, and the kernel's answer to a background
  process touching the terminal is to stop it with `SIGTTOU` — or to fail the
  call with `EIO` when the process group cannot be stopped. Ignoring the signal
  for the duration of the call is what marks the access as deliberate.

## Other features

* Quoting: `'literal'`, `"$expanded"`, and `\` escapes.
* Expansion: `$VAR`, `${VAR}`, `$?` (last exit status) and a leading `~`.
* Redirection: `< file`, `> file`, `>> file`, for built-ins as well as programs.
* `#` starts a comment.

## Limitations

* No pipelines (`|`), command lists (`;`, `&&`), globbing or command
  substitution.
* Built-ins ignore a trailing `&` and run in the foreground, since running them
  elsewhere would throw away the state change that is their whole point.
* Terminal modes (`termios`) are not saved and restored around each job, so a
  program that leaves the terminal in raw mode leaves it that way.
* Job status changes are noticed when the shell next looks (before a prompt, or
  in `jobs`/`ps`) rather than asynchronously via `SIGCHLD`.

## Files

| File | Purpose |
|------|---------|
| `main.go` | Shell struct, terminal/signal setup, read-eval-print loop |
| `parser.go` | Tokenizer, quoting, expansion, redirection parsing |
| `exec.go` | `fork()`/`exec()` and terminal handover |
| `jobs.go` | Process table, `wait()` handling, `jobs`/`fg`/`bg`/`ps` |
| `builtins.go` | The remaining built-in commands |
| `tty.go` | `tcgetpgrp`/`tcsetpgrp` as raw `ioctl` calls |
| `demo.gosh` | Scripted tour, run with `./gosh demo.gosh` |

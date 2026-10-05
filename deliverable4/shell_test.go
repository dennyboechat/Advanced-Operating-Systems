package main

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain makes the test binary stand in for aos when the shell starts one of
// the simulations as a child process ("aos mem ..."): os.Executable() is the
// test binary here.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[0] == "aos" && runSubcommand(os.Args[1], os.Args[2:]) {
		os.Exit(0)
	}
	hashIterations = 1000 // fast hashing; the format is the same
	loginFailDelay = 0
	os.Exit(m.Run())
}

// syncBuffer is a bytes.Buffer safe for the concurrent writers of a pipeline.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testSystem creates a fresh file system with the default accounts.
func testSystem(t *testing.T) (*FileSystem, *Accounts, string) {
	t.Helper()
	dir := t.TempDir()
	fsys, acct, created, err := openSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected a new file system")
	}
	return fsys, acct, dir
}

// result is what a shell session printed.
type result struct {
	out, err string
	status   int
}

// session feeds input to the shell's login loop, exactly as a terminal-less
// "printf ... | aos" would: a user name, a password, then commands.
func session(t *testing.T, fsys *FileSystem, acct *Accounts, input string) result {
	t.Helper()
	var out, errw syncBuffer
	sh := newShell(fsys, acct, &out, &errw)
	r := bufio.NewReader(strings.NewReader(input))
	sh.in, sh.tin = r, r
	sh.stdin = strings.NewReader("")

	done := make(chan int, 1)
	go func() { done <- sh.Main("") }()
	select {
	case status := <-done:
		return result{out.String(), errw.String(), status}
	case <-time.After(20 * time.Second):
		t.Fatalf("shell did not finish; output so far:\n%s\n%s", out.String(), errw.String())
		return result{}
	}
}

// as logs in with one of the default accounts and runs the commands.
func as(t *testing.T, fsys *FileSystem, acct *Accounts, user string, commands ...string) result {
	t.Helper()
	pw := map[string]string{}
	for _, u := range defaultUsers {
		pw[u.name] = u.password
	}
	return session(t, fsys, acct, user+"\n"+pw[user]+"\n"+strings.Join(commands, "\n")+"\n")
}

func needProgram(t *testing.T, names ...string) {
	t.Helper()
	sh := &Shell{env: baseEnv()}
	for _, n := range names {
		if _, err := sh.lookPath(n); err != nil {
			t.Skipf("%s is not installed", n)
		}
	}
}

func wantOut(t *testing.T, r result, want string) {
	t.Helper()
	if r.out != want {
		t.Errorf("output:\n%s\nwant:\n%s\nstderr:\n%s", r.out, want, r.err)
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

func TestParsePipeline(t *testing.T) {
	sh := &Shell{env: map[string]string{"HOME": "/home/alice"}}
	p, err := sh.parse("ls -l ~ | grep 'a b' | sort -r > out.txt &")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Cmds) != 3 || !p.Background {
		t.Fatalf("got %d commands, background %v", len(p.Cmds), p.Background)
	}
	if got := strings.Join(p.Cmds[0].Argv, ","); got != "ls,-l,/home/alice" {
		t.Errorf("stage 0 argv = %s", got)
	}
	if got := p.Cmds[1].Argv; len(got) != 2 || got[1] != "a b" {
		t.Errorf("stage 1 argv = %q", got)
	}
	if c := p.Cmds[2]; c.OutFile != "out.txt" || c.Append || c.InFile != "" {
		t.Errorf("stage 2 redirection = %+v", c)
	}
	if p.Text != "ls -l /home/alice | grep 'a b' | sort -r > out.txt" {
		t.Errorf("text = %q", p.Text)
	}

	// A quoted bar is an ordinary character.
	p, err = sh.parse(`echo "a|b" '|'`)
	if err != nil || len(p.Cmds) != 1 || p.Cmds[0].Argv[1] != "a|b" || p.Cmds[0].Argv[2] != "|" {
		t.Errorf("quoted bar: %+v, %v", p, err)
	}
}

func TestParsePipelineErrors(t *testing.T) {
	sh := &Shell{env: map[string]string{}}
	for _, line := range []string{"| ls", "ls |", "ls | | wc", "ls || wc", "ls & | wc", "> f | wc"} {
		if _, err := sh.parse(line); err == nil {
			t.Errorf("%q: expected a syntax error", line)
		}
	}
}

// ---------------------------------------------------------------------------
// Pipes
// ---------------------------------------------------------------------------

func TestPipeLsGrep(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	wantOut(t, as(t, fsys, acct, "alice", "ls | grep txt"), "notes.txt\ntodo.txt\n")
}

func TestPipeThreeStages(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "cat /var/log/app.log | grep error | sort -r")
	wantOut(t, r, ""+
		"2026-10-01 09:22:10 error connection refused by db\n"+
		"2026-10-01 09:15:00 error authentication failed for guest\n"+
		"2026-10-01 09:10:03 error connection refused by db\n"+
		"2026-10-01 09:05:12 error disk quota exceeded for /home/bob\n")
}

func TestPipeLongChain(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice",
		"cat /var/log/app.log | grep -v INFO | grep -i error | sort | uniq -c | sort -rn | head -1")
	// The duplicate "connection refused" lines differ only in their time, so
	// every count is 1; sort -rn keeps the reverse-alphabetical first line.
	wantOut(t, r, "      1 2026-10-01 09:22:10 error connection refused by db\n")
}

// Two external programs connected by a pipe: two fork()/exec()s in one
// process group, the kernel's pipe between them.
func TestPipeBetweenExternalPrograms(t *testing.T) {
	needProgram(t, "seq", "tr")
	fsys, acct, _ := testSystem(t)
	wantOut(t, as(t, fsys, acct, "alice", "seq 1 5 | tr 1-5 a-e"), "a\nb\nc\nd\ne\n")
}

func TestPipeMixesProgramsAndBuiltins(t *testing.T) {
	needProgram(t, "seq", "rev")
	fsys, acct, _ := testSystem(t)
	wantOut(t, as(t, fsys, acct, "alice", "seq 1 20 | grep 1 | wc -l"), "      11\n")
	wantOut(t, as(t, fsys, acct, "alice", "echo hello | rev | tr a-z A-Z"), "OLLEH\n")
}

// When the reader of a pipe exits, the writer must be stopped (SIGPIPE), or
// "yes | head" would never finish.
func TestPipeReaderExitStopsWriter(t *testing.T) {
	needProgram(t, "yes")
	fsys, acct, _ := testSystem(t)
	wantOut(t, as(t, fsys, acct, "alice", "yes | head -n 3"), "y\ny\ny\n")
	wantOut(t, as(t, fsys, acct, "admin", "yes | yes no | head -2"), "no\nno\n")
}

func TestPipeStatusIsLastCommand(t *testing.T) {
	needProgram(t, "false")
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice",
		"echo hi | grep nomatch", "echo $?",
		"false | echo ok", "echo $?",
		"echo hi | false", "echo $?")
	wantOut(t, r, "1\nok\n0\n1\n")
}

func TestPipeWithRedirections(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice",
		"cat /var/log/app.log | grep error > /tmp/errors.txt",
		"wc -l < /tmp/errors.txt",
		"grep refused < /tmp/errors.txt | wc -l",
		"echo one >> /tmp/errors.txt",
		"tail -n 1 /tmp/errors.txt")
	wantOut(t, r, "       4\n       2\none\n")
}

// Built-ins in a pipeline run in a subshell: a cd there does not move the
// shell itself, as in bash.
func TestPipeStagesRunInSubshells(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "cd /tmp | echo x", "pwd", "export A=1 | echo y", "echo [$A]")
	wantOut(t, r, "x\n/home/alice\ny\n[]\n")
}

func TestPipeRejectsInteractiveBuiltins(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "su | cat", "echo $?")
	if !strings.Contains(r.err, "cannot be used in a pipeline") || r.out != "2\n" {
		t.Errorf("out %q err %q", r.out, r.err)
	}
}

func TestPipeFailedStageDoesNotHang(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice", "nosuchprogram | wc -l", "cat /etc/shadow | wc -l")
	wantOut(t, r, "       0\n       0\n")
	if !strings.Contains(r.err, "nosuchprogram: command not found") ||
		!strings.Contains(r.err, "cat: /etc/shadow: Permission denied") {
		t.Errorf("stderr: %s", r.err)
	}
}

// The simulations of earlier deliverables run as child processes, so they can
// be piped too.
func TestPipeSimulation(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice",
		`mem -algo fifo -frames 3 -refs "7 0 1 2 0 3 0 4 2 3 0 3 2 1 2 0 1 7 0 1" | grep "Page faults"`)
	if !strings.Contains(r.out, "Page faults:") || !strings.Contains(r.out, "15") {
		t.Errorf("output %q, stderr %q", r.out, r.err)
	}
}

// ---------------------------------------------------------------------------
// Scripts
// ---------------------------------------------------------------------------

func TestScriptNeedsExecutePermission(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice",
		"./hello.gosh a b", "echo $?",
		"chmod u+x hello.gosh",
		"./hello.gosh a b", "echo $?",
		"./hello.gosh x y | grep Hello")
	if !strings.Contains(r.err, "hello.gosh: Permission denied") {
		t.Errorf("stderr: %s", r.err)
	}
	want := "126\nHello from a script, alice! Arguments: a b\nalice\nnotes.txt\ntodo.txt\n0\n" +
		"Hello from a script, alice! Arguments: x y\n"
	wantOut(t, r, want)
}

func TestScriptRunsInSubshell(t *testing.T) {
	fsys, acct, _ := testSystem(t)
	r := as(t, fsys, acct, "alice",
		`echo "cd /tmp" > s.gosh`, `echo "pwd" >> s.gosh`, `echo "exit 3" >> s.gosh`, `echo "echo unreachable" >> s.gosh`,
		"chmod 755 s.gosh", "./s.gosh", "echo $?", "pwd")
	wantOut(t, r, "/tmp\n3\n/home/alice\n")
}

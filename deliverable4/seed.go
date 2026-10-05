package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// The sample inputs of the scheduler and the paging simulator, so they can be
// used from inside the shell: sched -file /usr/share/examples/processes.txt
var (
	//go:embed processes.txt
	exampleProcesses string
	//go:embed workload.txt
	exampleWorkload string
)

// The file system a fresh installation starts with.
//
//	/                     root  root   755
//	├── etc               root  root   755   system configuration
//	│   ├── passwd        root  root   644   accounts (readable by all)
//	│   ├── shadow        root  root   600   password hashes (root only)
//	│   ├── motd          root  root   644
//	│   └── system.conf   root  root   644   a "system file" users cannot modify
//	├── home
//	│   ├── admin         admin admin  700
//	│   ├── alice         alice users  700   notes.txt, todo.txt, hello.gosh, ...
//	│   ├── bob           bob   users  700   secret.txt (600)
//	│   └── guest         guest guest  755
//	├── shared            root  users  775   group-writable: alice and bob, not guest
//	│   ├── readme.txt    root  root   644
//	│   └── team.txt      alice users  664
//	├── tmp               root  root   777   anyone may create files
//	├── usr/share/examples                    processes.txt, workload.txt for sched / mem
//	└── var/log
//	    ├── app.log       root  root   644   sample log for grep / sort pipelines
//	    ├── system.log    root  admin  640   administrators only
//	    └── auth.log      root  root   600   logins, su, account changes, denials

type seedUser struct {
	name     string
	uid      int
	role     Role
	password string
}

// defaultUsers are the accounts of a new installation. The passwords are
// printed when the file system is created and should be changed with passwd.
var defaultUsers = []seedUser{
	{"root", 0, RoleAdmin, ""}, // owns the system files; login disabled
	{"admin", 1, RoleAdmin, "admin123"},
	{"alice", 1001, RoleStandard, "alice123"},
	{"bob", 1002, RoleStandard, "bob123"},
	{"guest", 2000, RoleGuest, "guest123"},
}

type seedFile struct {
	path    string
	inode   Inode
	content string // "" with dir set means a directory
	dir     bool
}

var seedTree = []seedFile{
	{path: "/etc", inode: Inode{"root", "root", 0o755}, dir: true},
	{path: "/home", inode: Inode{"root", "root", 0o755}, dir: true},
	{path: "/home/admin", inode: Inode{"admin", "admin", 0o700}, dir: true},
	{path: "/home/alice", inode: Inode{"alice", "users", 0o700}, dir: true},
	{path: "/home/bob", inode: Inode{"bob", "users", 0o700}, dir: true},
	{path: "/home/guest", inode: Inode{"guest", "guest", 0o755}, dir: true},
	{path: "/shared", inode: Inode{"root", "users", 0o775}, dir: true},
	{path: "/tmp", inode: Inode{"root", "root", 0o777}, dir: true},
	{path: "/var", inode: Inode{"root", "root", 0o755}, dir: true},
	{path: "/var/log", inode: Inode{"root", "root", 0o755}, dir: true},
	{path: "/usr", inode: Inode{"root", "root", 0o755}, dir: true},
	{path: "/usr/share", inode: Inode{"root", "root", 0o755}, dir: true},
	{path: "/usr/share/examples", inode: Inode{"root", "root", 0o755}, dir: true},

	{path: "/etc/motd", inode: Inode{"root", "root", 0o644}, content: "Welcome to AOS, the Advanced Operating Systems shell.\n" +
		"Type 'help' for the commands, 'whoami' to see who you are.\n"},
	{path: "/etc/system.conf", inode: Inode{"root", "root", 0o644}, content: "# System configuration - only administrators may change this file.\n" +
		"hostname=aos\nmax_processes=64\npage_frames=4\nscheduler=rr\nquantum=2\n"},

	{path: "/home/alice/notes.txt", inode: Inode{"alice", "users", 0o644}, content: "Meeting on Monday\nFinish the OS deliverable\nBuy coffee\n"},
	{path: "/home/alice/todo.txt", inode: Inode{"alice", "users", 0o644}, content: "write tests\nfix error handling\nupdate README\n"},
	{path: "/home/alice/photo.jpg", inode: Inode{"alice", "users", 0o644}, content: "not really a photo\n"},
	{path: "/home/alice/data.csv", inode: Inode{"alice", "users", 0o644}, content: "name,score\nalice,90\nbob,85\ncarol,97\n"},
	{path: "/home/alice/hello.gosh", inode: Inode{"alice", "users", 0o644}, content: "# A shell script. Run it with ./hello.gosh after chmod +x hello.gosh\n" +
		"echo \"Hello from a script, $USER! Arguments: $1 $2\"\nwhoami\nls | grep txt\n"},
	{path: "/home/bob/secret.txt", inode: Inode{"bob", "users", 0o600}, content: "bob's private notes: the answer is 42\n"},

	{path: "/shared/readme.txt", inode: Inode{"root", "root", 0o644}, content: "Shared folder. Members of the 'users' group may add files here.\n"},
	{path: "/shared/team.txt", inode: Inode{"alice", "users", 0o664}, content: "Team list:\nalice\n"},

	{path: "/var/log/app.log", inode: Inode{"root", "root", 0o644}, content: "" +
		"2026-10-01 09:00:01 INFO  server started\n" +
		"2026-10-01 09:05:12 error disk quota exceeded for /home/bob\n" +
		"2026-10-01 09:07:45 WARN  high memory usage\n" +
		"2026-10-01 09:10:03 error connection refused by db\n" +
		"2026-10-01 09:12:30 INFO  user alice logged in\n" +
		"2026-10-01 09:15:00 error authentication failed for guest\n" +
		"2026-10-01 09:20:44 INFO  backup finished\n" +
		"2026-10-01 09:22:10 error connection refused by db\n"},
	{path: "/var/log/system.log", inode: Inode{"root", "admin", 0o640}, content: "kernel: page frames 4, scheduler rr\n"},
	{path: authLog, inode: Inode{"root", "root", 0o600}, content: ""},
}

func init() {
	seedTree = append(seedTree,
		seedFile{path: "/usr/share/examples/processes.txt", inode: Inode{"root", "root", 0o644}, content: exampleProcesses},
		seedFile{path: "/usr/share/examples/workload.txt", inode: Inode{"root", "root", 0o644}, content: exampleWorkload})
}

// openSystem opens the shell's file system in dataDir, creating and populating
// it on first use. created reports whether that happened.
func openSystem(dataDir string) (fsys *FileSystem, acct *Accounts, created bool, err error) {
	disk := filepath.Join(dataDir, "disk")
	meta := filepath.Join(dataDir, "inodes.json")

	if _, err := os.Stat(disk); os.IsNotExist(err) {
		if err := os.MkdirAll(disk, 0o755); err != nil {
			return nil, nil, false, err
		}
		created = true
	}

	fsys, err = OpenFileSystem(disk, meta)
	if err != nil {
		return nil, nil, false, err
	}
	acct = &Accounts{fs: fsys}
	if created {
		if err := seed(fsys, acct); err != nil {
			return nil, nil, false, fmt.Errorf("creating the file system: %v", err)
		}
	}
	return fsys, acct, created, nil
}

func seed(fsys *FileSystem, acct *Accounts) error {
	fsys.SetInode("/", Inode{"root", "root", 0o755})
	for _, f := range seedTree {
		real := fsys.Real(f.path)
		var err error
		if f.dir {
			err = os.MkdirAll(real, 0o755)
		} else {
			err = os.WriteFile(real, []byte(f.content), 0o644)
		}
		if err != nil {
			return err
		}
		fsys.SetInode(f.path, f.inode)
	}

	var users []*User
	hashes := map[string]string{}
	for _, s := range defaultUsers {
		home := "/home/" + s.name
		group := groupFor[s.role]
		if s.name == "root" {
			home, group = "/", "root"
		}
		users = append(users, &User{Name: s.name, UID: s.uid, Group: group, Role: s.role, Home: home})
		if s.password != "" {
			h, err := hashPassword(s.password)
			if err != nil {
				return err
			}
			hashes[s.name] = h
		}
	}
	if err := acct.save(users, hashes); err != nil {
		return err
	}
	return fsys.Save()
}

// seedBanner tells the person who just created the file system how to log in.
func seedBanner(dataDir string) string {
	s := fmt.Sprintf("aos: created a new file system in %s with these accounts:\n", dataDir)
	for _, u := range defaultUsers {
		if u.password != "" {
			s += fmt.Sprintf("  %-6s %-9s password %s\n", u.name, u.role, u.password)
		}
	}
	return s + "  (change them with passwd)\n"
}

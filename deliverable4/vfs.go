package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The shell's file system.
//
// Every user of the shell is the same user to the host kernel, so the host's
// own permission bits cannot tell alice from bob. The shell therefore keeps a
// file system of its own, the way a kernel does: a directory on the host
// becomes "/" (a chroot jail - ".." at the top stays at the top), and an inode
// table records the owner, group and rwx mode of every file in it. Every
// built-in that touches a file goes through the checks below, so access is
// decided by the simulated permissions and never by the host's.
//
// The checks are the Unix ones:
//
//	read a file           r on the file
//	write a file          w on the file
//	create or delete      w and x on the directory that holds the entry
//	list a directory      r on the directory
//	enter a directory     x on the directory (cd, and every directory on a path)
//	run a script          r and x on the file
//	chmod                 owner of the file, or an administrator
//	chown                 administrators only
//
// Administrators bypass read and write checks the way root does, but still
// cannot execute a file that has no x bit at all. Guest accounts are read-only:
// every write is refused whatever the mode bits say.

// Perm is a set of rwx permission bits, as they appear in one octal digit.
type Perm uint32

const (
	PermExec  Perm = 1
	PermWrite Perm = 2
	PermRead  Perm = 4
)

func (p Perm) String() string {
	s := []byte("---")
	if p&PermRead != 0 {
		s[0] = 'r'
	}
	if p&PermWrite != 0 {
		s[1] = 'w'
	}
	if p&PermExec != 0 {
		s[2] = 'x'
	}
	return string(s)
}

// Inode is the metadata the shell keeps for one file.
type Inode struct {
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  uint32 `json:"mode"` // permission bits, 0o777
}

const (
	defaultFileMode = 0o644 // 0666 &^ umask 022
	defaultDirMode  = 0o755 // 0777 &^ umask 022
)

// Errors reported by the file system. Built-ins print them after the name the
// user typed, e.g. "cat: /etc/shadow: Permission denied".
var (
	ErrDenied   = errors.New("Permission denied")
	ErrReadOnly = errors.New("Permission denied (guest accounts are read-only)")
	ErrNotOwner = errors.New("Operation not permitted (not the owner)")
	ErrAdmin    = errors.New("Operation not permitted (administrators only)")
	ErrNotExist = errors.New("No such file or directory")
	ErrExist    = errors.New("File exists")
	ErrIsDir    = errors.New("Is a directory")
	ErrNotDir   = errors.New("Not a directory")
	ErrNotEmpty = errors.New("Directory not empty")
	ErrEscape   = errors.New("Permission denied (path leaves the file system)")
)

// Entry is a file as the shell sees it: its virtual path, what the host says
// about it and what the inode table says about it.
type Entry struct {
	Path  string // virtual path, e.g. /home/alice/notes.txt
	Info  os.FileInfo
	Inode Inode
}

// FileSystem is the jailed file system with simulated permissions.
type FileSystem struct {
	root     string // host directory that is "/"
	realRoot string // root with symlinks resolved, for the jail check
	metaFile string // where the inode table is saved

	mu     sync.Mutex
	inodes map[string]Inode

	// audit, when set, is told about every refused access.
	audit func(user, op, vpath string, err error)
}

// OpenFileSystem loads the inode table of the file system rooted at root.
func OpenFileSystem(root, metaFile string) (*FileSystem, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	fsys := &FileSystem{root: root, realRoot: real, metaFile: metaFile, inodes: map[string]Inode{}}
	data, err := os.ReadFile(metaFile)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &fsys.inodes); err != nil {
			return nil, fmt.Errorf("%s: %v", metaFile, err)
		}
	case !os.IsNotExist(err):
		return nil, err
	}
	if _, ok := fsys.inodes["/"]; !ok {
		fsys.inodes["/"] = Inode{"root", "root", 0o755}
	}
	return fsys, nil
}

// Abs turns a path typed by the user into a clean absolute virtual path.
// path.Clean keeps ".." from climbing above "/", which is what makes the
// directory a jail.
func (fsys *FileSystem) Abs(cwd, p string) string {
	if p == "" {
		return cwd
	}
	if !strings.HasPrefix(p, "/") {
		p = cwd + "/" + p
	}
	return path.Clean(p)
}

// Real is the host path behind a virtual path.
func (fsys *FileSystem) Real(vp string) string {
	return filepath.Join(fsys.root, filepath.FromSlash(vp))
}

// Exists reports whether something is at vp, without any permission check.
func (fsys *FileSystem) Exists(vp string) bool {
	_, err := os.Lstat(fsys.Real(vp))
	return err == nil
}

// contained refuses paths that a symbolic link would take outside the jail.
func (fsys *FileSystem) contained(vp string) error {
	r, err := filepath.EvalSymlinks(fsys.Real(vp))
	if err != nil {
		return nil // does not exist (yet): nothing to follow
	}
	if r != fsys.realRoot && !strings.HasPrefix(r, fsys.realRoot+string(filepath.Separator)) {
		return ErrEscape
	}
	return nil
}

// inodeLocked returns the metadata of vp. A file the table does not know -
// one created on the host, or by a program run from the shell - belongs to the
// owner of the directory it appeared in, with the default mode.
func (fsys *FileSystem) inodeLocked(vp string, isDir bool) Inode {
	if in, ok := fsys.inodes[vp]; ok {
		return in
	}
	if vp == "/" {
		return Inode{"root", "root", 0o755}
	}
	parent := fsys.inodeLocked(path.Dir(vp), true)
	mode := uint32(defaultFileMode)
	if isDir {
		mode = defaultDirMode
	}
	return Inode{Owner: parent.Owner, Group: parent.Group, Mode: mode}
}

// classBits picks the owner, group or other digit of the mode for u.
func classBits(u *User, in Inode) Perm {
	switch {
	case u.Name == in.Owner:
		return Perm(in.Mode>>6) & 7
	case u.Group == in.Group:
		return Perm(in.Mode>>3) & 7
	default:
		return Perm(in.Mode) & 7
	}
}

// allowed applies the permission rules to one inode.
func allowed(u *User, in Inode, isDir bool, want Perm) bool {
	if u.Role == RoleAdmin {
		// Like root: everything except executing a file nobody may execute.
		return isDir || want&PermExec == 0 || in.Mode&0o111 != 0
	}
	return classBits(u, in)&want == want
}

// checkLocked verifies that u may reach vp (x on every directory above it) and
// then has want on vp itself. It returns the host's information about vp, or
// nil when vp does not exist and mustExist is false.
func (fsys *FileSystem) checkLocked(u *User, op, vp string, want Perm, mustExist bool) (os.FileInfo, error) {
	deny := func(err error) (os.FileInfo, error) {
		if fsys.audit != nil {
			fsys.audit(u.Name, op, vp, err)
		}
		return nil, err
	}

	if want&PermWrite != 0 && u.Role == RoleGuest {
		return deny(ErrReadOnly)
	}
	if err := fsys.contained(vp); err != nil {
		return deny(err)
	}

	// Search permission on every directory on the way down.
	if vp != "/" {
		dir := "/"
		for _, part := range strings.Split(strings.TrimPrefix(path.Dir(vp), "/"), "/") {
			if part != "" {
				dir = path.Join(dir, part)
			}
			info, err := os.Stat(fsys.Real(dir))
			if err != nil {
				return nil, ErrNotExist
			}
			if !info.IsDir() {
				return nil, ErrNotDir
			}
			if !allowed(u, fsys.inodeLocked(dir, true), true, PermExec) {
				return deny(ErrDenied)
			}
			if dir == path.Dir(vp) {
				break
			}
		}
	}

	info, err := os.Lstat(fsys.Real(vp))
	if err != nil {
		if mustExist {
			return nil, ErrNotExist
		}
		return nil, nil
	}
	if want != 0 && !allowed(u, fsys.inodeLocked(vp, info.IsDir()), info.IsDir(), want) {
		return deny(ErrDenied)
	}
	return info, nil
}

// checkParentLocked verifies that u may add or remove entries in the directory
// holding vp: w and x on that directory.
func (fsys *FileSystem) checkParentLocked(u *User, op, vp string) error {
	if vp == "/" {
		return ErrDenied
	}
	info, err := fsys.checkLocked(u, op, path.Dir(vp), PermWrite|PermExec, true)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrNotDir
	}
	return nil
}

// ---------------------------------------------------------------------------
// Operations used by the built-ins
// ---------------------------------------------------------------------------

// Stat returns what is at vp. Like stat(2) it needs search permission on the
// directories above vp, but nothing on vp itself.
func (fsys *FileSystem) Stat(u *User, vp string) (*Entry, error) {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	info, err := fsys.checkLocked(u, "stat", vp, 0, true)
	if err != nil {
		return nil, err
	}
	return &Entry{Path: vp, Info: info, Inode: fsys.inodeLocked(vp, info.IsDir())}, nil
}

// Open opens a file for reading.
func (fsys *FileSystem) Open(u *User, vp string) (*os.File, error) {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	info, err := fsys.checkLocked(u, "read", vp, PermRead, true)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, ErrIsDir
	}
	return os.Open(fsys.Real(vp))
}

// Create opens a file for writing, truncating it or appending to it. A new
// file is owned by u, in u's group, with mode 0644.
func (fsys *FileSystem) Create(u *User, vp string, appendTo bool) (*os.File, error) {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()

	flags := os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_WRONLY | os.O_APPEND
	}

	info, err := fsys.checkLocked(u, "write", vp, PermWrite, false)
	if err != nil {
		return nil, err
	}
	if info != nil {
		if info.IsDir() {
			return nil, ErrIsDir
		}
		return os.OpenFile(fsys.Real(vp), flags, 0)
	}

	if err := fsys.checkParentLocked(u, "create", vp); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(fsys.Real(vp), flags|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, errNoPath(err)
	}
	fsys.inodes[vp] = Inode{Owner: u.Name, Group: u.Group, Mode: defaultFileMode}
	return f, fsys.saveLocked()
}

// ReadDir lists a directory, which takes read permission on it.
func (fsys *FileSystem) ReadDir(u *User, vp string) ([]*Entry, error) {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	info, err := fsys.checkLocked(u, "list", vp, PermRead, true)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, ErrNotDir
	}
	dirents, err := os.ReadDir(fsys.Real(vp))
	if err != nil {
		return nil, errNoPath(err)
	}
	var out []*Entry
	for _, d := range dirents {
		fi, err := d.Info()
		if err != nil {
			continue
		}
		child := path.Join(vp, d.Name())
		out = append(out, &Entry{Path: child, Info: fi, Inode: fsys.inodeLocked(child, fi.IsDir())})
	}
	return out, nil
}

// Chdir checks that u may make vp the working directory.
func (fsys *FileSystem) Chdir(u *User, vp string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	return fsys.chdirLocked(u, vp)
}

// CanEnter reports whether u could cd into vp, without auditing a refusal:
// the shell asks when it picks a starting directory, not on the user's behalf.
// The audit hook is only read with the lock held, so muting it here is safe.
func (fsys *FileSystem) CanEnter(u *User, vp string) bool {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	saved := fsys.audit
	fsys.audit = nil
	defer func() { fsys.audit = saved }()
	return fsys.chdirLocked(u, vp) == nil
}

func (fsys *FileSystem) chdirLocked(u *User, vp string) error {
	info, err := fsys.checkLocked(u, "cd", vp, 0, true)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrNotDir
	}
	if !allowed(u, fsys.inodeLocked(vp, true), true, PermExec) {
		return fsys.denied(u, "cd", vp, ErrDenied)
	}
	return nil
}

// Mkdir creates a directory owned by u.
func (fsys *FileSystem) Mkdir(u *User, vp string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	return fsys.mkdirLocked(u, vp)
}

func (fsys *FileSystem) mkdirLocked(u *User, vp string) error {
	if fsys.Exists(vp) {
		return ErrExist
	}
	if err := fsys.checkParentLocked(u, "mkdir", vp); err != nil {
		return err
	}
	if err := os.Mkdir(fsys.Real(vp), 0o755); err != nil {
		return errNoPath(err)
	}
	fsys.inodes[vp] = Inode{Owner: u.Name, Group: u.Group, Mode: defaultDirMode}
	return fsys.saveLocked()
}

// MkdirAll creates vp and any missing parents, each with the same checks.
func (fsys *FileSystem) MkdirAll(u *User, vp string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	dir := "/"
	for _, part := range strings.Split(strings.TrimPrefix(vp, "/"), "/") {
		if part == "" {
			continue
		}
		dir = path.Join(dir, part)
		info, err := os.Stat(fsys.Real(dir))
		if err == nil {
			if !info.IsDir() {
				return ErrNotDir
			}
			continue
		}
		if err := fsys.mkdirLocked(u, dir); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes a file, or an empty directory when dirOK is set. Deleting an
// entry is a change to its directory, so it takes w and x there - not on the
// file. That is why /tmp-style shared directories need the sticky bit on Unix.
func (fsys *FileSystem) Remove(u *User, vp string, dirOK bool) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	info, err := fsys.checkLocked(u, "remove", vp, 0, true)
	if err != nil {
		return err
	}
	if info.IsDir() && !dirOK {
		return ErrIsDir
	}
	if !info.IsDir() && dirOK {
		return ErrNotDir
	}
	if err := fsys.checkParentLocked(u, "remove", vp); err != nil {
		return err
	}
	if err := os.Remove(fsys.Real(vp)); err != nil {
		if info.IsDir() {
			return ErrNotEmpty
		}
		return errNoPath(err)
	}
	fsys.forgetLocked(vp)
	return fsys.saveLocked()
}

// RemoveAll deletes a whole tree. Emptying each directory on the way takes
// read, write and search permission on it, exactly as rm -r needs on Unix, so a
// tree containing someone else's private directory cannot be removed.
func (fsys *FileSystem) RemoveAll(u *User, vp string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if _, err := fsys.checkLocked(u, "remove", vp, 0, true); err != nil {
		return err
	}
	if err := fsys.checkParentLocked(u, "remove", vp); err != nil {
		return err
	}
	err := fsys.removeTreeLocked(u, vp)
	if serr := fsys.saveLocked(); err == nil {
		err = serr
	}
	return err
}

func (fsys *FileSystem) removeTreeLocked(u *User, vp string) error {
	info, err := os.Lstat(fsys.Real(vp))
	if err != nil {
		return errNoPath(err)
	}
	if info.IsDir() {
		if !allowed(u, fsys.inodeLocked(vp, true), true, PermRead|PermWrite|PermExec) {
			if fsys.audit != nil {
				fsys.audit(u.Name, "remove", vp, ErrDenied)
			}
			return fmt.Errorf("%s: %w", vp, ErrDenied)
		}
		dirents, err := os.ReadDir(fsys.Real(vp))
		if err != nil {
			return errNoPath(err)
		}
		for _, d := range dirents {
			if err := fsys.removeTreeLocked(u, path.Join(vp, d.Name())); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(fsys.Real(vp)); err != nil {
		return errNoPath(err)
	}
	fsys.forgetLocked(vp)
	return nil
}

// Rename moves an entry, which changes both directories involved.
func (fsys *FileSystem) Rename(u *User, from, to string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	info, err := fsys.checkLocked(u, "move", from, 0, true)
	if err != nil {
		return err
	}
	if err := fsys.checkParentLocked(u, "move", from); err != nil {
		return err
	}
	if fi, err := os.Stat(fsys.Real(to)); err == nil && fi.IsDir() {
		to = path.Join(to, path.Base(from))
	}
	if to == from {
		return nil
	}
	if info.IsDir() && strings.HasPrefix(to, from+"/") {
		return fmt.Errorf("cannot move a directory into itself")
	}
	if err := fsys.checkParentLocked(u, "move", to); err != nil {
		return err
	}
	if err := os.Rename(fsys.Real(from), fsys.Real(to)); err != nil {
		return errNoPath(err)
	}

	moved := map[string]Inode{}
	for k, in := range fsys.inodes {
		if k == from || strings.HasPrefix(k, from+"/") {
			moved[to+strings.TrimPrefix(k, from)] = in
			delete(fsys.inodes, k)
		}
	}
	fsys.forgetLocked(to)
	for k, in := range moved {
		fsys.inodes[k] = in
	}
	return fsys.saveLocked()
}

// Touch updates the timestamps of an existing file (owner or write
// permission), or creates an empty one.
func (fsys *FileSystem) Touch(u *User, vp string) error {
	fsys.mu.Lock()
	info, err := fsys.checkLocked(u, "touch", vp, 0, false)
	if err != nil {
		fsys.mu.Unlock()
		return err
	}
	if info != nil {
		defer fsys.mu.Unlock()
		in := fsys.inodeLocked(vp, info.IsDir())
		if u.Role == RoleGuest {
			return ErrReadOnly
		}
		if u.Name != in.Owner && !allowed(u, in, info.IsDir(), PermWrite) {
			return ErrDenied
		}
		now := time.Now()
		return errNoPath(os.Chtimes(fsys.Real(vp), now, now))
	}
	fsys.mu.Unlock()

	f, err := fsys.Create(u, vp, true)
	if err != nil {
		return err
	}
	return f.Close()
}

// Chmod changes the mode bits. Only the owner or an administrator may do it.
func (fsys *FileSystem) Chmod(u *User, vp string, change func(old uint32) uint32) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	info, err := fsys.checkLocked(u, "chmod", vp, 0, true)
	if err != nil {
		return err
	}
	in := fsys.inodeLocked(vp, info.IsDir())
	switch {
	case u.Role == RoleGuest:
		return fsys.denied(u, "chmod", vp, ErrReadOnly)
	case u.Role != RoleAdmin && u.Name != in.Owner:
		return fsys.denied(u, "chmod", vp, ErrNotOwner)
	}
	in.Mode = change(in.Mode) & 0o777
	fsys.inodes[vp] = in
	return fsys.saveLocked()
}

// Chown gives a file to another user and/or group: administrators only, since
// otherwise users could escape quotas or plant files on each other.
func (fsys *FileSystem) Chown(u *User, vp, owner, group string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	info, err := fsys.checkLocked(u, "chown", vp, 0, true)
	if err != nil {
		return err
	}
	if u.Role != RoleAdmin {
		return fsys.denied(u, "chown", vp, ErrAdmin)
	}
	in := fsys.inodeLocked(vp, info.IsDir())
	if owner != "" {
		in.Owner = owner
	}
	if group != "" {
		in.Group = group
	}
	fsys.inodes[vp] = in
	return fsys.saveLocked()
}

// CheckRead is used for the operands of programs run from the shell: they read
// the file through the host, so the shell checks read permission first.
func (fsys *FileSystem) CheckRead(u *User, vp string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	_, err := fsys.checkLocked(u, "read", vp, PermRead, true)
	return err
}

// CheckExec verifies that u may run the script at vp: r and x on a regular
// file.
func (fsys *FileSystem) CheckExec(u *User, vp string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	info, err := fsys.checkLocked(u, "execute", vp, PermRead|PermExec, true)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return ErrIsDir
	}
	return nil
}

func (fsys *FileSystem) denied(u *User, op, vp string, err error) error {
	if fsys.audit != nil {
		fsys.audit(u.Name, op, vp, err)
	}
	return err
}

// ---------------------------------------------------------------------------
// System access: the "kernel" acting on its own behalf, with no checks
// ---------------------------------------------------------------------------

// SystemWrite replaces a file's contents, creating it with the given inode if
// it does not exist. Used for /etc/passwd, /etc/shadow and home directories.
func (fsys *FileSystem) SystemWrite(vp string, data []byte, in *Inode) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	tmp := fsys.Real(vp) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, fsys.Real(vp)); err != nil {
		return err
	}
	if _, ok := fsys.inodes[vp]; !ok && in != nil {
		fsys.inodes[vp] = *in
		return fsys.saveLocked()
	}
	return nil
}

// SystemMkdir creates a directory with the given inode.
func (fsys *FileSystem) SystemMkdir(vp string, in Inode) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if err := os.MkdirAll(fsys.Real(vp), 0o755); err != nil {
		return err
	}
	fsys.inodes[vp] = in
	return fsys.saveLocked()
}

// SystemRemoveAll deletes a tree.
func (fsys *FileSystem) SystemRemoveAll(vp string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if vp == "/" {
		return ErrDenied
	}
	if err := os.RemoveAll(fsys.Real(vp)); err != nil {
		return err
	}
	fsys.forgetLocked(vp)
	return fsys.saveLocked()
}

// SystemRead reads a file.
func (fsys *FileSystem) SystemRead(vp string) ([]byte, error) {
	return os.ReadFile(fsys.Real(vp))
}

// SystemAppend appends a line to a log file.
func (fsys *FileSystem) SystemAppend(vp, line string) {
	f, err := os.OpenFile(fsys.Real(vp), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	io.WriteString(f, line)
	f.Close()
}

// SetInode records metadata directly (used when the file system is created).
func (fsys *FileSystem) SetInode(vp string, in Inode) {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	fsys.inodes[vp] = in
}

// Save writes the inode table to disk.
func (fsys *FileSystem) Save() error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	return fsys.saveLocked()
}

// forgetLocked drops the inodes of vp and everything below it.
func (fsys *FileSystem) forgetLocked(vp string) {
	for k := range fsys.inodes {
		if k == vp || strings.HasPrefix(k, vp+"/") {
			delete(fsys.inodes, k)
		}
	}
}

// saveLocked writes the inode table atomically: a crash leaves the old table
// or the new one, never half of each. (encoding/json sorts map keys, so the
// file lists paths in order.)
func (fsys *FileSystem) saveLocked() error {
	data, err := json.MarshalIndent(fsys.inodes, "", "  ")
	if err != nil {
		return err
	}
	tmp := fsys.metaFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, fsys.metaFile)
}

// modeString renders "drwxr-x---" from the type and the simulated mode.
func modeString(info os.FileInfo, mode uint32) string {
	kind := "-"
	switch {
	case info.IsDir():
		kind = "d"
	case info.Mode()&os.ModeSymlink != 0:
		kind = "l"
	}
	return kind + Perm(mode>>6&7).String() + Perm(mode>>3&7).String() + Perm(mode&7).String()
}

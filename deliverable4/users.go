package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// User accounts.
//
// Accounts live in two files inside the shell's file system, split the way
// Unix splits them:
//
//	/etc/passwd  name:uid:group:role:home   mode 0644, anyone may read it
//	/etc/shadow  name:password-hash         mode 0600, root only
//
// Passwords are never stored: shadow holds a salted PBKDF2-SHA256 hash, and a
// login hashes the typed password with the same salt and compares the results
// in constant time. Both files are re-read on every lookup, so they are the
// single source of truth - an administrator can inspect them with cat.

// Role is a permission level.
type Role string

const (
	RoleAdmin    Role = "admin"    // superuser: bypasses file permissions, manages accounts
	RoleStandard Role = "standard" // ordinary user: bound by the mode bits
	RoleGuest    Role = "guest"    // read-only: every write is refused
)

// groupFor is the primary group a new account of each role gets.
var groupFor = map[Role]string{RoleAdmin: "admin", RoleStandard: "users", RoleGuest: "guest"}

func parseRole(s string) (Role, error) {
	switch r := Role(strings.ToLower(s)); r {
	case RoleAdmin, RoleStandard, RoleGuest:
		return r, nil
	case "user", "std":
		return RoleStandard, nil
	}
	return "", fmt.Errorf("%s: unknown role (admin, standard or guest)", s)
}

// User is one account.
type User struct {
	Name  string
	UID   int
	Group string
	Role  Role
	Home  string
}

const (
	passwdFile = "/etc/passwd"
	shadowFile = "/etc/shadow"
	authLog    = "/var/log/auth.log"

	minPasswordLen = 6
)

// hashIterations is the PBKDF2 work factor. Tests lower it.
var hashIterations = 100_000

var (
	errLogin       = errors.New("Login incorrect")
	errNoSuchUser  = errors.New("no such user")
	errUserExists  = errors.New("user already exists")
	validUserName  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	lockedPassword = "!" // a shadow entry that matches no password
)

// Accounts reads and writes the account files.
type Accounts struct {
	fs *FileSystem
	mu sync.Mutex // serializes read-modify-write of the two files
}

// hashPassword returns "pbkdf2-sha256$iterations$salt$hash".
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", hashIterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// verifyPassword checks a password against a stored hash.
func verifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false // locked ("!") or malformed: no password matches
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked against when the user does not exist, so that a wrong
// user name takes as long to reject as a wrong password and the timing does not
// reveal which accounts exist.
var dummyHash = sync.OnceValue(func() string {
	h, _ := hashPassword("not a password")
	return h
})

// load parses both files.
func (a *Accounts) load() ([]*User, map[string]string, error) {
	data, err := a.fs.SystemRead(passwdFile)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %v", passwdFile, errNoPath(err))
	}
	var users []*User
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) != 5 {
			continue
		}
		uid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		users = append(users, &User{Name: f[0], UID: uid, Group: f[2], Role: Role(f[3]), Home: f[4]})
	}

	hashes := map[string]string{}
	if data, err := a.fs.SystemRead(shadowFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if name, hash, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
				hashes[name] = hash
			}
		}
	}
	return users, hashes, nil
}

// save writes both files back.
func (a *Accounts) save(users []*User, hashes map[string]string) error {
	sort.Slice(users, func(i, j int) bool { return users[i].UID < users[j].UID })
	var pw, sh strings.Builder
	for _, u := range users {
		fmt.Fprintf(&pw, "%s:%d:%s:%s:%s\n", u.Name, u.UID, u.Group, u.Role, u.Home)
		hash := hashes[u.Name]
		if hash == "" {
			hash = lockedPassword
		}
		fmt.Fprintf(&sh, "%s:%s\n", u.Name, hash)
	}
	if err := a.fs.SystemWrite(passwdFile, []byte(pw.String()), &Inode{"root", "root", 0o644}); err != nil {
		return err
	}
	return a.fs.SystemWrite(shadowFile, []byte(sh.String()), &Inode{"root", "root", 0o600})
}

// List returns every account, ordered by uid.
func (a *Accounts) List() ([]*User, error) {
	users, _, err := a.load()
	if err != nil {
		return nil, err
	}
	sort.Slice(users, func(i, j int) bool { return users[i].UID < users[j].UID })
	return users, nil
}

// Lookup finds an account by name.
func (a *Accounts) Lookup(name string) (*User, error) {
	users, _, err := a.load()
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.Name == name {
			return u, nil
		}
	}
	return nil, fmt.Errorf("%s: %w", name, errNoSuchUser)
}

// Groups lists the group names in use.
func (a *Accounts) Groups() map[string]bool {
	groups := map[string]bool{"root": true}
	for _, g := range groupFor {
		groups[g] = true
	}
	if users, err := a.List(); err == nil {
		for _, u := range users {
			groups[u.Group] = true
		}
	}
	return groups
}

// Authenticate checks a user name and password. Every failure gives the same
// answer, so a caller cannot tell an unknown user from a wrong password.
func (a *Accounts) Authenticate(name, password string) (*User, error) {
	users, hashes, err := a.load()
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.Name == name {
			if verifyPassword(password, hashes[name]) {
				return u, nil
			}
			return nil, errLogin
		}
	}
	verifyPassword(password, dummyHash())
	return nil, errLogin
}

// Add creates an account. The caller creates the home directory.
func (a *Accounts) Add(name string, role Role, password string) (*User, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !validUserName.MatchString(name) {
		return nil, fmt.Errorf("%s: invalid user name (lowercase letters, digits, '_' and '-')", name)
	}
	users, hashes, err := a.load()
	if err != nil {
		return nil, err
	}
	uid := 1000
	for _, u := range users {
		if u.Name == name {
			return nil, fmt.Errorf("%s: %w", name, errUserExists)
		}
		if u.UID >= uid && u.UID < 2000 {
			uid = u.UID + 1
		}
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &User{Name: name, UID: uid, Group: groupFor[role], Role: role, Home: "/home/" + name}
	hashes[name] = hash
	return u, a.save(append(users, u), hashes)
}

// Delete removes an account.
func (a *Accounts) Delete(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	users, hashes, err := a.load()
	if err != nil {
		return err
	}
	for i, u := range users {
		if u.Name == name {
			delete(hashes, name)
			return a.save(append(users[:i], users[i+1:]...), hashes)
		}
	}
	return fmt.Errorf("%s: %w", name, errNoSuchUser)
}

// SetPassword replaces a password.
func (a *Accounts) SetPassword(name, password string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	users, hashes, err := a.load()
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.Name == name {
			hash, err := hashPassword(password)
			if err != nil {
				return err
			}
			hashes[name] = hash
			return a.save(users, hashes)
		}
	}
	return fmt.Errorf("%s: %w", name, errNoSuchUser)
}

func checkPasswordStrength(pw string) error {
	if len(pw) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	return nil
}

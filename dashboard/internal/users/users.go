// Package users manages the regular Linux users (UID 1000-59999, the same range the
// docker-group and fish-default-shell services use): their groups, login shell, password and
// lock, through the standard tools (gpasswd, usermod, useradd, userdel, chpasswd).
// Logging in to the dashboard needs wheel, so the guards keep anyone from locking
// themselves, or everyone, out.
package users

import (
	"bufio"
	"errors"
	"os"
	"os/user"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

// Moved in tests.
var (
	passwdFile = "/etc/passwd"
	groupFile  = "/etc/group"
	shellsFile = "/etc/shells"
)

type User struct {
	Name   string `json:"name"`
	UID    int    `json:"uid"`
	Full   string `json:"full"`
	Home   string `json:"home"`
	Shell  string `json:"shell"`
	Wheel  bool   `json:"wheel"`
	Docker bool   `json:"docker"`
	Locked bool   `json:"locked"`
}

type List struct {
	Users  []User   `json:"users"`
	Shells []string `json:"shells"`
}

func regular(uid int) bool { return uid >= 1000 && uid < 60000 }

// parsePasswd returns the regular users in /etc/passwd order.
func parsePasswd(s string) []User {
	var out []User
	for _, line := range strings.Split(s, "\n") {
		f := strings.Split(line, ":")
		if len(f) < 7 {
			continue
		}
		uid, err := strconv.Atoi(f[2])
		if err != nil || !regular(uid) {
			continue
		}
		full, _, _ := strings.Cut(f[4], ",")
		out = append(out, User{Name: f[0], UID: uid, Full: full, Home: f[5], Shell: f[6]})
	}
	return out
}

// parseGroups maps group name to members, from /etc/group.
func parseGroups(s string) map[string][]string {
	m := map[string][]string{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		var members []string
		for _, u := range strings.Split(f[3], ",") {
			if u = strings.TrimSpace(u); u != "" {
				members = append(members, u)
			}
		}
		m[f[0]] = members
	}
	return m
}

// Shells lists the login shells in /etc/shells that exist, without duplicates through
// /bin -> /usr/bin.
func Shells() []string {
	f, err := os.Open(shellsFile)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "/bin/") || strings.HasPrefix(s, "/sbin/") {
			continue
		}
		if strings.HasSuffix(s, "/nologin") || strings.HasSuffix(s, "/false") {
			continue
		}
		if _, err := os.Stat(s); err == nil && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// inGroup also counts a primary group (the docker or wheel GID as the user's own).
func inGroup(u User, group string, groups map[string][]string) bool {
	if slices.Contains(groups[group], u.Name) {
		return true
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return false
	}
	lu, err := user.Lookup(u.Name)
	return err == nil && lu.Gid == g.Gid
}

func locked(name string) bool {
	out, _ := run.Cmd(5*time.Second, "passwd", "-S", name)
	f := strings.Fields(out)
	return len(f) > 1 && (f[1] == "LK" || f[1] == "L")
}

func Get() (List, error) {
	pw, err := os.ReadFile(passwdFile)
	if err != nil {
		return List{}, err
	}
	gr, _ := os.ReadFile(groupFile)
	groups := parseGroups(string(gr))
	l := List{Users: parsePasswd(string(pw)), Shells: Shells()}
	for i := range l.Users {
		u := &l.Users[i]
		u.Wheel = inGroup(*u, "wheel", groups)
		u.Docker = inGroup(*u, "docker", groups)
		u.Locked = locked(u.Name)
	}
	if l.Users == nil {
		l.Users = []User{}
	}
	return l, nil
}

var nameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func find(name string) (User, List, error) {
	l, err := Get()
	if err != nil {
		return User{}, l, err
	}
	for _, u := range l.Users {
		if u.Name == name {
			return u, l, nil
		}
	}
	return User{}, l, errors.New("no regular user " + name)
}

// wheelCount is how many users could still log in to the dashboard.
func wheelCount(l List, except string) int {
	n := 0
	for _, u := range l.Users {
		if u.Wheel && !u.Locked && u.Name != except {
			n++
		}
	}
	return n
}

// CheckRemoveWheel refuses changes that take the acting user, or the last one, out of wheel.
func CheckRemoveWheel(l List, actor, name string) error {
	if name == actor {
		return errors.New("you can't take yourself out of wheel: you'd lose access to this dashboard")
	}
	if wheelCount(l, name) == 0 {
		return errors.New(name + " is the last user in wheel; nobody could log in to the dashboard")
	}
	return nil
}

// SetGroup adds a user to or removes them from wheel or docker.
func SetGroup(actor, name, group string, member bool) error {
	if group != "wheel" && group != "docker" {
		return errors.New("only wheel and docker can be changed here")
	}
	u, l, err := find(name)
	if err != nil {
		return err
	}
	if group == "wheel" && !member && u.Wheel {
		if err := CheckRemoveWheel(l, actor, name); err != nil {
			return err
		}
	}
	flag := "-d"
	if member {
		flag = "-a"
	}
	_, err = run.Cmd(15*time.Second, "gpasswd", flag, name, group)
	return err
}

func SetShell(name, shell string) error {
	if _, _, err := find(name); err != nil {
		return err
	}
	if !slices.Contains(Shells(), shell) {
		return errors.New("not a login shell in /etc/shells")
	}
	_, err := run.Cmd(15*time.Second, "usermod", "-s", shell, name)
	return err
}

func checkPassword(pw string) error {
	if len(pw) < 8 {
		return errors.New("use at least 8 characters")
	}
	if strings.ContainsAny(pw, "\n\r:") {
		return errors.New("the password can't contain a colon or line break")
	}
	return nil
}

// SetPassword goes through chpasswd's stdin, so it never shows up in the process list.
func SetPassword(name, pw string) error {
	if _, _, err := find(name); err != nil {
		return err
	}
	if err := checkPassword(pw); err != nil {
		return err
	}
	_, err := run.CmdInput(15*time.Second, name+":"+pw+"\n", "chpasswd")
	return err
}

func SetLocked(actor, name string, lock bool) error {
	u, l, err := find(name)
	if err != nil {
		return err
	}
	if lock {
		if name == actor {
			return errors.New("you can't lock yourself out")
		}
		if u.Wheel && wheelCount(l, name) == 0 {
			return errors.New(name + " is the last unlocked user in wheel")
		}
	}
	flag := "-U"
	if lock {
		flag = "-L"
	}
	_, err = run.Cmd(15*time.Second, "usermod", flag, name)
	return err
}

type New struct {
	Name     string `json:"name"`
	Full     string `json:"full"`
	Password string `json:"password"`
	Wheel    bool   `json:"wheel"`
	Docker   bool   `json:"docker"`
	Shell    string `json:"shell"`
}

func Add(n New) error {
	if !nameRe.MatchString(n.Name) {
		return errors.New("a username is lowercase letters, digits, - and _, starting with a letter")
	}
	if _, err := user.Lookup(n.Name); err == nil {
		return errors.New(n.Name + " exists already")
	}
	if err := checkPassword(n.Password); err != nil {
		return err
	}
	if strings.ContainsAny(n.Full, ":,\n") {
		return errors.New("the full name can't contain : or ,")
	}
	args := []string{"-m"}
	if n.Full != "" {
		args = append(args, "-c", n.Full)
	}
	if n.Shell != "" {
		if !slices.Contains(Shells(), n.Shell) {
			return errors.New("not a login shell in /etc/shells")
		}
		args = append(args, "-s", n.Shell)
	}
	var groups []string
	if n.Wheel {
		groups = append(groups, "wheel")
	}
	if n.Docker {
		groups = append(groups, "docker")
	}
	if len(groups) > 0 {
		args = append(args, "-G", strings.Join(groups, ","))
	}
	if _, err := run.Cmd(30*time.Second, "useradd", append(args, n.Name)...); err != nil {
		return err
	}
	_, err := run.CmdInput(15*time.Second, n.Name+":"+n.Password+"\n", "chpasswd")
	return err
}

func Delete(actor, name string, removeHome bool) error {
	u, l, err := find(name)
	if err != nil {
		return err
	}
	if name == actor {
		return errors.New("you can't delete yourself")
	}
	if u.Wheel && wheelCount(l, name) == 0 {
		return errors.New(name + " is the last user in wheel")
	}
	args := []string{}
	if removeHome {
		args = append(args, "-r")
	}
	_, err = run.Cmd(time.Minute, "userdel", append(args, name)...)
	return err
}

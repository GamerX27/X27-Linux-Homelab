package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Args is what the server passes to `dashboard fsop <op> <json>`.
type Args struct {
	Path      string   `json:"path,omitempty"`
	Paths     []string `json:"paths,omitempty"` // several entries; with To, a folder to put them in
	To        string   `json:"to,omitempty"`
	Name      string   `json:"name,omitempty"`
	Mode      string   `json:"mode,omitempty"` // octal, for chmod
	Create    bool     `json:"create,omitempty"`
	Overwrite bool     `json:"overwrite,omitempty"`
	Parents   bool     `json:"parents,omitempty"`
	Recursive bool     `json:"recursive,omitempty"`
}

// each runs fn for Path -> To, or for every entry in Paths -> To/<its name>. It goes
// through all of them and reports every failure.
func each(a Args, fn func(from, to string) error) error {
	if len(a.Paths) == 0 {
		return fn(a.Path, a.To)
	}
	var msgs []string
	for _, p := range a.Paths {
		name := path.Base(strings.Trim(p, "/"))
		if err := fn(p, path.Join(a.To, name)); err != nil {
			msg := err.Error()
			if !strings.HasPrefix(msg, strings.Trim(p, "/")) {
				msg = name + ": " + msg
			}
			msgs = append(msgs, msg)
		}
	}
	if len(msgs) > 0 {
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
}

type Stat struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

// Fsop runs one operation as the current user (the process runuser started) and returns the
// exit code. Results go to stdout as JSON (or raw bytes for download), errors to stderr.
func Fsop(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprint(stderr, err.Error())
		return 1
	}
	if len(args) != 2 {
		return fail(errors.New("usage: dashboard fsop <op> <json>"))
	}
	var a Args
	if err := json.Unmarshal([]byte(args[1]), &a); err != nil {
		return fail(err)
	}
	u, err := user.Current()
	if err != nil {
		return fail(err)
	}
	home := u.HomeDir
	out := func(v any, err error) int {
		if err != nil {
			return fail(err)
		}
		json.NewEncoder(stdout).Encode(v)
		return 0
	}
	switch args[0] {
	case "list":
		l, err := List(home, a.Path)
		return out(map[string]any{"home": home, "entries": l}, err)
	case "read":
		s, err := Read(home, a.Path)
		return out(map[string]string{"content": s}, err)
	case "write":
		b, err := io.ReadAll(io.LimitReader(stdin, MaxEdit+1))
		if err == nil && len(b) > MaxEdit {
			err = errors.New("the file is too large to save here (over 1 MB)")
		}
		if err != nil {
			return fail(err)
		}
		return out(map[string]bool{"ok": true}, Write(home, a.Path, string(b), a.Create))
	case "mkdir":
		return out(map[string]bool{"ok": true}, Mkdir(home, a.Path))
	case "move":
		return out(map[string]bool{"ok": true}, each(a, func(from, to string) error { return Move(home, from, to) }))
	case "copy":
		return out(map[string]bool{"ok": true}, each(a, func(from, to string) error { return Copy(home, from, to) }))
	case "delete":
		return out(map[string]bool{"ok": true}, each(a, func(p, _ string) error { return Delete(home, p) }))
	case "chmod":
		m, err := strconv.ParseUint(a.Mode, 8, 32)
		if err != nil || m > 0o777 {
			return fail(errors.New("permissions must be three octal digits, like 644"))
		}
		return out(map[string]bool{"ok": true}, each(a, func(p, _ string) error { return Chmod(home, p, os.FileMode(m), a.Recursive) }))
	case "extract":
		p, err := Extract(home, a.Path)
		return out(map[string]string{"path": p}, err)
	case "compress":
		p, err := Compress(home, a.Paths)
		return out(map[string]string{"path": p}, err)
	case "zip":
		if err := Zip(home, a.Paths, stdout); err != nil {
			return fail(err)
		}
		return 0
	case "upload":
		n, err := Upload(home, a.Path, a.Name, a.Overwrite, a.Parents, stdin)
		return out(map[string]int64{"size": n}, err)
	case "stat":
		p, err := Resolve(home, a.Path)
		if err != nil {
			return fail(err)
		}
		st, err := os.Stat(p)
		if err != nil {
			return fail(clean(err, home))
		}
		name := filepath.Base(p)
		if p == filepath.Clean(home) {
			name = u.Username
		}
		return out(Stat{Name: name, IsDir: st.IsDir(), Size: st.Size()}, nil)
	case "download":
		if err := Download(home, a.Path, stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	return fail(errors.New("unknown operation"))
}

// Runner starts `dashboard fsop` as the given user. In --dev it runs as whoever runs the
// server, without runuser.
type Runner struct{ Dev bool }

func (r Runner) command(ctx context.Context, username, op string, a Args) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(a)
	if r.Dev {
		return exec.CommandContext(ctx, self, "fsop", op, string(b)), nil
	}
	if _, err := user.Lookup(username); err != nil || username == "" {
		return nil, fmt.Errorf("there's no user %s on this node", username)
	}
	return exec.CommandContext(ctx, "runuser", "-u", username, "--", self, "fsop", op, string(b)), nil
}

func opError(stderr *bytes.Buffer, err error) error {
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return errors.New(msg)
	}
	return err
}

// Run executes op, feeding stdin, and decodes its JSON result into out.
func (r Runner) Run(username, op string, a Args, stdin io.Reader, out any) error {
	timeout := 30 * time.Second
	switch op {
	case "upload", "write", "delete", "move", "copy", "chmod", "extract", "compress":
		timeout = 2 * time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd, err := r.command(ctx, username, op, a)
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return opError(&stderr, err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(stdout.Bytes(), out)
}

// Stream runs a download (op "download" or "zip") straight into w.
func (r Runner) Stream(ctx context.Context, username, op string, a Args, w io.Writer) error {
	cmd, err := r.command(ctx, username, op, a)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := cmd.Run(); err != nil {
		return opError(&stderr, err)
	}
	return nil
}

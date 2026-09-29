package docker

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

// A stack is a compose project in the user's ~/docker/<name>/ (the same default as
// docker-compose-update). The dashboard can create and edit these; compose projects
// started from anywhere else are listed too, but only managed, not edited.

var composeNames = []string{"compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml"}

var stackNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func ValidStackName(name string) bool { return stackNameRe.MatchString(name) }

// ProjectName is the project name docker compose gives a folder: lower case, with
// anything but letters, digits, - and _ dropped ("Nginx-Proxy-Manager" → "nginx-proxy-manager").
func ProjectName(folder string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(folder) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		}
	}
	return strings.TrimLeft(sb.String(), "-_")
}

type Owner struct {
	Name     string
	Home     string
	UID, GID int
}

// LookupOwner finds the user whose ~/docker holds the stacks.
func LookupOwner(username string) (Owner, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return Owner{}, fmt.Errorf("there's no user %s on this node", username)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return Owner{Name: u.Username, Home: u.HomeDir, UID: uid, GID: gid}, nil
}

func (o Owner) Root() string { return filepath.Join(o.Home, "docker") }

type Stack struct {
	Name       string   `json:"name"`
	Dir        string   `json:"dir"`
	File       string   `json:"file"`
	InRoot     bool     `json:"inRoot"` // in ~/docker, so the UI may edit it
	FilesExist bool     `json:"filesExist"`
	Running    int      `json:"running"`
	Total      int      `json:"total"`
	Containers []string `json:"containers"`
	Ports      []Port   `json:"ports"`
}

func findComposeFile(dir string) string {
	for _, n := range composeNames {
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil && st.Mode().IsRegular() {
			return filepath.Join(dir, n)
		}
	}
	return ""
}

// ListStacks merges the folders in ~/docker with the compose projects Docker knows about.
// A folder's project name is its name, as `docker compose` defaults to.
func ListStacks(o Owner, cs []Container) []Stack {
	byName := map[string]*Stack{}
	root := o.Root()
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		f := findComposeFile(dir)
		if f == "" {
			continue
		}
		name := ProjectName(e.Name())
		if !ValidStackName(name) || byName[name] != nil {
			continue
		}
		byName[name] = &Stack{Name: name, Dir: dir, File: f, InRoot: true, FilesExist: true}
	}
	for _, p := range Projects(cs) {
		s := byName[p.Name]
		if s == nil {
			s = &Stack{Name: p.Name, Dir: p.Dir, FilesExist: p.FilesExist}
			if len(p.Files) > 0 {
				s.File = p.Files[0]
			}
			byName[p.Name] = s
		}
		s.Running, s.Total, s.Containers = p.Running, p.Total, p.Containers
	}
	for _, c := range cs {
		if s := byName[c.Labels[labelProject]]; s != nil && c.State == "running" {
			s.Ports = append(s.Ports, c.Ports...)
		}
	}
	out := make([]Stack, 0, len(byName))
	for _, s := range byName {
		out = append(out, *s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// StackDir resolves a stack name to its folder in ~/docker, refusing anything else. An
// existing folder whose project name matches is used as it is (~/docker/Jellyfin for
// "jellyfin"); otherwise the folder is the name itself.
func StackDir(o Owner, name string) (string, error) {
	if !ValidStackName(name) {
		return "", errors.New("a stack name is lower-case letters, digits, - and _ (up to 64)")
	}
	entries, _ := os.ReadDir(o.Root())
	for _, e := range entries {
		if e.IsDir() && ProjectName(e.Name()) == name {
			return filepath.Join(o.Root(), e.Name()), nil
		}
	}
	return filepath.Join(o.Root(), name), nil
}

type StackFiles struct {
	Compose string `json:"compose"`
	Env     string `json:"env"`
	File    string `json:"file"`
}

func ReadStack(o Owner, name string) (StackFiles, error) {
	dir, err := StackDir(o, name)
	if err != nil {
		return StackFiles{}, err
	}
	f := findComposeFile(dir)
	if f == "" {
		return StackFiles{}, errors.New("no compose file in " + dir)
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return StackFiles{}, err
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	return StackFiles{Compose: string(b), Env: string(env), File: f}, nil
}

// SaveStack validates with `docker compose config` before anything is written, then writes
// the files owned by the user. Editing keeps the previous compose file as <file>.bak.
func SaveStack(o Owner, name, compose, env string, create bool) (string, error) {
	dir, err := StackDir(o, name)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(compose) == "" {
		return "", errors.New("the compose file is empty")
	}
	if len(compose) > 512<<10 || len(env) > 128<<10 {
		return "", errors.New("file too large")
	}
	file := filepath.Join(dir, "compose.yml")
	if create {
		if _, err := os.Stat(dir); err == nil {
			return "", fmt.Errorf("%s already exists", dir)
		}
	} else {
		if f := findComposeFile(dir); f != "" {
			file = f
		} else {
			return "", errors.New("no compose file in " + dir)
		}
	}

	// Validate from temp files, resolving relative paths against the real folder.
	tmp, err := os.MkdirTemp("", "dashboard-stack-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	tc := filepath.Join(tmp, "compose.yml")
	if err := os.WriteFile(tc, []byte(compose), 0o600); err != nil {
		return "", err
	}
	args := []string{"compose", "-p", name, "--project-directory", dir, "-f", tc}
	if env != "" {
		te := filepath.Join(tmp, ".env")
		if err := os.WriteFile(te, []byte(env), 0o600); err != nil {
			return "", err
		}
		args = append(args, "--env-file", te)
	}
	if out, err := run.Cmd(30*time.Second, "docker", append(args, "config", "-q")...); err != nil {
		return out, fmt.Errorf("the compose file isn't valid:\n%s", strings.ReplaceAll(err.Error(), tc, filepath.Base(file)))
	}

	if err := ensureDir(o, o.Root()); err != nil {
		return "", err
	}
	if err := ensureDir(o, dir); err != nil {
		return "", err
	}
	if !create {
		if old, err := os.ReadFile(file); err == nil {
			if err := writeOwned(o, file+".bak", old, 0o644); err != nil {
				return "", err
			}
		}
	}
	if err := writeOwned(o, file, []byte(compose), 0o644); err != nil {
		return "", err
	}
	envPath := filepath.Join(dir, ".env")
	if env != "" {
		if err := writeOwned(o, envPath, []byte(env), 0o600); err != nil {
			return "", err
		}
	} else if !create {
		os.Remove(envPath)
	}
	return "Saved " + file, nil
}

func ensureDir(o Owner, dir string) error {
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return errors.New(dir + " isn't a folder")
		}
		return nil
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	return os.Chown(dir, o.UID, o.GID)
}

func writeOwned(o Owner, path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chown(tmp, o.UID, o.GID); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// stackProject resolves a stack (one in ~/docker, or a compose project Docker already
// runs) to the compose project to run commands on. needsFiles refuses projects whose
// compose files aren't on this host; otherwise those can only be addressed by name.
func (c *Client) stackProject(o Owner, name string, needsFiles bool) (Project, error) {
	cs, err := c.Containers()
	if err != nil {
		return Project{}, err
	}
	stacks := ListStacks(o, cs)
	i := slices.IndexFunc(stacks, func(s Stack) bool { return s.Name == name })
	if i < 0 {
		return Project{}, errors.New("no such stack")
	}
	s := stacks[i]
	if !s.FilesExist {
		if needsFiles {
			return Project{}, errors.New("the compose files for this stack aren't on this host (Portainer or another tool manages it)")
		}
		return Project{Name: s.Name}, nil
	}
	p := Project{Name: s.Name, Dir: s.Dir, FilesExist: true}
	if s.File != "" {
		p.Files = []string{s.File}
	}
	return p, nil
}

// StackAction runs start/stop/restart/recreate/remove on a stack.
func (c *Client) StackAction(o Owner, name, action string) (string, error) {
	// Projects from files that aren't on this host can only be stopped/restarted/removed.
	p, err := c.stackProject(o, name, action == "start" || action == "recreate")
	if err != nil {
		return "", err
	}
	dc := func(args ...string) (string, error) {
		return run.Cmd(15*time.Minute, "docker", composeArgs(p, args...)...)
	}
	switch action {
	case "start":
		return dc("up", "-d")
	case "stop":
		return dc("stop")
	case "restart":
		return dc("restart")
	case "remove":
		return dc("down")
	case "recreate":
		out, err := dc("pull")
		if err != nil {
			return out, err
		}
		out2, err := dc("up", "-d", "--force-recreate", "--remove-orphans")
		return strings.TrimSpace(out + "\n" + out2), err
	}
	return "", errors.New("unknown stack action")
}

// RemoveStack runs `docker compose down` and, when asked, deletes the stack's folder in
// ~/docker with everything in it (including root-owned data Docker created). A folder that
// is a symlink is unlinked, never followed.
func (c *Client) RemoveStack(o Owner, name string, deleteFolder bool) (string, error) {
	out, err := c.StackAction(o, name, "remove")
	if err != nil || !deleteFolder {
		return out, err
	}
	msg, err := DeleteStackFolder(o, name)
	return strings.TrimSpace(out + "\n" + msg), err
}

func DeleteStackFolder(o Owner, name string) (string, error) {
	dir, err := StackDir(o, name)
	if err != nil {
		return "", err
	}
	root := filepath.Clean(o.Root())
	if filepath.Dir(dir) != root || dir == root {
		return "", errors.New("refusing to delete " + dir)
	}
	st, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return "Removed the link " + dir + " (its target was left alone).", os.Remove(dir)
	}
	if !st.IsDir() {
		return "", errors.New(dir + " isn't a folder")
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	return "Deleted " + dir + ".", nil
}

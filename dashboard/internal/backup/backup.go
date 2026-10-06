// Package backup archives compose stacks from ~/docker on a schedule or on demand. Each app
// is stopped with `docker compose stop`, its folder (compose file, .env and bind-mounted
// data) is packed into <dest>/<app>/<app>-YYYYMMDD-HHMMSS.tar.gz, and the app is started
// again as it was. Older archives beyond the configured count are deleted.
package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/features"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

const (
	DefaultKeep = 7
	notifyCmd   = "/usr/libexec/autoupdate-notify" // Gotify, set up under Features
)

var ErrRunning = errors.New("a backup is already running on this node")

// archiveRe matches the archives this package writes; nothing else in the folder is touched.
var archiveRe = regexp.MustCompile(`^([a-z0-9][a-z0-9_-]{0,63})-(\d{8}-\d{6})\.tar\.gz$`)

type Config struct {
	Owner   string   `json:"owner"` // whose ~/docker is backed up: the user who saved this
	Apps    []string `json:"apps"`
	Dest    string   `json:"dest"` // empty: ~/backups/docker
	Keep    int      `json:"keep"`
	Freq    string   `json:"freq"` // off, daily, weekly, monthly
	Weekday string   `json:"weekday"`
	Day     string   `json:"day"`
	Time    string   `json:"time"`
	Since   int64    `json:"since"`   // when the schedule was set: runs are counted from here
	LastRun int64    `json:"lastRun"` // when the last scheduled run started
}

func (c Config) scheduled() bool { return c.Freq != "" && c.Freq != "off" }

// Job is one backup run in the background, polled by the UI like a container update.
type Job struct {
	ID         int64    `json:"id"`
	Kind       string   `json:"kind"`  // manual, scheduled
	Phase      string   `json:"phase"` // starting, stopping, archiving, starting-again, done, failed
	Current    string   `json:"current,omitempty"`
	Pending    []string `json:"pending,omitempty"`
	Done       int      `json:"done"`
	Failed     int      `json:"failed"`
	Error      string   `json:"error,omitempty"`
	StartedAt  int64    `json:"startedAt"`
	FinishedAt int64    `json:"finishedAt,omitempty"`
}

func (j *Job) Running() bool { return j.FinishedAt == 0 }

type Archive struct {
	App  string `json:"app"`
	File string `json:"file"`
	Size int64  `json:"size"`
	Time int64  `json:"time"`
}

type Status struct {
	Config   Config    `json:"config"`
	Dest     string    `json:"dest"` // the folder in use
	NextRun  int64     `json:"nextRun,omitempty"`
	Job      *Job      `json:"job,omitempty"`
	Archives []Archive `json:"archives"`
}

type Manager struct {
	dk      *docker.Client
	path    string
	mu      sync.Mutex
	cfg     Config
	job     *Job
	log     []string
	running sync.Mutex // one backup run at a time
}

// New loads the saved settings and starts the scheduler.
func New(dk *docker.Client) *Manager {
	m := &Manager{dk: dk, path: filepath.Join(config.StateDir, "backups.json"), cfg: Config{Keep: DefaultKeep, Freq: "off"}}
	if b, err := os.ReadFile(m.path); err == nil {
		if err := json.Unmarshal(b, &m.cfg); err != nil {
			log.Printf("backups: reading %s: %v", m.path, err)
		}
	}
	go func() {
		for {
			time.Sleep(time.Minute)
			m.tick(time.Now())
		}
	}()
	return m
}

func (m *Manager) save() error {
	b, _ := json.MarshalIndent(m.cfg, "", "  ")
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// DestFor is where the archives go: the configured folder, or ~/backups/docker.
func DestFor(c Config, o docker.Owner) string {
	if c.Dest != "" {
		return c.Dest
	}
	return filepath.Join(o.Home, "backups", "docker")
}

// ValidDest refuses relative paths, / and anything inside ~/docker, where the archive
// would end up in the folders it packs.
func ValidDest(o docker.Owner, dest string) error {
	d := filepath.Clean(dest)
	if !filepath.IsAbs(d) {
		return errors.New("the backup folder must be an absolute path, like /mnt/backups")
	}
	if d == "/" {
		return errors.New("pick a folder for the backups, not /")
	}
	root := filepath.Clean(o.Root())
	if d == root || strings.HasPrefix(d, root+"/") {
		return errors.New("keep the backups outside " + root + ", the folder being backed up")
	}
	return nil
}

// Next is the first scheduled time after `after`, or zero when no schedule is set.
// A day of month that a month doesn't have is skipped, as systemd timers do.
func Next(c Config, after time.Time) time.Time {
	var hh, mm int
	if !c.scheduled() {
		return time.Time{}
	}
	if _, err := fmt.Sscanf(c.Time, "%d:%d", &hh, &mm); err != nil {
		return time.Time{}
	}
	y, mo, d := after.Date()
	for i := 0; i <= 400; i++ {
		t := time.Date(y, mo, d+i, hh, mm, 0, 0, after.Location())
		if !t.After(after) {
			continue
		}
		switch c.Freq {
		case "daily":
			return t
		case "weekly":
			if strings.ToLower(t.Weekday().String()[:3]) == c.Weekday {
				return t
			}
		case "monthly":
			if fmt.Sprint(t.Day()) == c.Day {
				return t
			}
		}
	}
	return time.Time{}
}

func (c Config) nextRun() time.Time {
	return Next(c, time.Unix(max(c.LastRun, c.Since), 0))
}

func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cfg
	c.Apps = slices.Clone(c.Apps)
	return c
}

// Save validates and stores the settings from the UI; o is the user saving them.
func (m *Manager) Save(o docker.Owner, in Config) error {
	apps := []string{}
	for _, a := range in.Apps {
		if !docker.ValidStackName(a) {
			return fmt.Errorf("%q isn't a stack name", a)
		}
		if !slices.Contains(apps, a) {
			apps = append(apps, a)
		}
	}
	sort.Strings(apps)
	if in.Keep == 0 {
		in.Keep = DefaultKeep
	}
	if in.Keep < 1 || in.Keep > 365 {
		return errors.New("keep between 1 and 365 backups per app")
	}
	if in.Freq == "" {
		in.Freq = "off"
	}
	if in.Freq != "off" {
		if _, err := features.ScheduleArgs(in.Freq, in.Weekday, in.Day, in.Time); err != nil {
			return err
		}
		in.Weekday = strings.ToLower(in.Weekday)
	}
	in.Dest = strings.TrimSpace(in.Dest)
	if in.Dest != "" {
		if err := ValidDest(o, in.Dest); err != nil {
			return err
		}
		in.Dest = filepath.Clean(in.Dest)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.cfg
	c := Config{Owner: o.Name, Apps: apps, Dest: in.Dest, Keep: in.Keep, Freq: in.Freq,
		Weekday: in.Weekday, Day: in.Day, Time: in.Time, Since: old.Since, LastRun: old.LastRun}
	if c.Freq != old.Freq || c.Weekday != old.Weekday || c.Day != old.Day || c.Time != old.Time {
		c.Since = time.Now().Unix() // don't run at once for a time that already passed
	}
	m.cfg = c
	return m.save()
}

func (m *Manager) Status(o docker.Owner) Status {
	m.mu.Lock()
	c := m.cfg
	c.Apps = slices.Clone(c.Apps)
	var j *Job
	if m.job != nil {
		cp := *m.job
		cp.Pending = slices.Clone(cp.Pending)
		j = &cp
	}
	m.mu.Unlock()
	st := Status{Config: c, Dest: DestFor(c, o), Job: j, Archives: Archives(DestFor(c, o))}
	if t := c.nextRun(); !t.IsZero() {
		st.NextRun = t.Unix()
	}
	return st
}

// Log is the output of the running or last backup.
func (m *Manager) Log() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.log, "\n")
}

func (m *Manager) logf(format string, a ...any) {
	s := strings.TrimSpace(fmt.Sprintf(format, a...))
	if s == "" {
		return
	}
	m.mu.Lock()
	m.log = append(m.log, s)
	m.mu.Unlock()
}

func (m *Manager) setJob(f func(j *Job)) {
	m.mu.Lock()
	f(m.job)
	m.mu.Unlock()
}

func (m *Manager) phase(p string) { m.setJob(func(j *Job) { j.Phase = p }) }

// tick starts the scheduled run once it's due, including one missed while the node was off.
func (m *Manager) tick(now time.Time) {
	c := m.Config()
	next := c.nextRun()
	if next.IsZero() || next.After(now) {
		return
	}
	o, err := docker.LookupOwner(c.Owner)
	if err == nil {
		_, err = m.Start(o, "scheduled", nil)
	}
	if errors.Is(err, ErrRunning) {
		return // try again in a minute
	}
	if err != nil {
		log.Printf("backups: scheduled run: %v", err)
		notify("Backup failed on "+hostname(), err.Error())
	}
	m.mu.Lock()
	m.cfg.LastRun = now.Unix()
	if err := m.save(); err != nil {
		log.Printf("backups: saving %s: %v", m.path, err)
	}
	m.mu.Unlock()
}

// Start backs up apps (the configured ones when empty) in the background.
func (m *Manager) Start(o docker.Owner, kind string, apps []string) (Job, error) {
	c := m.Config()
	if len(apps) == 0 {
		apps = c.Apps
	}
	if len(apps) == 0 {
		return Job{}, errors.New("pick at least one app to back up")
	}
	for _, a := range apps {
		if !docker.ValidStackName(a) {
			return Job{}, fmt.Errorf("%q isn't a stack name", a)
		}
	}
	dest := DestFor(c, o)
	if err := ValidDest(o, dest); err != nil {
		return Job{}, err
	}
	if !m.running.TryLock() {
		return Job{}, ErrRunning
	}
	m.mu.Lock()
	m.job = &Job{ID: time.Now().UnixMilli(), Kind: kind, Phase: "starting", Pending: slices.Clone(apps), StartedAt: time.Now().Unix()}
	m.log = nil
	j := *m.job
	m.mu.Unlock()
	go func() {
		defer m.running.Unlock()
		var failed []string
		for _, a := range apps {
			m.setJob(func(j *Job) {
				j.Current, j.Phase = a, "starting"
				j.Pending = slices.DeleteFunc(j.Pending, func(p string) bool { return p == a })
			})
			if err := m.backupApp(o, dest, c.Keep, a); err != nil {
				m.logf("%s: failed: %v", a, err)
				failed = append(failed, a)
				m.setJob(func(j *Job) { j.Failed++ })
			} else {
				m.setJob(func(j *Job) { j.Done++ })
			}
		}
		m.mu.Lock()
		j := m.job
		j.FinishedAt, j.Current, j.Pending = time.Now().Unix(), "", nil
		if j.Failed > 0 {
			j.Phase, j.Error = "failed", fmt.Sprintf("%d of %d failed: %s", j.Failed, len(apps), strings.Join(failed, ", "))
		} else {
			j.Phase = "done"
		}
		log.Printf("backup (%s): %s, %d done, %d failed", kind, j.Phase, j.Done, j.Failed)
		msg := j.Error
		m.mu.Unlock()
		if kind == "scheduled" && len(failed) > 0 {
			notify("Backup failed on "+hostname(), msg)
		}
	}()
	return j, nil
}

// backupApp stops a running stack, archives its folder, and always starts it again.
func (m *Manager) backupApp(o docker.Owner, dest string, keep int, app string) (err error) {
	cs, err := m.dk.Containers()
	if err != nil {
		return err
	}
	stacks := docker.ListStacks(o, cs)
	i := slices.IndexFunc(stacks, func(s docker.Stack) bool { return s.Name == app && s.InRoot })
	if i < 0 {
		return errors.New("there's no stack " + app + " in " + o.Root())
	}
	s := stacks[i]

	if s.Running > 0 {
		defer func() {
			m.phase("starting-again")
			m.logf("%s: starting again", app)
			out, serr := m.dk.StackAction(o, app, "resume")
			m.logf("%s", out)
			if serr != nil && err == nil {
				err = fmt.Errorf("starting it again: %w", serr)
			}
		}()
		m.phase("stopping")
		m.logf("%s: stopping %d container(s)", app, s.Running)
		out, serr := m.dk.StackAction(o, app, "stop")
		m.logf("%s", out)
		if serr != nil {
			return fmt.Errorf("stopping: %w", serr)
		}
	}

	m.phase("archiving")
	dir := filepath.Join(dest, app)
	if err := mkdirOwned(o, dir); err != nil {
		return err
	}
	name := app + "-" + time.Now().Format("20060102-150405") + ".tar.gz"
	final := filepath.Join(dir, name)
	tmp := final + ".partial"
	m.logf("%s: packing %s", app, s.Dir)
	// Runs as root: containers often leave root-owned files in their bind mounts.
	out, err := run.Cmd(6*time.Hour, "tar", "--numeric-owner", "-czf", tmp, "-C", filepath.Dir(s.Dir), filepath.Base(s.Dir))
	m.logf("%s", out)
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("archiving: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil { // .env files hold secrets
		os.Remove(tmp)
		return err
	}
	if err := os.Chown(tmp, o.UID, o.GID); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	size := int64(0)
	if st, err := os.Stat(final); err == nil {
		size = st.Size()
	}
	m.logf("%s: wrote %s (%d bytes)", app, final, size)
	for _, f := range Prune(dir, app, keep) {
		m.logf("%s: deleted old backup %s", app, f)
	}
	return nil
}

// mkdirOwned creates dir and any missing parents, owned by the user and private to them.
func mkdirOwned(o docker.Owner, dir string) error {
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return errors.New(dir + " isn't a folder")
		}
		return nil
	}
	if err := mkdirOwned(o, filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return os.Chown(dir, o.UID, o.GID)
}

// archives lists an app folder's backups, oldest first (the names sort by time).
func archives(dir, app string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if m := archiveRe.FindStringSubmatch(e.Name()); m != nil && m[1] == app && e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Prune deletes all but the newest keep backups of app and returns what it deleted.
func Prune(dir, app string, keep int) []string {
	list := archives(dir, app)
	if keep < 1 || len(list) <= keep {
		return nil
	}
	var gone []string
	for _, f := range list[:len(list)-keep] {
		if os.Remove(filepath.Join(dir, f)) == nil {
			gone = append(gone, f)
		}
	}
	return gone
}

// Archives lists every backup under dest, newest first.
func Archives(dest string) []Archive {
	out := []Archive{}
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if !e.IsDir() || !docker.ValidStackName(e.Name()) {
			continue
		}
		for _, f := range archives(filepath.Join(dest, e.Name()), e.Name()) {
			st, err := os.Stat(filepath.Join(dest, e.Name(), f))
			if err != nil {
				continue
			}
			ts := archiveRe.FindStringSubmatch(f)[2]
			t, _ := time.ParseInLocation("20060102-150405", ts, time.Local)
			out = append(out, Archive{App: e.Name(), File: f, Size: st.Size(), Time: t.Unix()})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Time > out[b].Time })
	return out
}

// Delete removes one backup the user picked in the UI.
func (m *Manager) Delete(o docker.Owner, app, file string) error {
	mm := archiveRe.FindStringSubmatch(file)
	if !docker.ValidStackName(app) || mm == nil || mm[1] != app {
		return errors.New("not a backup file")
	}
	return os.Remove(filepath.Join(DestFor(m.Config(), o), app, file))
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// notify sends a Gotify message when Gotify is set up; autoupdate-notify does nothing otherwise.
func notify(title, msg string) {
	if _, err := os.Stat(notifyCmd); err != nil {
		return
	}
	if out, err := run.Cmd(time.Minute, notifyCmd, "8", title, msg); err != nil {
		log.Printf("backups: gotify: %v %s", err, out)
	}
}

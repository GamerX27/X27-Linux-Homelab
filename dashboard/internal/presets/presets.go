// Package presets reads ready-made compose files from a Forgejo/Gitea repository
// (default https://codeberg.org/X27/Docker-X27-Composes): one folder per service under
// Composes/, each with compose.yml and optionally note.txt. The main node fetches them
// for the "New stack" dialog; nothing is run from them until the user saves the stack.
package presets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	branch   = "main"
	folder   = "Composes"
	cacheFor = time.Hour
	maxFile  = 256 << 10
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

type Preset struct {
	Name    string `json:"name"`
	HasNote bool   `json:"hasNote"`
}

type Files struct {
	Name    string `json:"name"`
	Compose string `json:"compose"`
	Note    string `json:"note"`
}

type Source struct {
	base, owner, repo string // https://codeberg.org, X27, Docker-X27-Composes
	client            *http.Client

	mu     sync.Mutex
	list   []Preset
	listAt time.Time
	files  map[string]Files
	fileAt map[string]time.Time
}

// New parses a repository URL like https://codeberg.org/X27/Docker-X27-Composes.
func New(repoURL string) (*Source, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(repoURL), ".git"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("bad presets repository %q", repoURL)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || !nameRe.MatchString(parts[0]) || !nameRe.MatchString(parts[1]) {
		return nil, fmt.Errorf("presets repository must look like https://host/owner/repo, got %q", repoURL)
	}
	return &Source{base: u.Scheme + "://" + u.Host, owner: parts[0], repo: parts[1],
		client: &http.Client{Timeout: 10 * time.Second},
		files:  map[string]Files{}, fileAt: map[string]time.Time{}}, nil
}

func (s *Source) URL() string { return s.base + "/" + s.owner + "/" + s.repo }

func (s *Source) get(u string) ([]byte, int, error) {
	resp, err := s.client.Get(u)
	if err != nil {
		return nil, 0, fmt.Errorf("couldn't reach %s: %w", s.base, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(b) > maxFile {
		return nil, resp.StatusCode, errors.New("file too large")
	}
	return b, resp.StatusCode, nil
}

type tree struct {
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	} `json:"tree"`
}

// ParseTree picks the Composes/<name>/ folders that hold a compose.yml from a git tree listing.
func ParseTree(b []byte) ([]Preset, error) {
	var t tree
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	compose, note := map[string]bool{}, map[string]bool{}
	for _, e := range t.Tree {
		parts := strings.Split(e.Path, "/")
		if e.Type != "blob" || len(parts) != 3 || parts[0] != folder || !nameRe.MatchString(parts[1]) {
			continue
		}
		switch parts[2] {
		case "compose.yml":
			compose[parts[1]] = true
		case "note.txt":
			note[parts[1]] = true
		}
	}
	out := make([]Preset, 0, len(compose))
	for n := range compose {
		out = append(out, Preset{Name: n, HasNote: note[n]})
	}
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name) })
	return out, nil
}

func (s *Source) List() ([]Preset, error) {
	s.mu.Lock()
	if s.list != nil && time.Since(s.listAt) < cacheFor {
		defer s.mu.Unlock()
		return s.list, nil
	}
	s.mu.Unlock()
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/git/trees/%s?recursive=1&per_page=10000", s.base, s.owner, s.repo, branch)
	b, code, err := s.get(u)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d for the preset list", s.base, code)
	}
	list, err := ParseTree(b)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.list, s.listAt = list, time.Now()
	s.mu.Unlock()
	return list, nil
}

func (s *Source) raw(name, file string) (string, int, error) {
	u := fmt.Sprintf("%s/%s/%s/raw/branch/%s/%s/%s/%s", s.base, s.owner, s.repo, branch, folder, url.PathEscape(name), file)
	b, code, err := s.get(u)
	return string(b), code, err
}

func (s *Source) Get(name string) (Files, error) {
	if !nameRe.MatchString(name) || name == "." || name == ".." {
		return Files{}, errors.New("bad preset name")
	}
	s.mu.Lock()
	if f, ok := s.files[name]; ok && time.Since(s.fileAt[name]) < cacheFor {
		s.mu.Unlock()
		return f, nil
	}
	s.mu.Unlock()
	compose, code, err := s.raw(name, "compose.yml")
	if err != nil {
		return Files{}, err
	}
	if code != http.StatusOK {
		return Files{}, fmt.Errorf("no preset %s (%d)", name, code)
	}
	f := Files{Name: name, Compose: compose}
	if note, code, err := s.raw(name, "note.txt"); err == nil && code == http.StatusOK {
		f.Note = note
	}
	s.mu.Lock()
	s.files[name], s.fileAt[name] = f, time.Now()
	s.mu.Unlock()
	return f, nil
}

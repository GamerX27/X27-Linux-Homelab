// Package tools is what an AI may do on the dashboard: one registry of tools, served to
// external clients over MCP and to the built-in assistant. Every tool calls the dashboard's
// own HTTP API (through Dispatch) as the person it acts for, so it gets the same validation,
// node proxying and log lines as a click in the UI.
//
// Scopes: read only looks; operate starts, stops, updates and backs up apps; admin changes
// the OS, its users and services. Nothing here reads or writes files, opens a terminal, or
// returns a secret (Gotify token, .env values, passwords, setup keys).
package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Scope int

const (
	Read Scope = iota
	Operate
	Admin
)

var scopeNames = []string{"read", "operate", "admin"}

func (s Scope) String() string {
	if s < 0 || int(s) >= len(scopeNames) {
		return "read"
	}
	return scopeNames[s]
}

func (s Scope) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func (s *Scope) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	p, err := ParseScope(v)
	*s = p
	return err
}

func ParseScope(v string) (Scope, error) {
	i := slices.Index(scopeNames, v)
	if i < 0 {
		return Read, errors.New("scope must be read, operate or admin")
	}
	return Scope(i), nil
}

// Dispatch runs one request against the dashboard API as user and returns the JSON reply.
type Dispatch func(user, method, path string, body any) (json.RawMessage, error)

// Call is one tool call: who it's for and how to reach the API.
type Call struct {
	User string
	Do   Dispatch
	Args map[string]any
}

type Tool struct {
	Name        string
	Description string
	Scope       Scope
	Params      map[string]any // properties of the JSON-schema object
	Required    []string
	Run         func(c Call) (any, error)
}

// Schema is the tool's input as a JSON-schema object.
func (t Tool) Schema() map[string]any {
	props := t.Params
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(t.Required) > 0 {
		s["required"] = t.Required
	}
	return s
}

// For lists the tools a scope may use.
func For(s Scope) []Tool {
	var out []Tool
	for _, t := range All {
		if t.Scope <= s {
			out = append(out, t)
		}
	}
	return out
}

func Find(name string) (Tool, bool) {
	for _, t := range All {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Run validates the arguments against the tool's parameters and runs it.
func (t Tool) Exec(c Call) (any, error) {
	if c.Args == nil {
		c.Args = map[string]any{}
	}
	for k := range c.Args {
		if _, ok := t.Params[k]; !ok {
			return nil, fmt.Errorf("unknown argument %q", k)
		}
	}
	for _, k := range t.Required {
		if v, ok := c.Args[k]; !ok || v == nil || v == "" {
			return nil, fmt.Errorf("missing argument %q", k)
		}
	}
	for k, p := range t.Params {
		if enum, ok := p.(map[string]any)["enum"].([]string); ok {
			if v, set := c.Args[k]; set && !slices.Contains(enum, fmt.Sprint(v)) {
				return nil, fmt.Errorf("%s must be one of %s", k, strings.Join(enum, ", "))
			}
		}
	}
	return t.Run(c)
}

// ---- Argument helpers ----------------------------------------------------------------

func (c Call) str(k string) string {
	switch v := c.Args[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (c Call) num(k string, def int) int {
	switch v := c.Args[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (c Call) boolean(k string) bool {
	switch v := c.Args[k].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "yes" || v == "1"
	}
	return false
}

func (c Call) list(k string) []string {
	var out []string
	switch v := c.Args[k].(type) {
	case []any:
		for _, x := range v {
			if s := strings.TrimSpace(fmt.Sprint(x)); s != "" {
				out = append(out, s)
			}
		}
	case []string:
		out = v
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

var nodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// node is the node's base path. Models may pass a node's name instead of its id; the
// dashboard's list resolves it.
func (c Call) node() (string, error) {
	n := c.str("node")
	if n == "" || n == "this" || n == "main" {
		n = "local"
	}
	if n != "local" {
		var list []struct {
			ID, Name string
			Local    bool
		}
		if raw, err := c.Do(c.User, "GET", "/api/nodes", nil); err == nil && json.Unmarshal(raw, &list) == nil {
			for _, x := range list {
				if strings.EqualFold(x.Name, n) || x.ID == n {
					n = x.ID
					break
				}
			}
		}
	}
	if !nodeRe.MatchString(n) {
		return "", errors.New("unknown node " + n + "; list_nodes shows them")
	}
	return "/api/n/" + n, nil
}

func (c Call) get(path string) (json.RawMessage, error) { return c.Do(c.User, "GET", path, nil) }
func (c Call) post(path string, body any) (json.RawMessage, error) {
	if body == nil {
		body = map[string]any{}
	}
	return c.Do(c.User, "POST", path, body)
}

func (c Call) nodeGet(p string) (json.RawMessage, error) {
	base, err := c.node()
	if err != nil {
		return nil, err
	}
	return c.get(base + p)
}

func (c Call) nodePost(p string, body any) (json.RawMessage, error) {
	base, err := c.node()
	if err != nil {
		return nil, err
	}
	return c.post(base+p, body)
}

func seg(s string) string { return url.PathEscape(s) }

// decode keeps only the fields a model needs out of a reply.
func decode[T any](raw json.RawMessage, err error) (T, error) {
	var v T
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(raw, &v)
}

func output(raw json.RawMessage, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	var r struct {
		Output string `json:"output"`
		Job    any    `json:"job"`
	}
	json.Unmarshal(raw, &r)
	if r.Job != nil {
		return map[string]any{"started": true, "job": r.Job}, nil
	}
	if r.Output == "" {
		r.Output = "Done."
	}
	return map[string]any{"ok": true, "output": r.Output}, nil
}

func ago(unix int64) string {
	if unix == 0 {
		return "never"
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// ---- Parameters ----------------------------------------------------------------------

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func enum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}
func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

var nodeParam = str(`Node id or name from list_nodes. "local" (the default) is the main node.`)

func with(extra map[string]any) map[string]any {
	m := map[string]any{"node": nodeParam}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

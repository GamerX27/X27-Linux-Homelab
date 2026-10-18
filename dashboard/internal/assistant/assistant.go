// Package assistant is the dashboard's built-in AI: a chat that talks to a local model
// server (Ollama, LM Studio, or anything else with the OpenAI chat API) and uses the same
// tools as the MCP server. Read tools run on their own; anything that changes something
// stops and waits for the person to approve it in the browser. The conversation lives in
// the browser; the server keeps only the settings.
package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tools"
)

const (
	MaxRounds     = 12   // model calls per turn
	MaxToolOutput = 8000 // characters of a tool result the model sees
)

// Providers are presets for the settings form; any OpenAI-compatible server works.
var Providers = map[string]string{
	"ollama":   "http://127.0.0.1:11434/v1",
	"lmstudio": "http://127.0.0.1:1234/v1",
	"custom":   "",
}

type Config struct {
	Enabled  bool        `json:"enabled"`
	Provider string      `json:"provider"`
	BaseURL  string      `json:"baseUrl"`
	Model    string      `json:"model"`
	APIKey   string      `json:"apiKey,omitempty"`
	MaxScope tools.Scope `json:"maxScope"`
}

// Public is Config without the key.
type Public struct {
	Config
	HasKey bool `json:"hasKey"`
}

type Settings struct {
	path string
	mu   sync.Mutex
	cfg  Config
}

func OpenSettings(dir string) *Settings {
	s := &Settings{path: filepath.Join(dir, "assistant.json"), cfg: Config{Provider: "ollama", BaseURL: Providers["ollama"], MaxScope: tools.Operate}}
	if b, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(b, &s.cfg)
	}
	return s
}

func (s *Settings) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Settings) Public() Public {
	c := s.Get()
	p := Public{Config: c, HasKey: c.APIKey != ""}
	p.APIKey = ""
	return p
}

var baseRe = regexp.MustCompile(`^https?://[^\s/]+(/\S*)?$`)

// Save stores new settings. An empty key keeps the stored one; KeepKey=false with an
// empty key clears it.
func (s *Settings) Save(c Config, clearKey bool) error {
	c.BaseURL = strings.TrimSuffix(strings.TrimSpace(c.BaseURL), "/")
	c.Model = strings.TrimSpace(c.Model)
	if !baseRe.MatchString(c.BaseURL) {
		return errors.New("the server URL must start with http:// or https://, e.g. http://192.168.1.20:11434/v1")
	}
	if _, ok := Providers[c.Provider]; !ok {
		c.Provider = "custom"
	}
	if c.Enabled && c.Model == "" {
		return errors.New("pick a model")
	}
	if c.MaxScope < tools.Read || c.MaxScope > tools.Admin {
		c.MaxScope = tools.Operate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.APIKey == "" && !clearKey {
		c.APIKey = s.cfg.APIKey
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.cfg = c
	return nil
}

// ---- OpenAI-compatible chat API --------------------------------------------------------

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// Args decodes the arguments, which servers send as a JSON string (the spec) or an object.
func (tc ToolCall) Args() (map[string]any, error) {
	raw := bytes.TrimSpace(tc.Function.Arguments)
	if len(raw) == 0 || string(raw) == `""` || string(raw) == "null" {
		return map[string]any{}, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		if strings.TrimSpace(s) == "" {
			return map[string]any{}, nil
		}
		raw = []byte(s)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("the model sent arguments that aren't JSON: %s", raw)
	}
	return m, nil
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type Client struct {
	BaseURL, APIKey string
	HTTP            *http.Client
}

func NewClient(c Config) *Client {
	return &Client{BaseURL: strings.TrimSuffix(c.BaseURL, "/"), APIKey: c.APIKey, HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach the model server at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error any `json:"error"`
		}
		json.Unmarshal(b, &e)
		msg := strings.TrimSpace(string(b))
		switch v := e.Error.(type) {
		case string:
			msg = v
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				msg = m
			}
		}
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return fmt.Errorf("model server: %s: %s", resp.Status, msg)
	}
	return json.Unmarshal(b, out)
}

// Models lists the model ids the server offers.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/models", nil, &r); err != nil {
		return nil, err
	}
	out := []string{}
	for _, m := range r.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// NoTools asks the model for a text answer only.
const NoTools tools.Scope = -1

func toolDefs(scope tools.Scope) []map[string]any {
	var out []map[string]any
	if scope == NoTools {
		return nil
	}
	for _, t := range tools.For(scope) {
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.Schema()}})
	}
	return out
}

func (c *Client) Chat(ctx context.Context, model string, msgs []Message, scope tools.Scope) (Message, error) {
	var r struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	body := map[string]any{"model": model, "messages": msgs, "stream": false, "temperature": 0.2}
	if defs := toolDefs(scope); len(defs) > 0 {
		body["tools"] = defs
	}
	if err := c.do(ctx, "POST", "/chat/completions", body, &r); err != nil {
		return Message{}, err
	}
	if len(r.Choices) == 0 {
		return Message{}, errors.New("the model sent no answer")
	}
	m := r.Choices[0].Message
	m.Role = "assistant"
	m.Content = StripThinking(m.Content)
	for i := range m.ToolCalls {
		if m.ToolCalls[i].ID == "" {
			m.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
		}
		m.ToolCalls[i].Type = "function"
	}
	return m, nil
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

// StripThinking drops the <think>…</think> block reasoning models put before the answer.
func StripThinking(s string) string { return strings.TrimSpace(thinkRe.ReplaceAllString(s, "")) }

// ---- The loop -------------------------------------------------------------------------

// Event is one line of the NDJSON stream to the browser.
type Event struct {
	Type    string   `json:"type"` // message, tool, pending, error, done
	Message *Message `json:"message,omitempty"`
	Call    *Pending `json:"call,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type Pending struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Title string         `json:"title"`
	Args  map[string]any `json:"args"`
	Scope tools.Scope    `json:"scope"`
}

type Turn struct {
	Messages  []Message       `json:"messages"`
	Approvals map[string]bool `json:"approvals"` // tool call id → run it (true) or not
	Context   string          `json:"context"`   // what the person is looking at
}

type Runner struct {
	Config   Config
	Client   interface {
		Chat(ctx context.Context, model string, msgs []Message, scope tools.Scope) (Message, error)
	}
	User     string
	Dispatch tools.Dispatch
	Emit     func(Event)
	Log      func(format string, a ...any)

	textOnly bool // the next model call gets no tools
}

func SystemPrompt(user, context string) string {
	p := `You are the assistant built into the X27-Linux Homelab dashboard. You help ` + user + ` run their homelab servers ("nodes").

About these servers: each runs X27-Linux Homelab, a read-only, image-based (bootc/rpm-ostree) Linux made for Docker. The OS updates as a whole image and can be rolled back; there is no dnf. Everything else runs in Docker, mostly as compose apps ("stacks") in ~/docker/<name>/. One main node serves this dashboard and manages the others.

How to work:
- Use the tools to look things up; never guess names, numbers or states. list_nodes first if you don't know the nodes.
- Tools that change something (start, stop, update, reboot…) need the user's approval, which the dashboard asks for. Say what you're about to do and why before calling such a tool. Never call one the user didn't ask for, and don't repeat a call the user declined.
- Be brief and concrete. Use short lists. Quote the exact error lines from logs when they explain a problem.
- If a tool fails, say so and what it returned; don't pretend it worked.
Today is ` + time.Now().Format("Monday 2 January 2006, 15:04 MST") + "."
	if context != "" {
		p += "\nThe user is looking at: " + context
	}
	return p
}

func truncate(s string) string {
	if len(s) <= MaxToolOutput {
		return s
	}
	return s[:MaxToolOutput/2] + "\n…(" + fmt.Sprint(len(s)-MaxToolOutput) + " characters left out)…\n" + s[len(s)-MaxToolOutput/2:]
}

func resultText(v any, err error) string {
	if err != nil {
		return "ERROR: " + err.Error()
	}
	if s, ok := v.(string); ok {
		return truncate(s)
	}
	b, _ := json.Marshal(v)
	return truncate(string(b))
}

// answered lists the tool calls that already have a result.
func answered(msgs []Message) map[string]bool {
	m := map[string]bool{}
	for _, x := range msgs {
		if x.Role == "tool" {
			m[x.ToolCallID] = true
		}
	}
	return m
}

const declinedText = "The user declined this. Don't try it again; ask what they want instead."

func signature(tc ToolCall) string {
	args, _ := tc.Args()
	b, _ := json.Marshal(args) // map keys come out sorted
	return tc.Function.Name + string(b)
}

// declined lists the calls the user said no to, so the same call isn't asked for again.
func declined(msgs []Message) map[string]bool {
	byID := map[string]ToolCall{}
	out := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			byID[tc.ID] = tc
		}
		if m.Role == "tool" && m.Content == declinedText {
			out[signature(byID[m.ToolCallID])] = true
		}
	}
	return out
}

func laterResult(msgs []Message, id string) bool {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return true
		}
	}
	return false
}

// runCalls handles the tool calls of the last assistant message that have no result yet.
// It returns false when one waits for approval.
func (r *Runner) runCalls(ctx context.Context, msgs *[]Message, approvals map[string]bool) bool {
	last := (*msgs)[len(*msgs)-1]
	done := answered(*msgs)
	for _, tc := range last.ToolCalls {
		if done[tc.ID] {
			continue
		}
		add := func(text string) {
			m := Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: text}
			*msgs = append(*msgs, m)
			r.Emit(Event{Type: "tool", Message: &m})
		}
		t, ok := tools.Find(tc.Function.Name)
		if !ok {
			add("ERROR: there is no tool called " + tc.Function.Name)
			continue
		}
		args, err := tc.Args()
		if err != nil {
			add("ERROR: " + err.Error())
			continue
		}
		if t.Scope > r.Config.MaxScope {
			add(fmt.Sprintf("ERROR: %s needs %s access; the assistant is limited to %s. Tell the user to do it in the dashboard instead.", t.Name, t.Scope, r.Config.MaxScope))
			continue
		}
		if t.Scope > tools.Read {
			allow, decided := approvals[tc.ID]
			if !decided && declined(*msgs)[signature(tc)] {
				add("The user already declined exactly this. It was not run and won't be offered again. Answer in text and ask what they want instead.")
				r.textOnly = true
				continue
			}
			if !decided {
				r.Emit(Event{Type: "pending", Call: &Pending{ID: tc.ID, Name: t.Name, Title: t.Description, Args: args, Scope: t.Scope}})
				return false
			}
			if !allow {
				add(declinedText)
				continue
			}
		}
		if r.Log != nil && t.Scope > tools.Read {
			r.Log("assistant ran %s (approved) on node %v", t.Name, args["node"])
		}
		v, err := t.Exec(tools.Call{User: r.User, Do: r.Dispatch, Args: args})
		add(resultText(v, err))
	}
	return true
}

// Run answers one turn: it calls the model and the tools until the model answers in text,
// a tool waits for approval, or MaxRounds is reached. Everything new is sent with Emit.
func (r *Runner) Run(ctx context.Context, t Turn) {
	defer r.Emit(Event{Type: "done"})
	if len(t.Messages) == 0 {
		r.Emit(Event{Type: "error", Error: "nothing to answer"})
		return
	}
	msgs := []Message{{Role: "system", Content: SystemPrompt(r.User, t.Context)}}
	done := answered(t.Messages)
	for i, m := range t.Messages {
		if m.Role == "system" {
			continue // the browser can't set the system prompt
		}
		msgs = append(msgs, m)
		// A call that was left waiting when the user wrote something else: model servers
		// refuse a conversation where a tool call has no result.
		if m.Role == "assistant" && i < len(t.Messages)-1 {
			for _, tc := range m.ToolCalls {
				if !done[tc.ID] && !laterResult(t.Messages[i+1:], tc.ID) {
					msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: "Not run: the user moved on."})
				}
			}
		}
	}
	for round := 0; ; round++ {
		if last := msgs[len(msgs)-1]; last.Role == "assistant" && len(last.ToolCalls) > 0 {
			if !r.runCalls(ctx, &msgs, t.Approvals) {
				return
			}
			t.Approvals = nil // approvals are for the calls they were given for
		} else if last.Role == "assistant" {
			return
		}
		if round >= MaxRounds {
			r.Emit(Event{Type: "error", Error: fmt.Sprintf("Stopped after %d steps. Ask again to continue.", MaxRounds)})
			return
		}
		scope := r.Config.MaxScope
		if r.textOnly {
			scope, r.textOnly = NoTools, false
		}
		m, err := r.Client.Chat(ctx, r.Config.Model, msgs, scope)
		if err != nil {
			r.Emit(Event{Type: "error", Error: err.Error()})
			return
		}
		msgs = append(msgs, m)
		r.Emit(Event{Type: "message", Message: &m})
	}
}

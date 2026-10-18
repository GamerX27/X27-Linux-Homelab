package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tools"
)

// script answers each Chat call with the next message.
type script struct {
	replies []Message
	calls   int
	seen    [][]Message
	scopes  []tools.Scope
}

func (s *script) Chat(_ context.Context, _ string, msgs []Message, scope tools.Scope) (Message, error) {
	s.seen = append(s.seen, msgs)
	s.scopes = append(s.scopes, scope)
	m := s.replies[min(s.calls, len(s.replies)-1)]
	s.calls++
	return m, nil
}

func call(id, name, args string) Message {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = json.RawMessage(args)
	return Message{Role: "assistant", ToolCalls: []ToolCall{tc}}
}

func runner(s *script, scope tools.Scope, dispatched *[]string) (*Runner, *[]Event) {
	var events []Event
	return &Runner{Config: Config{Model: "m", MaxScope: scope}, Client: s, User: "sindre",
		Dispatch: func(user, method, path string, body any) (json.RawMessage, error) {
			*dispatched = append(*dispatched, method+" "+path)
			if path == "/api/nodes" {
				return json.RawMessage(`[{"id":"local","name":"tower","local":true,"online":true}]`), nil
			}
			return json.RawMessage(`{"output":"ok"}`), nil
		},
		Emit: func(e Event) { events = append(events, e) }}, &events
}

func TestReadToolRunsAndAnswers(t *testing.T) {
	var d []string
	s := &script{replies: []Message{call("1", "list_nodes", `"{}"`), {Role: "assistant", Content: "<think>hm</think>One node: tower."}}}
	r, ev := runner(s, tools.Operate, &d)
	r.Run(context.Background(), Turn{Messages: []Message{{Role: "user", Content: "what nodes?"}}})
	if len(d) != 1 || d[0] != "GET /api/nodes" {
		t.Fatal(d)
	}
	var types []string
	for _, e := range *ev {
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "message,tool,message,done" {
		t.Fatal(types)
	}
	if s.seen[1][len(s.seen[1])-1].Role != "tool" {
		t.Fatal("tool result not passed back")
	}
}

func TestWriteToolWaitsForApproval(t *testing.T) {
	var d []string
	ask := call("c1", "container_action", `{"container":"jellyfin","action":"restart"}`)
	s := &script{replies: []Message{ask, {Role: "assistant", Content: "Restarted."}}}
	r, ev := runner(s, tools.Operate, &d)
	msgs := []Message{{Role: "user", Content: "restart jellyfin"}}
	r.Run(context.Background(), Turn{Messages: msgs})
	if len(d) != 0 {
		t.Fatal("ran without approval", d)
	}
	var pending *Pending
	for _, e := range *ev {
		if e.Type == "pending" {
			pending = e.Call
		}
	}
	if pending == nil || pending.Name != "container_action" {
		t.Fatal(*ev)
	}
	// The browser sends the conversation back with the approval.
	r2, _ := runner(s, tools.Operate, &d)
	r2.Run(context.Background(), Turn{Messages: append(msgs, ask), Approvals: map[string]bool{"c1": true}})
	if len(d) != 1 || d[0] != "POST /api/n/local/docker/containers/jellyfin/restart" {
		t.Fatal(d)
	}
}

func TestScopeLimitAndDecline(t *testing.T) {
	var d []string
	s := &script{replies: []Message{call("r", "reboot_node", `{"node":"local"}`), {Role: "assistant", Content: "Can't."}}}
	r, _ := runner(s, tools.Operate, &d)
	r.Run(context.Background(), Turn{Messages: []Message{{Role: "user", Content: "reboot"}}})
	if len(d) != 0 || !strings.Contains(s.seen[1][len(s.seen[1])-1].Content, "needs admin") {
		t.Fatal(d, s.seen)
	}
	// A declined call isn't run, and the model hears about it.
	ask := call("c2", "stack_action", `{"stack":"web","action":"stop"}`)
	s2 := &script{replies: []Message{{Role: "assistant", Content: "OK."}}}
	r2, _ := runner(s2, tools.Admin, &d)
	r2.Run(context.Background(), Turn{Messages: []Message{{Role: "user", Content: "stop web"}, ask}, Approvals: map[string]bool{"c2": false}})
	if len(d) != 0 || !strings.Contains(s2.seen[0][len(s2.seen[0])-1].Content, "declined") {
		t.Fatal(d)
	}
}

func TestDanglingCallGetsAResult(t *testing.T) {
	var d []string
	s := &script{replies: []Message{{Role: "assistant", Content: "Sure."}}}
	r, _ := runner(s, tools.Admin, &d)
	r.Run(context.Background(), Turn{Messages: []Message{{Role: "user", Content: "a"},
		call("x", "reboot_node", `{}`), {Role: "user", Content: "never mind"}}})
	got := s.seen[0]
	if got[2].Role != "assistant" || got[3].Role != "tool" || got[3].ToolCallID != "x" || got[4].Role != "user" {
		t.Fatalf("%+v", got)
	}
}

func TestRoundLimit(t *testing.T) {
	var d []string
	s := &script{replies: []Message{call("loop", "list_nodes", `{}`)}}
	r, ev := runner(s, tools.Read, &d)
	r.Run(context.Background(), Turn{Messages: []Message{{Role: "user", Content: "go"}}})
	if s.calls != MaxRounds {
		t.Fatal(s.calls)
	}
	if e := (*ev)[len(*ev)-2]; e.Type != "error" {
		t.Fatal(e)
	}
}

func TestClientAgainstFakeServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"qwen3:8b"},{"id":"llama3.1:8b"}]}`))
		case "/v1/chat/completions":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if len(body["tools"].([]any)) == 0 {
				t.Error("no tools sent")
			}
			// Ollama-style: arguments as an object, no id.
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"list_nodes","arguments":{}}}]}}]}`))
		}
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL + "/v1", APIKey: "k"})
	ms, err := c.Models(context.Background())
	if err != nil || len(ms) != 2 {
		t.Fatal(ms, err)
	}
	m, err := c.Chat(context.Background(), "qwen3:8b", []Message{{Role: "user", Content: "hi"}}, tools.Read)
	if err != nil || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID == "" {
		t.Fatal(m, err)
	}
	if a, err := m.ToolCalls[0].Args(); err != nil || len(a) != 0 {
		t.Fatal(a, err)
	}
	bad := NewClient(Config{BaseURL: srv.URL + "/v1", APIKey: "nope"})
	if _, err := bad.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatal(err)
	}
}

func TestDeclinedCallIsNotOfferedAgain(t *testing.T) {
	var d []string
	ask := call("c1", "container_action", `{"container":"web","action":"start"}`)
	again := call("c2", "container_action", `{"action":"start","container":"web"}`)
	s := &script{replies: []Message{again, {Role: "assistant", Content: "OK, I won't."}}}
	r, ev := runner(s, tools.Operate, &d)
	r.Run(context.Background(), Turn{Messages: []Message{{Role: "user", Content: "start web"}, ask}, Approvals: map[string]bool{"c1": false}})
	for _, e := range *ev {
		if e.Type == "pending" {
			t.Fatal("asked again for a declined call")
		}
	}
	if len(d) != 0 || s.calls != 2 {
		t.Fatal(d, s.calls)
	}
	if got := s.scopes[1]; got != NoTools {
		t.Fatalf("the answer after a repeat should have no tools, got %v", got)
	}
}

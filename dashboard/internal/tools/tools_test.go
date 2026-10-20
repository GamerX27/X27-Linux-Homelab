package tools

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestRegistry(t *testing.T) {
	seen := map[string]bool{}
	nameRe := regexp.MustCompile(`^[a-z_]{3,40}$`)
	for _, tl := range All {
		if seen[tl.Name] || !nameRe.MatchString(tl.Name) || tl.Description == "" || tl.Run == nil {
			t.Errorf("bad tool %q", tl.Name)
		}
		seen[tl.Name] = true
		for _, r := range tl.Required {
			if _, ok := tl.Params[r]; !ok {
				t.Errorf("%s requires unknown %s", tl.Name, r)
			}
		}
		if _, err := json.Marshal(tl.Schema()); err != nil {
			t.Error(err)
		}
	}
	if len(For(Read)) >= len(For(Operate)) || len(For(Operate)) >= len(For(Admin)) || len(For(Admin)) != len(All) {
		t.Error("scopes should nest")
	}
	for _, tl := range For(Read) {
		if tl.Scope != Read {
			t.Error(tl.Name)
		}
	}
}

// fake records the requests a tool makes.
func fake(reqs *[]string, reply string) Dispatch {
	return func(user, method, path string, body any) (json.RawMessage, error) {
		b, _ := json.Marshal(body)
		*reqs = append(*reqs, method+" "+path+" "+string(b))
		if path == "/api/nodes" {
			return json.RawMessage(`[{"id":"local","name":"tower","local":true},{"id":"ab12","name":"NAS"}]`), nil
		}
		return json.RawMessage(reply), nil
	}
}

func TestExecValidatesAndResolvesNodes(t *testing.T) {
	var reqs []string
	tl, _ := Find("container_action")
	if _, err := tl.Exec(Call{Do: fake(&reqs, `{}`), Args: map[string]any{"container": "x", "action": "remove"}}); err == nil {
		t.Error("action outside the enum accepted")
	}
	if _, err := tl.Exec(Call{Do: fake(&reqs, `{}`), Args: map[string]any{"container": "x", "action": "stop", "rm": true}}); err == nil {
		t.Error("unknown argument accepted")
	}
	if _, err := tl.Exec(Call{Do: fake(&reqs, `{}`), Args: map[string]any{"action": "stop"}}); err == nil {
		t.Error("missing container accepted")
	}
	reqs = nil
	if _, err := tl.Exec(Call{User: "u", Do: fake(&reqs, `{"output":""}`), Args: map[string]any{"node": "nas", "container": "web", "action": "stop"}}); err != nil {
		t.Fatal(err)
	}
	if reqs[len(reqs)-1] != "POST /api/n/ab12/docker/containers/web/stop {}" {
		t.Fatal(reqs)
	}
	if _, err := (Call{Do: fake(&reqs, `{}`), Args: map[string]any{"node": "../../etc"}}).node(); err == nil {
		t.Error("path in node accepted")
	}
}

func TestGetStackHidesEnvValues(t *testing.T) {
	var reqs []string
	tl, _ := Find("get_stack")
	v, err := tl.Exec(Call{Do: fake(&reqs, `{"compose":"services: {}","env":"DB_PASSWORD=hunter2\nexport TZ=Europe/Oslo\n# c=1"}`), Args: map[string]any{"stack": "app"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), "DB_PASSWORD") || !strings.Contains(string(b), "TZ") {
		t.Fatal(string(b))
	}
}

func TestSaveStackKeepsEnv(t *testing.T) {
	var reqs []string
	tl, _ := Find("save_stack")
	_, err := tl.Exec(Call{Do: fake(&reqs, `{"compose":"old","env":"SECRET=1\n","output":"saved"}`), Args: map[string]any{"stack": "app", "compose": "new"}})
	if err != nil {
		t.Fatal(err)
	}
	last := reqs[len(reqs)-1]
	if !strings.HasPrefix(last, "POST /api/n/local/docker/stacks/app ") || !strings.Contains(last, `"env":"SECRET=1\n"`) {
		t.Fatal(last)
	}
}

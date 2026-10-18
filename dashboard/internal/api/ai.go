package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/apitokens"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/assistant"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/auth"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tools"
)

// dispatcher runs tool requests through the main node's own API, as the given user, so a
// tool goes through exactly what a click in the UI does (validation, node proxying, logs).
func dispatcher(api http.Handler) tools.Dispatch {
	return func(user, method, path string, body any) (json.RawMessage, error) {
		var rd io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			rd = bytes.NewReader(b)
		}
		req := httptest.NewRequestWithContext(context.Background(), method, path, rd)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, withUser(req, user))
		if rec.Code >= 300 {
			var e struct{ Error string }
			json.Unmarshal(rec.Body.Bytes(), &e)
			if e.Error == "" {
				e.Error = http.StatusText(rec.Code)
			}
			return nil, errors.New(e.Error)
		}
		return rec.Body.Bytes(), nil
	}
}

// toolText is a tool's result as the text a model reads.
func toolText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.MarshalIndent(v, "", " ")
	return string(b)
}

// logArgs names a call's arguments for the log, without long values (compose files).
func logArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k, v := range args {
		s := fmt.Sprint(v)
		if len(s) > 40 {
			s = s[:40] + "…"
		}
		keys = append(keys, k+"="+s)
	}
	sort.Strings(keys)
	return strings.Join(keys, " ")
}

const mcpInstructions = `Tools for an X27-Linux Homelab: image-based Linux servers ("nodes") that run Docker apps (compose stacks in ~/docker), managed from one main node. Call list_nodes first; other tools take a "node" (id or name, default the main node). Changes go through the same checks as the dashboard and are logged with the token's name.`

func newMCPServer(version string, scope tools.Scope, do tools.Dispatch) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "x27-homelab", Title: "X27-Linux Homelab", Version: version},
		&mcp.ServerOptions{Instructions: mcpInstructions})
	for _, t := range tools.For(scope) {
		destructive := t.Scope == tools.Admin
		s.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.Schema(),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: t.Scope == tools.Read, DestructiveHint: &destructive}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				ti := req.Extra.TokenInfo
				if ti == nil {
					return nil, errors.New("no token")
				}
				args := map[string]any{}
				if len(req.Params.Arguments) > 0 {
					if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
						return nil, err
					}
				}
				who := fmt.Sprintf("mcp:%v/%s", ti.Extra["name"], ti.UserID)
				v, err := t.Exec(tools.Call{User: ti.UserID, Do: do, Args: args})
				res := &mcp.CallToolResult{}
				if err != nil {
					log.Printf("%s: %s %s failed: %v", who, t.Name, logArgs(args), err)
					res.SetError(err)
					return res, nil
				}
				if t.Scope > tools.Read {
					log.Printf("%s: %s %s", who, t.Name, logArgs(args))
				}
				res.Content = []mcp.Content{&mcp.TextContent{Text: toolText(v)}}
				return res, nil
			})
	}
	for _, p := range tools.Prompts {
		mp := &mcp.Prompt{Name: p.Name, Title: p.Title, Description: p.Description}
		for _, a := range p.Args {
			mp.Arguments = append(mp.Arguments, &mcp.PromptArgument{Name: a, Required: true})
		}
		s.AddPrompt(mp, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Description: p.Description, Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: p.Text(req.Params.Arguments)}}}}, nil
		})
	}
	return s
}

// mcpHandler serves MCP over streamable HTTP at /mcp, for API tokens only. A token sees the
// tools of its scope and acts as the user who made it, who must still be in wheel.
func mcpHandler(version string, dev bool, tokens *apitokens.Store, do tools.Dispatch) http.Handler {
	servers := map[tools.Scope]*mcp.Server{}
	for _, s := range []tools.Scope{tools.Read, tools.Operate, tools.Admin} {
		servers[s] = newMCPServer(version, s, do)
	}
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		ti := mcpauth.TokenInfoFromContext(r.Context())
		if ti == nil {
			return nil
		}
		sc, _ := tools.ParseScope(ti.Scopes[0])
		return servers[sc]
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	verify := func(ctx context.Context, token string, r *http.Request) (*mcpauth.TokenInfo, error) {
		t, err := tokens.Verify(token)
		if err != nil {
			log.Printf("mcp: refused a token from %s: %v", r.RemoteAddr, err)
			return nil, fmt.Errorf("%w: %v", mcpauth.ErrInvalidToken, err)
		}
		if !dev && !auth.InWheel(t.Owner) {
			return nil, fmt.Errorf("%w: %s isn't in wheel anymore", mcpauth.ErrInvalidToken, t.Owner)
		}
		exp := time.Now().Add(time.Hour)
		if t.ExpiresAt != 0 {
			exp = time.Unix(t.ExpiresAt, 0)
		}
		return &mcpauth.TokenInfo{Scopes: []string{t.Scope.String()}, UserID: t.Owner, Expiration: exp,
			Extra: map[string]any{"name": t.Name}}, nil
	}
	return mcpauth.RequireBearerToken(verify, nil)(h)
}

func certPEM(c tls.Certificate) []byte {
	if len(c.Certificate) == 0 {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]})
}

// aiRoutes: the AI page's API — tokens for MCP clients, and the built-in assistant.
func aiRoutes(mux *http.ServeMux, api *http.ServeMux, o Options) (*apitokens.Store, tools.Dispatch, error) {
	tokens, err := apitokens.Open(config.StateDir)
	if err != nil {
		return nil, nil, err
	}
	settings := assistant.OpenSettings(config.StateDir)
	do := dispatcher(api)

	mux.Handle("/mcp", mcpHandler(o.Version, o.Dev, tokens, do))
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="homelab-dashboard.pem"`)
		w.Write(certPEM(o.Cert))
	})

	api.HandleFunc("GET /api/tokens", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, tokens.List())
	})
	api.HandleFunc("POST /api/tokens", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name  string
			Scope string
			Days  int
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		scope, err := tools.ParseScope(body.Scope)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		t, secret, err := tokens.Create(userOf(r), body.Name, scope, body.Days)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		log.Printf("%s: created %s API token %q", userOf(r), scope, t.Name)
		writeJSON(w, http.StatusOK, map[string]any{"token": t, "secret": secret})
	})
	api.HandleFunc("DELETE /api/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "revoke API token "+r.PathValue("id"), "", tokens.Delete(r.PathValue("id")))
	})
	api.HandleFunc("GET /api/tools", func(w http.ResponseWriter, r *http.Request) {
		type entry struct {
			Name        string      `json:"name"`
			Description string      `json:"description"`
			Scope       tools.Scope `json:"scope"`
		}
		var out []entry
		for _, t := range tools.All {
			out = append(out, entry{t.Name, t.Description, t.Scope})
		}
		var prompts []map[string]any
		for _, p := range tools.Prompts {
			prompts = append(prompts, map[string]any{"name": p.Name, "title": p.Title, "description": p.Description, "args": p.Args,
				"text": p.Text(map[string]string{"node": "<node>", "stack": "<app>"})})
		}
		writeJSON(w, http.StatusOK, map[string]any{"tools": out, "prompts": prompts})
	})

	// The assistant
	api.HandleFunc("GET /api/assistant", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"settings": settings.Public(), "providers": assistant.Providers})
	})
	api.HandleFunc("POST /api/assistant/settings", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			assistant.Config
			ClearKey bool `json:"clearKey"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		err := settings.Save(body.Config, body.ClearKey)
		result(w, r, fmt.Sprintf("assistant settings: %s %s, up to %s", body.BaseURL, body.Model, body.MaxScope), "", err)
	})
	api.HandleFunc("POST /api/assistant/models", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ BaseURL, APIKey string }
		readJSON(r, &body)
		c := settings.Get()
		if body.BaseURL != "" {
			c.BaseURL = strings.TrimSuffix(strings.TrimSpace(body.BaseURL), "/")
		}
		if body.APIKey != "" {
			c.APIKey = body.APIKey
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		ms, err := assistant.NewClient(c).Models(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": ms})
	})
	var chatMu sync.Mutex
	chats := map[string]int{} // running turns per user, so one person can't pile them up
	api.HandleFunc("POST /api/assistant/chat", func(w http.ResponseWriter, r *http.Request) {
		cfg := settings.Get()
		if !cfg.Enabled || cfg.Model == "" {
			writeErr(w, http.StatusBadRequest, errors.New("the assistant isn't set up: pick a model server and model on the AI page"))
			return
		}
		var turn assistant.Turn
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&turn); err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("bad request body"))
			return
		}
		user := userOf(r)
		chatMu.Lock()
		if chats[user] >= 2 {
			chatMu.Unlock()
			writeErr(w, http.StatusTooManyRequests, errors.New("wait for your other questions to finish"))
			return
		}
		chats[user]++
		chatMu.Unlock()
		defer func() { chatMu.Lock(); chats[user]--; chatMu.Unlock() }()

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		var wmu sync.Mutex
		run := assistant.Runner{Config: cfg, Client: assistant.NewClient(cfg), User: user, Dispatch: do,
			Emit: func(e assistant.Event) {
				wmu.Lock()
				defer wmu.Unlock()
				enc.Encode(e)
				if flusher != nil {
					flusher.Flush()
				}
			},
			Log: func(format string, a ...any) { log.Printf(user+": "+format, a...) }}
		run.Run(r.Context(), turn)
	})
	return tokens, do, nil
}

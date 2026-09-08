// Space actions — let the TV agent DO things, not just render.
//
// Every installed space ships a declarative `actions` catalog in its manifest
// (id → {description, params}); spaces.go already parses it alongside `tv`. Here
// we turn that catalog into OpenAI function-calling tools, run a bounded
// tool-calling loop, and execute each chosen action HEADLESS via the
// space-runtime (POST /run with the user's delegated token). This is the same
// capability layer the desktop brain and cloud operator use — the TV is just
// another surface calling it directly.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

var errNoActions = &simpleErr{"no space actions available"}

// spaceRuntimeURL is the headless action executor. Default targets the
// construct-space CapRover internal service; override with SPACE_RUNTIME_URL.
func spaceRuntimeURL() string {
	return strings.TrimRight(env("SPACE_RUNTIME_URL", "http://srv-captain--space-runtime:60190"), "/")
}

// runSpaceAction executes one space action headless via the space-runtime. The
// runtime fetches + verifies the published bundle (name+version) and runs its
// actions.ts against Graph as the user (token). Returns the result as JSON text.
func runSpaceAction(ctx context.Context, token, space, version, action string, params map[string]any) (string, error) {
	if params == nil {
		params = map[string]any{}
	}
	payload := map[string]any{
		"spaceId": space, "action": action, "params": params, "token": token,
		"name": space, // marketplace name == space id
	}
	if version != "" {
		payload["version"] = version
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", spaceRuntimeURL()+"/run", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var res struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", &simpleErr{fmt.Sprintf("space-runtime: bad response (http %d)", resp.StatusCode)}
	}
	if !res.OK {
		if res.Error == "" {
			res.Error = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return "", &simpleErr{"action failed: " + res.Error}
	}
	if len(res.Result) == 0 {
		return "{\"ok\":true}", nil
	}
	return string(res.Result), nil
}

const maxActionTools = 120

// actionTools renders the action catalog as OpenAI function tools. Spaces named
// in the query are ordered first so the most relevant tools survive the cap.
func actionTools(idx []tvSpace, query string) []map[string]any {
	ql := strings.ToLower(query)
	var hit, rest []tvSpace
	for _, s := range idx {
		if len(s.Actions) == 0 {
			continue
		}
		if strings.Contains(ql, strings.ToLower(s.ID)) || (s.Name != "" && strings.Contains(ql, strings.ToLower(s.Name))) {
			hit = append(hit, s)
		} else {
			rest = append(rest, s)
		}
	}
	// When the query names a space, expose only those spaces' tools — a tight,
	// reliable toolset beats dumping every space's actions at the model. Fall
	// back to the full (capped) catalog when nothing is named.
	chosen := hit
	if len(chosen) == 0 {
		chosen = rest
	}
	out := make([]map[string]any, 0, 32)
	for _, s := range chosen {
		for id, def := range s.Actions {
			out = append(out, oneActionTool(s, id, def))
			if len(out) >= maxActionTools {
				return out
			}
		}
	}
	return out
}

func oneActionTool(s tvSpace, id string, def actionDef) map[string]any {
	props := map[string]any{}
	required := []string{}
	for pn, p := range def.Params {
		prop := map[string]any{"type": jsonType(p.Type)}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		props[pn] = prop
		if p.Required {
			required = append(required, pn)
		}
	}
	params := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		params["required"] = required
	}
	desc := def.Description
	if s.Name != "" {
		desc = "[" + s.Name + "] " + desc
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": s.ID + "__" + id, "description": desc, "parameters": params,
		},
	}
}

func jsonType(t string) string {
	switch t {
	case "number", "integer", "boolean":
		return t
	default:
		return "string"
	}
}

func splitToolName(name string) (space, action string) {
	if i := strings.Index(name, "__"); i >= 0 {
		return name[:i], name[i+2:]
	}
	return "", name
}

func spaceVersion(idx []tvSpace, id string) string {
	for _, s := range idx {
		if s.ID == id {
			return s.Version
		}
	}
	return ""
}

// pickToolModel ensures the action loop runs on a model that supports native
// function-calling. The Construct-managed "source-medium" path doesn't reliably
// tool-call (it describes the call as text), so when the user's selection isn't
// an org "provider:model" id we pick a function-calling org model (deepseek >
// minimax > kimi). Falls back to the requested model if none are available.
func pickToolModel(ctx context.Context, userToken, requested string) string {
	if strings.Contains(requested, ":") {
		return requested
	}
	models := listOrgModels(ctx, userToken)
	for _, pref := range []string{"deepseek", "minimax", "kimi"} {
		for _, m := range models {
			if strings.HasPrefix(m, pref+":") {
				return m
			}
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return requested
}

const actionSystem = `You are the Construct TV assistant. The user asked you to DO something in their Construct spaces. Use the available space-action tools to carry it out.
Rules:
- To reference an existing project, board, calendar, or issue, FIRST call the relevant "list" tool to discover its id. Never invent ids.
- Fill in required parameters; infer sensible defaults (today/tomorrow for dates, the default/backlog status, etc.). Do not ask the user follow-up questions.
- Take the fewest actions needed and never repeat a create that already succeeded.
- When finished, reply with ONE short spoken sentence (English, couch-legible) confirming what you did. Do not output JSON or tool syntax in that final reply.`

// runActionAgent runs the bounded tool-calling loop. Returns a spoken sentence
// and the set of space ids it touched (so the client can refresh those widgets).
func runActionAgent(ctx context.Context, userToken, model, query string) (string, []string, error) {
	idx := getTvIndex()
	tools := actionTools(idx, query)
	if len(tools) == 0 {
		return "", nil, errNoActions
	}
	base, key, modelID, ok := resolveModel(ctx, userToken, pickToolModel(ctx, userToken, model))
	if !ok {
		return "", nil, errUnauthorized
	}
	messages := []map[string]any{
		{"role": "system", "content": actionSystem},
		{"role": "user", "content": query},
	}
	affected := map[string]bool{}
	for step := 0; step < 6; step++ {
		msg, err := openaiChatTools(ctx, base, key, modelID, messages, tools, 1024)
		if err != nil {
			return "", keysOf(affected), err
		}
		if len(msg.ToolCalls) == 0 {
			return strings.TrimSpace(msg.Content), keysOf(affected), nil
		}
		messages = append(messages, assistantTurn(msg))
		for _, tc := range msg.ToolCalls {
			space, action := splitToolName(tc.Function.Name)
			var params map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &params)
			content, rerr := runSpaceAction(ctx, userToken, space, spaceVersion(idx, space), action, params)
			if space != "" {
				affected[space] = true
			}
			if rerr != nil {
				content = "ERROR: " + rerr.Error()
			}
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": tc.ID, "content": clip(content, 4000),
			})
		}
	}
	return "I worked on that but it needed too many steps — please check the result.", keysOf(affected), nil
}

// assistantTurn rebuilds the assistant message (with tool_calls) for the next
// request, in the OpenAI wire shape.
func assistantTurn(msg *assistantMessage) map[string]any {
	calls := make([]map[string]any, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		calls = append(calls, map[string]any{
			"id": tc.ID, "type": "function",
			"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
		})
	}
	return map[string]any{"role": "assistant", "content": msg.Content, "tool_calls": calls}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// handleAct: ONE planner step of the action agent. The backend owns the catalog
// + model + multi-step reasoning, but tool calls are executed CLIENT-SIDE in the
// TV's already-loaded space bundle (the desktop in-app bridge mechanism) — so no
// signed actions.js / space-runtime is required. The client drives the loop:
//
//	1. POST { query, model }                 -> { done:false, tool_calls:[{id,space,action,version,args}], messages }
//	2. client runs each call via the bundle, then
//	   POST { query, model, messages, tool_results:[{id,content}] } -> next step …
//	3. … until { done:true, speak }
func handleAct(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query       string           `json:"query"`
		Model       string           `json:"model"`
		Messages    []map[string]any `json:"messages"`
		ToolResults []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"tool_results"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Query) == "" {
		writeJSON(w, 400, map[string]any{"error": "missing query"})
		return
	}
	query := strings.TrimSpace(in.Query)
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	idx := getTvIndex()
	tools := actionTools(idx, query)
	if len(tools) == 0 {
		writeJSON(w, 200, map[string]any{"done": true, "ok": false, "speak": "I don't have any actions for that yet."})
		return
	}
	base, key, modelID, ok := resolveModel(r.Context(), token, pickToolModel(r.Context(), token, in.Model))
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}

	messages := in.Messages
	if len(messages) == 0 {
		messages = []map[string]any{
			{"role": "system", "content": actionSystem},
			{"role": "user", "content": query},
		}
	}
	for _, tr := range in.ToolResults {
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": tr.ID, "content": clip(tr.Content, 4000)})
	}

	msg, err := openaiChatTools(r.Context(), base, key, modelID, messages, tools, 1024)
	if err != nil {
		writeJSON(w, 200, map[string]any{"done": true, "ok": false, "speak": "Sorry, I couldn't do that.", "error": err.Error()})
		return
	}
	if len(msg.ToolCalls) == 0 {
		speak := strings.TrimSpace(msg.Content)
		if speak == "" {
			speak = "Done."
		}
		writeJSON(w, 200, map[string]any{"done": true, "ok": true, "speak": speak})
		return
	}
	messages = append(messages, assistantTurn(msg))
	calls := make([]map[string]any, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		space, action := splitToolName(tc.Function.Name)
		calls = append(calls, map[string]any{
			"id": tc.ID, "space": space, "action": action,
			"version": spaceVersion(idx, space), "args": tc.Function.Arguments,
		})
	}
	writeJSON(w, 200, map[string]any{"done": false, "tool_calls": calls, "messages": messages})
}

// handleActions: the action/tool catalog shared with the agent (debug/inspection).
//
//	GET /api/actions -> { count, tools:[...] }
func handleActions(w http.ResponseWriter, r *http.Request) {
	tools := actionTools(getTvIndex(), r.URL.Query().Get("q"))
	writeJSON(w, 200, map[string]any{"count": len(tools), "tools": tools})
}

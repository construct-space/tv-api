// Construct TV — ambient TV dashboard backend.
//
// Serves the construct_tv frontend (embedded) and exposes:
//
//	GET  /api/health
//	GET  /api/weather?city=Ferizaj  (or ?lat=&lon=)  -> Open-Meteo (no key)
//	POST /api/ask  {query}  -> Construct inference -> typed screen-spec JSON
//
// The "cortex" reuses Construct's inference service (api/inference). If inference is
// unreachable (e.g. local dev with no INTERNAL_SHARED_SECRET), it falls back to a
// small local composer so the dashboard always responds.
//
// Stdlib only (net/http) — matches the api/status conventions.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Internal-Secret")
			h.Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- token usage telemetry (real, from inference responses) ----------------
var usageMu sync.Mutex
var usage = map[string]int{"requests": 0, "total_tokens": 0, "last_total": 0}

func recordUsage(total int) {
	usageMu.Lock()
	defer usageMu.Unlock()
	usage["requests"]++
	usage["total_tokens"] += total
	usage["last_total"] = total
}

func usageSnapshot() map[string]int {
	usageMu.Lock()
	defer usageMu.Unlock()
	out := map[string]int{}
	for k, v := range usage {
		out[k] = v
	}
	return out
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"status": "ok", "service": "tv",
			"model": env("TV_MODEL", defaultModel), "usage": usageSnapshot(),
		})
	})
	mux.HandleFunc("GET /api/weather", handleWeather)
	mux.HandleFunc("POST /api/ask", handleAsk)
	mux.HandleFunc("POST /api/summarize", handleSummarize)
	mux.HandleFunc("POST /api/pick-widgets", handlePickWidgets)
	mux.HandleFunc("POST /api/arrange", handleArrange)
	mux.HandleFunc("POST /api/act", handleAct)
	mux.HandleFunc("GET /api/actions", handleActions)
	mux.HandleFunc("GET /api/models", handleModels)
	mux.HandleFunc("GET /api/agenda", handleAgenda)

	// device-code login (TV shows a code; the Construct desktop links it)
	mux.HandleFunc("POST /api/device/start", startDevice)
	mux.HandleFunc("GET /api/device/poll", pollDevice)
	mux.HandleFunc("POST /api/device/link", linkDevice)
	mux.HandleFunc("POST /api/device/token", handleDeviceToken)

	// spaces: list the marketplace catalog + serve unpacked bundle entries
	mux.HandleFunc("GET /api/spaces", handleSpacesList)
	mux.HandleFunc("GET /api/tv-spaces", handleTvSpaces)
	mux.HandleFunc("GET /api/spaces/{id}/{entry...}", handleSpaceEntry)

	// same-origin proxy so space bundles can reach Construct backends without CORS
	mux.HandleFunc("/api/source/", handleSourceProxy)
	mux.HandleFunc("/api/graphql", handleGraphProxy)

	// embedded SPA (web/) with index fallback
	sub, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			if _, err := fs.Stat(sub, strings.TrimPrefix(r.URL.Path, "/")); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		b, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "index missing", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})

	port := env("PORT", "8087")
	log.Printf("[tv] listening on :%s (model=%s, inference=%s)", port, env("TV_MODEL", defaultModel), inferenceURL())
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

var httpClient = &http.Client{Timeout: 60 * time.Second}

// --------------------------------------------------------------------------- //
// Weather — Open-Meteo (free, no key)
// --------------------------------------------------------------------------- //
func handleWeather(w http.ResponseWriter, r *http.Request) {
	lat := r.URL.Query().Get("lat")
	lon := r.URL.Query().Get("lon")
	city := r.URL.Query().Get("city")
	if city == "" && lat == "" {
		city = env("TV_DEFAULT_CITY", "Ferizaj")
	}
	name := city
	country := ""
	if lat == "" || lon == "" {
		g, err := geocode(city)
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": "geocode failed"})
			return
		}
		lat, lon, name, country = g.lat, g.lon, g.name, g.country
	}
	u := "https://api.open-meteo.com/v1/forecast?latitude=" + lat + "&longitude=" + lon +
		"&current=temperature_2m,relative_humidity_2m,apparent_temperature,weather_code,wind_speed_10m" +
		"&hourly=temperature_2m,weather_code&daily=weather_code,temperature_2m_max,temperature_2m_min," +
		"precipitation_probability_max,sunrise,sunset&timezone=auto&forecast_days=6"
	body, err := getBytes(u)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "forecast failed"})
		return
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		writeJSON(w, 502, map[string]any{"error": "forecast parse"})
		return
	}
	raw["city"] = name
	raw["country"] = country
	writeJSON(w, 200, raw)
}

type geo struct{ lat, lon, name, country string }

func geocode(city string) (geo, error) {
	u := "https://geocoding-api.open-meteo.com/v1/search?count=1&language=en&format=json&name=" + url.QueryEscape(city)
	b, err := getBytes(u)
	if err != nil {
		return geo{}, err
	}
	var res struct {
		Results []struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Name      string  `json:"name"`
			Country   string  `json:"country"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &res); err != nil || len(res.Results) == 0 {
		return geo{}, io.EOF
	}
	r0 := res.Results[0]
	return geo{
		lat:  trimFloat(r0.Latitude),
		lon:  trimFloat(r0.Longitude),
		name: r0.Name, country: r0.Country,
	}, nil
}

func getBytes(u string) ([]byte, error) {
	resp, err := httpClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func trimFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// --------------------------------------------------------------------------- //
// Ask — Construct inference -> typed screen-spec
// --------------------------------------------------------------------------- //
const defaultModel = "source-medium"

// buildSystemPrompt injects the live TV-widget catalog so the model can choose
// to render real space widgets (mail, calendar, board, tasks…) when the request
// maps to them, and fall back to a composed screen otherwise.
func buildSystemPrompt(catalog string) string {
	if catalog == "" {
		catalog = "(no widgets available yet)"
	}
	return `You are the Construct TV assistant — the voice of Construct on the TV across
the room from the viewer. Your name is "Construct" (never "Jarvis" or "Ambient"); if
asked who you are, say you are the Construct TV assistant. You can either render WIDGETS
from the user's installed spaces, or compose an informational screen.

Available TV widgets (format: space/widget (size) — name):
` + catalog + `

Given the request, return ONLY one json object (no markdown, no commentary):
{
 "title": string,            // short, bold headline
 "speak": string,            // ONE natural spoken sentence
 "widgets": [                // pick the widgets most relevant to the request; [] if none fit
   {"space": string, "widget": string, "size": string}
 ],
 "blocks": [                 // ONLY when no widget fits (general knowledge questions)
   {"type":"text","heading":string,"body":string},
   {"type":"stat","label":string,"value":string},
   {"type":"list","heading":string,"items":[string]},
   {"type":"image","caption":string,"initials":string}
 ]
}
Rules:
- Prefer "widgets" whenever the request relates to an available space (e.g. "how is project X",
  "check my calendar", "any mail", "what are my tasks"). Choose every relevant widget; use the
  size shown in the catalog.
- Use "blocks" only for general questions with no matching widget (then include one "image" block).
- Never invent space/widget ids that are not in the catalog.
- Keep strings short and couch-legible. Write in English.`
}

func inferenceURL() string {
	return strings.TrimRight(env("INFERENCE_URL", "https://llm.lisaos.dev"), "/")
}

func handleAsk(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query string `json:"query"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Query) == "" {
		writeJSON(w, 400, map[string]any{"error": "missing query"})
		return
	}
	query := strings.TrimSpace(in.Query)
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	started := time.Now()

	// Deterministic first: map the request directly to available TV widgets. This
	// makes voice reliable regardless of whether inference is reachable. Only fall
	// back to the model (composed screen) when no widget intent matches.
	spec := localWidgetSpec(query)
	source := "match"
	tokens := 0
	if !hasWidgets(spec) {
		// General question → the operator (light agent harness, runs as the
		// user: web + their Space actions). Falls back to a raw inference
		// compose, then a local screen, when the operator isn't configured.
		if ans, ok := askOperator(r.Context(), token, query); ok {
			spec, source, tokens = operatorSpec(query, ans), "operator", 0
		} else if s2, t2, src2, err := compose(r.Context(), token, in.Model, query); err == nil {
			spec, tokens, source = s2, t2, src2
		} else {
			spec, source = fallbackSpec(query), "local"
		}
	}
	writeJSON(w, 200, map[string]any{
		"spec": spec,
		"meta": map[string]any{
			"source": source, "tokens": tokens,
			"ms":    time.Since(started).Milliseconds(),
			"model": env("TV_MODEL", defaultModel),
		},
	})
}

func hasWidgets(spec map[string]any) bool {
	ws, ok := spec["widgets"].([]map[string]any)
	return ok && len(ws) > 0
}

// localWidgetSpec maps a natural request to available TV widgets by matching the
// space id/name/widget names plus common synonyms. Returns a spec with a
// "widgets" array (empty if nothing matched).
func localWidgetSpec(query string) map[string]any {
	q := strings.ToLower(query)
	idx := getTvIndex()

	// synonym → space id hints
	syn := map[string][]string{
		"mail":     {"mail", "email", "inbox", "message"},
		"calendar": {"calendar", "agenda", "schedule", "event", "meeting", "appointment", "week", "day", "today", "tomorrow", "my day", "my week"},
		"weather":  {"weather", "forecast", "temperature", "rain"},
		"clock":    {"clock", "time", "timer", "alarm", "stopwatch"},
		"news":     {"news", "headline"},
		"finance":  {"finance", "money", "stock", "market", "portfolio", "budget"},
		"tasks":    {"task", "todo", "to-do"},
		"board":    {"board", "kanban", "project"},
		"notes":    {"note", "memo"},
	}

	picked := make([]map[string]any, 0, 4)
	for _, s := range idx {
		hit := strings.Contains(q, strings.ToLower(s.ID)) || (s.Name != "" && strings.Contains(q, strings.ToLower(s.Name)))
		if !hit {
			for _, kw := range syn[s.ID] {
				if strings.Contains(q, kw) {
					hit = true
					break
				}
			}
		}
		if !hit {
			for _, wdesc := range s.TV {
				if n, _ := wdesc["name"].(string); n != "" && strings.Contains(q, strings.ToLower(n)) {
					hit = true
					break
				}
			}
		}
		if !hit || len(s.TV) == 0 {
			continue
		}
		w := s.TV[0]
		id, _ := w["id"].(string)
		size, _ := w["defaultSize"].(string)
		picked = append(picked, map[string]any{"space": s.ID, "widget": id, "size": size})
	}

	speak := "Here you go."
	if len(picked) == 0 {
		speak = ""
	}
	return map[string]any{"title": strings.Title(query), "widgets": picked, "speak": speak}
}

func orgKey() string { return os.Getenv("ORG_API_KEY") }

// inferenceBase returns the OpenAI-compatible base. With an ORG key we hit the
// provider's org surface (which exposes every model the org configured, e.g.
// MiniMax); without one we use the per-user gateway path (billed to the user).
func inferenceBase() string {
	if orgKey() != "" {
		return strings.TrimRight(env("INFERENCE_BASE", gatewayBase()+"/api/inference/v1"), "/")
	}
	return strings.TrimRight(env("INFERENCE_BASE", gatewayBase()+"/api/construct"), "/")
}

// chatComplete runs one chat completion, routing by model id:
//   - "provider:model" (e.g. minimax:MiniMax-M2.7) → the org's provider key +
//     base_url, called directly (the user's token unlocks the org key).
//   - anything else (e.g. source-medium) → Construct-managed inference via the
//     gateway with the user's token (or a global ORG_API_KEY if configured).
func chatComplete(ctx context.Context, userToken, model, system, user string, maxTokens int) (string, int, error) {
	if model == "" {
		model = env("TV_MODEL", defaultModel)
	}
	// Provider-routed model (org key path).
	if i := strings.IndexByte(model, ':'); i > 0 && userToken != "" {
		provider, modelID := model[:i], model[i+1:]
		cat := sourceCatalog(ctx, userToken)
		if ci, ok := cat[provider]; ok && ci.BaseURL != "" {
			if key := orgProviderKey(ctx, userToken, provider); key != "" {
				return openaiChat(ctx, ci.BaseURL, key, modelID, system, user, maxTokens)
			}
		}
		// fall through to construct path if provider/key unavailable
	}
	// Construct-managed path.
	auth := userToken
	if k := orgKey(); k != "" {
		auth = k
	}
	if auth == "" {
		return "", 0, io.EOF
	}
	return openaiChat(ctx, inferenceBase(), auth, model, system, user, maxTokens)
}

// handleModels lists the models the TV can use: the org's provider models
// (provider:model) plus the Construct-managed default, for the Settings picker.
func handleModels(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	ids := []string{env("TV_MODEL", defaultModel)}
	ids = append(ids, listOrgModels(r.Context(), token)...)
	writeJSON(w, 200, map[string]any{"models": ids, "selected": env("TV_MODEL", defaultModel), "org": len(ids) > 1})
}

func compose(ctx context.Context, token, model, query string) (map[string]any, int, string, error) {
	content, tokens, err := chatComplete(ctx, token, model, buildSystemPrompt(tvWidgetCatalog()), query, 1400)
	if err != nil {
		return nil, 0, "", err
	}
	spec, err := extractJSON(content)
	if err != nil {
		return nil, 0, "", err
	}
	return spec, tokens, "inference", nil
}

// handleSummarize reads/summarizes arbitrary text (e.g. a widget's contents the
// frontend scraped) into a couch-friendly spoken line + short bullets.
func handleSummarize(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query string `json:"query"`
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
		writeJSON(w, 400, map[string]any{"error": "missing text"})
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	sys := `You are the Construct TV assistant. Summarize the provided content for someone across the room.
Return ONLY json: {"speak": one natural spoken sentence (<= 30 words), "title": short headline, "points": [up to 5 short bullet strings]}.
Honor the user's request (e.g. "top 5", "latest"). Keep it concise and factual.`
	usr := "Request: " + in.Query + "\n\nContent:\n" + clip(in.Text, 6000)
	// Generous budget: reasoning models (e.g. Kimi) spend tokens thinking before
	// the answer; too small a cap returns empty content (finish_reason=length).
	content, _, err := chatComplete(r.Context(), token, in.Model, sys, usr, 1200)
	if err != nil {
		writeJSON(w, 200, map[string]any{"speak": "", "title": "", "points": []string{}, "source": "local"})
		return
	}
	spec, perr := extractJSON(content)
	if perr != nil {
		writeJSON(w, 200, map[string]any{"speak": clip(content, 240), "title": "", "points": []string{}})
		return
	}
	writeJSON(w, 200, spec)
}

// handlePickWidgets lets the model arrange widgets onto a page: given a topic
// ("company/org related", "project") it picks the relevant TV widgets from the
// catalog. The frontend drops them onto the target page.
func handlePickWidgets(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Topic string `json:"topic"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Topic) == "" {
		writeJSON(w, 400, map[string]any{"error": "missing topic"})
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	sys := `Pick the TV widgets relevant to the user's topic from the catalog below.
Return ONLY json: {"widgets":[{"space":"<id>","widget":"<id>","size":"<WxH>"}]}.
Use exact space/widget ids and the size shown. Include every widget that fits the
topic (e.g. "company/org" → board, crm, people, payroll, invoicing, finance, mail;
"project" → board, tasks, calendar, mail). Empty array if nothing fits.

Available TV widgets (space/widget (size) — name):
` + tvWidgetCatalog()
	content, _, err := chatComplete(r.Context(), token, in.Model, sys, "Topic: "+in.Topic, 800)
	if err != nil {
		writeJSON(w, 200, map[string]any{"widgets": []any{}})
		return
	}
	spec, perr := extractJSON(content)
	if perr != nil {
		writeJSON(w, 200, map[string]any{"widgets": []any{}})
		return
	}
	// Validate against the real catalog — drop any space/widget the model invented.
	valid := map[string]bool{}
	for _, s := range getTvIndex() {
		for _, wd := range s.TV {
			if id, _ := wd["id"].(string); id != "" {
				valid[s.ID+"/"+id] = true
			}
		}
	}
	if raw, ok := spec["widgets"].([]any); ok {
		kept := make([]any, 0, len(raw))
		for _, it := range raw {
			if m, ok := it.(map[string]any); ok {
				sp, _ := m["space"].(string)
				wd, _ := m["widget"].(string)
				if valid[sp+"/"+wd] {
					kept = append(kept, m)
				}
			}
		}
		spec["widgets"] = kept
	}
	writeJSON(w, 200, spec)
}

// handleArrange edits the whole page layout. The model is given the current
// pages + the widget catalog, so it can add/remove/move widgets, create/rename
// pages, and answer questions ("what's on page 3"). Returns the updated pages.
func handleArrange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query  string           `json:"query"`
		Active int              `json:"active"`
		Pages  []map[string]any `json:"pages"`
		Model  string           `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Query) == "" {
		writeJSON(w, 400, map[string]any{"error": "missing query"})
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	pagesJSON, _ := json.Marshal(in.Pages)
	activeJSON, _ := json.Marshal(in.Active)
	sys := `You manage a TV dashboard's pages of widgets. Given the CURRENT pages and the
user's request, return the UPDATED pages. Preserve existing pages and widgets
unless the request changes them — only add/remove/move/reorder what's asked, or
create/rename pages. For a question ("what's on page 3") return the pages
unchanged and answer in "speak". Use exact space/widget ids and the size shown.

Return ONLY json:
{"speak": "<one short sentence>", "pages": [{"name": string, "widgets": [{"space": string, "widget": string, "size": "<WxH>"}]}], "goto": <0-based page index to show, optional>}

The active page index is ` + string(activeJSON) + `.

Available TV widgets (space/widget (size) — name):
` + tvWidgetCatalog() + `
Current pages (JSON):
` + string(pagesJSON)
	content, _, err := chatComplete(r.Context(), token, in.Model, sys, in.Query, 1600)
	if err != nil {
		writeJSON(w, 200, map[string]any{"speak": "", "pages": in.Pages})
		return
	}
	spec, perr := extractJSON(content)
	if perr != nil {
		writeJSON(w, 200, map[string]any{"speak": "", "pages": in.Pages})
		return
	}
	// Validate every widget against the real catalog — drop invented ones.
	valid := map[string]bool{}
	for _, s := range getTvIndex() {
		for _, wd := range s.TV {
			if id, _ := wd["id"].(string); id != "" {
				valid[s.ID+"/"+id] = true
			}
		}
	}
	if ps, ok := spec["pages"].([]any); ok {
		for _, p := range ps {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if ws, ok := pm["widgets"].([]any); ok {
				kept := make([]any, 0, len(ws))
				for _, it := range ws {
					if m, ok := it.(map[string]any); ok {
						sp, _ := m["space"].(string)
						wd, _ := m["widget"].(string)
						if valid[sp+"/"+wd] {
							kept = append(kept, m)
						}
					}
				}
				pm["widgets"] = kept
			}
		}
	}
	writeJSON(w, 200, spec)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// extractJSON returns the first balanced {...} object as a map (robust to prose).
func extractJSON(text string) (map[string]any, error) {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return nil, io.EOF
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(text); i++ {
		c := text[i]
		switch {
		case inStr:
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				var m map[string]any
				if err := json.Unmarshal([]byte(text[start:i+1]), &m); err != nil {
					return nil, err
				}
				return m, nil
			}
		}
	}
	return nil, io.EOF
}

func fallbackSpec(query string) map[string]any {
	return map[string]any{
		"title":    strings.Title(query),
		"subtitle": "Composed locally — inference unavailable",
		"blocks": []map[string]any{
			{"type": "text", "heading": "You asked", "body": query},
			{"type": "stat", "label": "Source", "value": "Local"},
			{"type": "image", "caption": strings.Title(query), "initials": initials(query)},
		},
		"speak": "Here is a local screen for " + query + ".",
	}
}

func initials(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return "—"
	}
	if len(parts) == 1 {
		if len(parts[0]) >= 2 {
			return strings.ToUpper(parts[0][:2])
		}
		return strings.ToUpper(parts[0])
	}
	return strings.ToUpper(string(parts[0][0]) + string(parts[len(parts)-1][0]))
}

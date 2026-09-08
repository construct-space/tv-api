// Spaces — list marketplace spaces and serve their bundle entries to the TV.
//
// The desktop installs a `.space` ZIP to disk and reads manifest.json + the
// IIFE off the filesystem. The TV has no filesystem store, so this backend
// fetches the marketplace tarball once, unpacks it in memory, and serves the
// entries over HTTP. The browser's space loader then evals app.iife.js and
// reads window.__CONSTRUCT_SPACE_<ID>.
//
//   GET /api/spaces                     -> { spaces: [{id,name,icon,version,description}] }
//   GET /api/spaces/{id}/{entry...}     -> raw entry (manifest.json, app.iife.js, style.css, ...)
//
// Bundles are public marketplace content; the user's forwarded token only
// matters once data flows (graph/source), which the frontend handles directly.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

func marketplaceURL() string {
	return strings.TrimRight(env("MARKETPLACE_URL", "https://my.lisaos.dev/api/marketplace"), "/")
}

// ---- in-memory bundle cache ------------------------------------------------
type bundle struct {
	entries map[string][]byte
	fetched time.Time
}

var bundleMu sync.Mutex
var bundles = map[string]*bundle{}

const bundleTTL = 30 * time.Minute

// handleSpacesList proxies the marketplace catalog into a slim TV shape.
func handleSpacesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	u := marketplaceURL() + "/spaces?pageSize=60"
	if q != "" {
		u += "&q=" + url.QueryEscape(q)
	}
	body, err := getBytes(u)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "marketplace unreachable", "spaces": []any{}})
		return
	}
	var raw struct {
		Spaces []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Icon        string `json:"icon"`
			Version     string `json:"version"`
			Description string `json:"description"`
		} `json:"spaces"`
	}
	if json.Unmarshal(body, &raw) != nil {
		writeJSON(w, 502, map[string]any{"error": "bad marketplace response", "spaces": []any{}})
		return
	}
	out := make([]map[string]any, 0, len(raw.Spaces))
	for _, s := range raw.Spaces {
		out = append(out, map[string]any{
			"id": s.ID, "name": s.Name, "icon": s.Icon,
			"version": s.Version, "description": s.Description,
		})
	}
	writeJSON(w, 200, map[string]any{"spaces": out})
}

// ---- TV widget index --------------------------------------------------------
// A space appears on the TV only if its manifest declares a non-empty `tv`
// array (TV-tailored widgets). We discover these by scanning bundle manifests
// (the marketplace list manifest is slim and omits widgets/tv), cached.
type tvSpace struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	Icon    string               `json:"icon"`
	Version string               `json:"version"`
	TV      []map[string]any     `json:"tv"`
	Actions map[string]actionDef `json:"actions,omitempty"`
}

// actionDef / actionParam mirror a space manifest's `actions` block — the
// declarative tool catalog each space ships (id → {description, params}). We read
// these the same way we read `tv`, then expose them to the agent as callable
// tools (see actions.go). Execution is headless via the space-runtime.
type actionParam struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}
type actionDef struct {
	Description string                 `json:"description"`
	Params      map[string]actionParam `json:"params"`
}

var tvIdxMu sync.Mutex
var tvIdx []tvSpace
var tvIdxAt time.Time

const tvIdxTTL = 15 * time.Minute

func handleTvSpaces(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") != "" {
		tvIdxMu.Lock()
		tvIdx = nil // bust cache so a freshly published space appears immediately
		tvIdxMu.Unlock()
	}
	writeJSON(w, 200, map[string]any{"spaces": getTvIndex()})
}

// getTvIndex returns spaces with a non-empty manifest.tv array, scanning bundle
// manifests in parallel (the marketplace list manifest omits widgets/tv). Cached.
func getTvIndex() []tvSpace {
	tvIdxMu.Lock()
	if tvIdx != nil && time.Since(tvIdxAt) < tvIdxTTL {
		out := tvIdx
		tvIdxMu.Unlock()
		return out
	}
	tvIdxMu.Unlock()

	body, err := getBytes(marketplaceURL() + "/spaces?pageSize=200")
	if err != nil {
		return []tvSpace{}
	}
	var raw struct {
		Spaces []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Icon    string `json:"icon"`
			Version string `json:"version"`
		} `json:"spaces"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return []tvSpace{}
	}

	out := make([]tvSpace, 0, 8)
	var mu sync.Mutex
	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	for _, item := range raw.Spaces {
		item := item
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			b, err := getBundle(item.ID)
			if err != nil {
				return
			}
			mj, ok := b.entries["manifest.json"]
			if !ok {
				return
			}
			var m struct {
				TV      []map[string]any     `json:"tv"`
				Actions map[string]actionDef `json:"actions"`
			}
			if json.Unmarshal(mj, &m) != nil || len(m.TV) == 0 {
				return
			}
			mu.Lock()
			out = append(out, tvSpace{
				ID: item.ID, Name: item.Name, Icon: item.Icon,
				Version: item.Version, TV: m.TV, Actions: m.Actions,
			})
			mu.Unlock()
		}()
	}
	wg.Wait()

	tvIdxMu.Lock()
	tvIdx = out
	tvIdxAt = time.Now()
	tvIdxMu.Unlock()
	return out
}

// tvWidgetCatalog renders the available TV widgets as a compact list for the
// model: "space/widget (size) — name". Used to let voice pick relevant widgets.
func tvWidgetCatalog() string {
	idx := getTvIndex()
	var b strings.Builder
	for _, s := range idx {
		for _, w := range s.TV {
			id, _ := w["id"].(string)
			name, _ := w["name"].(string)
			size, _ := w["defaultSize"].(string)
			if id == "" {
				continue
			}
			b.WriteString(s.ID + "/" + id)
			if size != "" {
				b.WriteString(" (" + size + ")")
			}
			if name != "" {
				b.WriteString(" — " + name)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// handleSpaceEntry serves one file from a space's (cached) bundle.
func handleSpaceEntry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry := path.Clean("/" + r.PathValue("entry"))[1:] // strip leading slash, prevent traversal
	if id == "" || entry == "" {
		writeJSON(w, 400, map[string]any{"error": "bad path"})
		return
	}
	b, err := getBundle(id)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "could not load bundle: " + err.Error()})
		return
	}
	data, ok := b.entries[entry]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType(entry))
	w.Header().Set("Cache-Control", "public, max-age=600")
	_, _ = w.Write(data)
}

func getBundle(id string) (*bundle, error) {
	bundleMu.Lock()
	if b, ok := bundles[id]; ok && time.Since(b.fetched) < bundleTTL {
		bundleMu.Unlock()
		return b, nil
	}
	bundleMu.Unlock()

	tarballURL, err := resolveTarball(id)
	if err != nil {
		return nil, err
	}
	raw, err := getBytes(tarballURL)
	if err != nil {
		return nil, err
	}
	entries, err := unpack(raw)
	if err != nil {
		return nil, err
	}
	b := &bundle{entries: entries, fetched: time.Now()}
	bundleMu.Lock()
	bundles[id] = b
	bundleMu.Unlock()
	return b, nil
}

func resolveTarball(id string) (string, error) {
	body, err := getBytes(marketplaceURL() + "/spaces/" + url.PathEscape(id))
	if err != nil {
		return "", err
	}
	var m struct {
		TarballURL string `json:"tarball_url"`
	}
	if json.Unmarshal(body, &m) != nil || m.TarballURL == "" {
		return "", &simpleErr{"no tarball_url for space"}
	}
	return m.TarballURL, nil
}

// unpack handles both a ZIP `.space` and a gzipped tar (marketplace formats vary).
func unpack(raw []byte) (map[string][]byte, error) {
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		return unpackTarGz(raw)
	}
	if len(raw) >= 4 && raw[0] == 'P' && raw[1] == 'K' {
		return unpackZip(raw)
	}
	// Some endpoints return a nested {id}.space ZIP inside a tar; try zip then targz.
	if e, err := unpackZip(raw); err == nil {
		return e, nil
	}
	return unpackTarGz(raw)
}

func unpackZip(raw []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		out[normalizeEntry(f.Name)] = data
	}
	return out, nil
}

func unpackTarGz(raw []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[normalizeEntry(hdr.Name)] = data
	}
	return out, nil
}

// normalizeEntry strips a single leading top-level dir (marketplace tarballs
// wrap everything in "<id>.space/") so "mail.space/manifest.json" resolves as
// "manifest.json". Only the first path segment is dropped; nested paths
// (agent/skills/data.md) keep their structure below it.
func normalizeEntry(name string) string {
	name = strings.TrimPrefix(name, "./")
	if i := strings.IndexByte(name, '/'); i >= 0 {
		first := name[:i]
		if strings.HasSuffix(first, ".space") {
			return name[i+1:]
		}
	}
	return name
}

func contentType(entry string) string {
	switch {
	case strings.HasSuffix(entry, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(entry, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(entry, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(entry, ".svg"):
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

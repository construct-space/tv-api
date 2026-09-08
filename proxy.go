// Same-origin proxy for Construct backends.
//
// Space bundles (e.g. mail) run in the TV's browser at https://tv.lisaos.dev
// and call the Construct gateway (api/source) and the GraphQL bridge. In the
// desktop app those calls work because Tauri bypasses CORS; in a real browser
// they're cross-origin and blocked. So we expose them under the TV's OWN origin
// and forward server-side (no CORS):
//
//   /api/source/*  ->  {GATEWAY_URL}/api/source/*
//   /api/graphql   ->  {GRAPH_URL}/graphql
//
// The user's bearer token (from device-link) is forwarded as-is, so the space
// reads the real account's data. This matches mail's own default apiBase
// fallback of `/api/source`.
package main

import (
	"io"
	"net/http"
	"strings"
)

func gatewayBase() string { return strings.TrimRight(env("GATEWAY_URL", "https://my.lisaos.dev"), "/") }
func graphBase() string   { return strings.TrimRight(env("GRAPH_URL", "https://graph.lisaos.dev"), "/") }

func handleSourceProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/source")
	target := gatewayBase() + "/api/source" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	proxyPass(w, r, target)
}

func handleGraphProxy(w http.ResponseWriter, r *http.Request) {
	proxyPass(w, r, graphBase()+"/graphql")
}

// proxyPass forwards the request (method, body, auth + a few relevant headers)
// to target and streams the response back.
func proxyPass(w http.ResponseWriter, r *http.Request, target string) {
	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "proxy request build failed"})
		return
	}
	for _, h := range []string{"Authorization", "Content-Type", "Accept", "X-Space-ID", "X-Project-ID"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "upstream unreachable"})
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

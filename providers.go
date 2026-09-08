// Org provider routing — the TV analogue of brain/provider/orgkeys.go.
//
// The user's token carries org membership, so we can: list the org's configured
// providers (/api/source/org/providers), read each provider's base_url + models
// from the catalog (/api/source/providers), and fetch the raw key on demand
// (/api/source/org/providers/{id}/key). With those we call the provider's
// OpenAI-compatible endpoint directly — letting the TV switch between MiniMax,
// DeepSeek, Kimi, etc. without any local key. Model ids are "provider:model".
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type provCatItem struct {
	BaseURL string
	Models  []string
}

var catMu sync.Mutex
var catCache map[string]provCatItem
var catAt time.Time

// sourceCatalog: provider → {base_url, model ids} for providers with a key
// configured. Cached 5 min (base URLs/models rarely change).
func sourceCatalog(ctx context.Context, token string) map[string]provCatItem {
	catMu.Lock()
	if catCache != nil && time.Since(catAt) < 5*time.Minute {
		c := catCache
		catMu.Unlock()
		return c
	}
	catMu.Unlock()
	if token == "" {
		return catCache
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", gatewayBase()+"/api/source/providers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return catCache
	}
	defer resp.Body.Close()
	type provRow struct {
		ID     string `json:"id"`
		APIKey struct {
			Enabled bool   `json:"enabled"`
			BaseURL string `json:"base_url"`
		} `json:"api_key"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	var raw struct {
		Data      []provRow `json:"data"`
		Providers []provRow `json:"providers"`
	}
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &raw) != nil {
		return catCache
	}
	rows := raw.Data
	if len(rows) == 0 {
		rows = raw.Providers
	}
	out := map[string]provCatItem{}
	for _, p := range rows {
		if !p.APIKey.Enabled {
			continue
		}
		ms := make([]string, 0, len(p.Models))
		for _, m := range p.Models {
			ms = append(ms, m.ID)
		}
		out[p.ID] = provCatItem{BaseURL: p.APIKey.BaseURL, Models: ms}
	}
	catMu.Lock()
	catCache = out
	catAt = time.Now()
	catMu.Unlock()
	return out
}

// orgProviderIDs: providers the user's org has configured (with keys).
func orgProviderIDs(ctx context.Context, token string) map[string]bool {
	req, _ := http.NewRequestWithContext(ctx, "GET", gatewayBase()+"/api/source/org/providers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil || resp == nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var rows []struct {
		Provider string `json:"provider"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &rows)
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Provider] = true
	}
	return out
}

// orgProviderKey: the raw API key for a provider (short-lived; never cached).
func orgProviderKey(ctx context.Context, token, provider string) string {
	req, _ := http.NewRequestWithContext(ctx, "GET", gatewayBase()+"/api/source/org/providers/"+provider+"/key", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var d struct {
		APIKey string `json:"api_key"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &d)
	return d.APIKey
}

// listOrgModels: "provider:model" entries the user can pick (org providers only).
func listOrgModels(ctx context.Context, token string) []string {
	cat := sourceCatalog(ctx, token)
	org := orgProviderIDs(ctx, token)
	out := []string{}
	for pid := range org {
		ci, ok := cat[pid]
		if !ok {
			continue
		}
		for _, m := range ci.Models {
			out = append(out, pid+":"+m)
		}
	}
	return out
}

// resolveModel returns the OpenAI-compatible (baseURL, key, modelID) for a model
// id. For a "provider:model" id it routes through the org's configured provider
// key; otherwise it falls back to the Construct-managed inference endpoint. ok is
// false only when no usable credential is available.
func resolveModel(ctx context.Context, userToken, model string) (base, key, modelID string, ok bool) {
	if model == "" {
		model = env("TV_MODEL", defaultModel)
	}
	if i := strings.IndexByte(model, ':'); i > 0 && userToken != "" {
		provider, mid := model[:i], model[i+1:]
		cat := sourceCatalog(ctx, userToken)
		if ci, has := cat[provider]; has && ci.BaseURL != "" {
			if k := orgProviderKey(ctx, userToken, provider); k != "" {
				return ci.BaseURL, k, mid, true
			}
		}
	}
	auth := userToken
	if k := orgKey(); k != "" {
		auth = k
	}
	if auth == "" {
		return "", "", "", false
	}
	return inferenceBase(), auth, model, true
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type assistantMessage struct {
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls"`
}

// openaiChatTools: one OpenAI chat completion with function-calling enabled.
// messages is the running transcript (system/user/assistant/tool turns); tools is
// the function catalog. Returns the assistant's reply (content and/or tool_calls).
func openaiChatTools(ctx context.Context, baseURL, key, model string, messages []map[string]any, tools []map[string]any, maxTokens int) (*assistantMessage, error) {
	payload := map[string]any{
		"model": model, "max_tokens": maxTokens, "messages": messages,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	if strings.Contains(baseURL, "moonshot") {
		payload["thinking"] = map[string]any{"type": "disabled"}
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &simpleErr{"inference " + resp.Status + ": " + strings.TrimSpace(string(msg))}
	}
	var cr struct {
		Choices []struct {
			Message assistantMessage `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &cr) != nil || len(cr.Choices) == 0 {
		return nil, io.EOF
	}
	recordUsage(cr.Usage.TotalTokens)
	return &cr.Choices[0].Message, nil
}

// openaiChat: one non-streaming OpenAI chat completion against an arbitrary base.
func openaiChat(ctx context.Context, baseURL, key, model, system, user string, maxTokens int) (string, int, error) {
	payload := map[string]any{
		"model": model, "max_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	// Moonshot/Kimi is a reasoning model — disable thinking so it answers directly
	// (otherwise reasoning eats the token budget and content comes back empty).
	if strings.Contains(baseURL, "moonshot") {
		payload["thinking"] = map[string]any{"type": "disabled"}
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", 0, io.EOF
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &cr) != nil || len(cr.Choices) == 0 {
		return "", 0, io.EOF
	}
	recordUsage(cr.Usage.TotalTokens)
	return cr.Choices[0].Message.Content, cr.Usage.TotalTokens, nil
}

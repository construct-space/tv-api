// operator.go — route a general TV question to the cloud operator.
//
// Same harness mobile uses: instead of composing an answer with a raw
// inference call, hand the question to the Construct operator (api/operator),
// which runs a light agent loop *as the user* (their token) — it can search
// the web and run their Space actions, not just complete text. This is the
// exact call source-api makes for the device-bus cloud fallback:
//
//	POST {OPERATOR_URL}/internal/ask  (X-Internal-Secret)  {token,text,request_id} -> {content}
//
// Configured via OPERATOR_URL + INTERNAL_SHARED_SECRET (api/tv already has the
// secret for delegated-token minting). Returns ("", false) when unconfigured or
// on any error, so handleAsk falls back to the existing inference path.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// operatorClient gets its own timeout — the agent loop can take longer than the
// shared 60s httpClient allows.
var operatorClient = &http.Client{Timeout: 95 * time.Second}

func operatorBase() string { return strings.TrimRight(os.Getenv("OPERATOR_URL"), "/") }

// askOperator runs the question through the operator as the user. Returns the
// text answer and true on success; ("", false) means "not configured / failed —
// fall back to inference".
func askOperator(ctx context.Context, userToken, query string) (string, bool) {
	base, secret := operatorBase(), os.Getenv("INTERNAL_SHARED_SECRET")
	if base == "" || secret == "" || userToken == "" {
		return "", false
	}
	body, _ := json.Marshal(map[string]any{
		"token":      userToken,
		"text":       query,
		"request_id": "tv-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})
	// Stay under the TV server's 90s WriteTimeout — if the operator is slow we
	// bail and fall back to inference rather than have the response cut off.
	rctx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "POST", base+"/internal/ask", bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	req.Header.Set("X-Internal-Secret", secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := operatorClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var out struct {
		Content string `json:"content"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || strings.TrimSpace(out.Content) == "" {
		return "", false
	}
	return out.Content, true
}

// operatorSpec wraps the operator's answer as a TV screen-spec. The frontend
// speaks spec.speak and shows it in the conversation overlay; no widgets.
func operatorSpec(query, answer string) map[string]any {
	return map[string]any{
		"title":   strings.Title(query),
		"speak":   answer,
		"widgets": []map[string]any{},
	}
}

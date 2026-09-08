// Refresh tokens — keep the TV signed in without re-pairing.
//
// The forwarded desktop access token expires (~1h), which would otherwise log
// the TV out (widgets, models, inference all 401). Instead we issue the TV a
// long-lived, HMAC-signed *refresh token* that binds the user identity. The TV
// exchanges it at /api/device/token for a fresh short-lived access token minted
// by accounts (/internal/delegated-token) — a real cat_ token Graph/source/
// inference accept as the user. Stateless: survives backend restarts, no store.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

func refreshSecret() []byte { return []byte(os.Getenv("INTERNAL_SHARED_SECRET")) }

// signRefresh encodes the identity as "<b64url(payload)>.<b64url(hmac)>".
func signRefresh(id identity) string {
	payload, _ := json.Marshal(id)
	mac := hmac.New(sha256.New, refreshSecret())
	mac.Write(payload)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(mac.Sum(nil))
}

// verifyRefresh checks the HMAC and returns the embedded identity.
func verifyRefresh(tok string) (*identity, bool) {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 || len(refreshSecret()) == 0 {
		return nil, false
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(parts[0])
	sig, err2 := enc.DecodeString(parts[1])
	if err1 != nil || err2 != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, refreshSecret())
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, false
	}
	var id identity
	if json.Unmarshal(payload, &id) != nil || id.UserID == "" {
		return nil, false
	}
	return &id, true
}

func accountsBase() string {
	if u := os.Getenv("ACCOUNTS_INTERNAL_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return gatewayBase()
}

// mintDelegated asks accounts for a fresh short-lived access token to act as the
// user (the desktop pattern; see api/conductor/delegate.go).
func mintDelegated(userID string) (string, string, error) {
	secret := os.Getenv("INTERNAL_SHARED_SECRET")
	if secret == "" || userID == "" {
		return "", "", errUnauthorized
	}
	body, _ := json.Marshal(map[string]any{"user_id": userID, "ttl_secs": 3600, "scope": "automations"})
	req, _ := http.NewRequest("POST", accountsBase()+"/internal/delegated-token", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", secret)
	// accounts' CSRF middleware skips POSTs that carry an Authorization header;
	// the actual auth is X-Internal-Secret (goauth.Trusted), which ignores this
	// value. Without it the request is rejected as "missing CSRF token".
	req.Header.Set("Authorization", "Bearer internal")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", &simpleErr{"mint failed: " + strings.TrimSpace(string(msg))}
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Token == "" {
		return "", "", &simpleErr{"empty token"}
	}
	return out.Token, out.ExpiresAt, nil
}

// handleDeviceToken exchanges a device refresh token for a fresh access token.
//
//	POST /api/device/token  { "refresh": "<refresh>" } -> { token, expires_at, user }
func handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Refresh string `json:"refresh"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Refresh == "" {
		writeJSON(w, 400, map[string]any{"error": "missing refresh"})
		return
	}
	id, ok := verifyRefresh(in.Refresh)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "invalid refresh"})
		return
	}
	tok, exp, err := mintDelegated(id.UserID)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "mint failed", "detail": err.Error(), "base": accountsBase()})
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok, "expires_at": exp, "user": id})
}

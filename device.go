// Device-code login — desktop-approved.
//
// The TV shows a short code. The user types it in the Construct desktop app
// (Settings -> Profile -> "Link a TV"), which calls /api/device/link with the
// user's own Authorization. We validate that token against accounts
// (/internal/validate-token, gateway pattern) and bind the resulting identity +
// token to the device. The TV polls /api/device/poll and gets a real Construct
// session it can use as the user (provider / inference / spaces).
//
//   TV:      POST /api/device/start            -> { device_id, user_code }
//            GET  /api/device/poll?device_id   -> { approved, token, user }
//   Desktop: POST /api/device/link  Bearer <user>  {user_code} -> binds identity
package main

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

type identity struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	OrgID  string `json:"org_id,omitempty"`
}

type device struct {
	userCode string
	approved bool
	token    string // the user's Construct token, forwarded by the desktop
	user     identity
	created  time.Time
}

var devMu sync.Mutex
var devices = map[string]*device{}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no ambiguous chars

func randStr(n int, alphabet string) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	out := make([]byte, n)
	for i := range b {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(out)
}

func newUserCode() string { return randStr(4, codeAlphabet) + "-" + randStr(4, codeAlphabet) }

func gcDevices() {
	cut := time.Now().Add(-30 * time.Minute)
	for id, d := range devices {
		if !d.approved && d.created.Before(cut) {
			delete(devices, id)
		}
	}
}

func startDevice(w http.ResponseWriter, r *http.Request) {
	devMu.Lock()
	defer devMu.Unlock()
	gcDevices()
	id := randStr(24, "abcdefghijklmnopqrstuvwxyz0123456789")
	code := newUserCode()
	devices[id] = &device{userCode: code, created: time.Now()}
	writeJSON(w, 200, map[string]any{"device_id": id, "user_code": code})
}

func pollDevice(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("device_id")
	devMu.Lock()
	d := devices[id]
	devMu.Unlock()
	if d == nil {
		writeJSON(w, 404, map[string]any{"error": "unknown device"})
		return
	}
	out := map[string]any{"approved": d.approved}
	if d.approved {
		out["user"] = d.user
		// Long-lived refresh token (binds identity) + a fresh delegated access
		// token. The TV stores the refresh token and re-mints access tokens via
		// /api/device/token, so it never logs out when the access token expires.
		out["refresh"] = signRefresh(d.user)
		if tok, exp, err := mintDelegated(d.user.UserID); err == nil {
			out["token"] = tok
			out["expires_at"] = exp
		} else {
			out["token"] = d.token // fallback: forwarded desktop token
		}
	}
	writeJSON(w, 200, out)
}

// link — called by the (already authenticated) Construct desktop.
func linkDevice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad body"})
		return
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		writeJSON(w, 401, map[string]any{"error": "missing Authorization"})
		return
	}
	id, err := validateUser(auth)
	if err != nil {
		writeJSON(w, 401, map[string]any{"error": "invalid token"})
		return
	}
	code := strings.ToUpper(strings.TrimSpace(in.UserCode))
	devMu.Lock()
	defer devMu.Unlock()
	for _, d := range devices {
		if d.userCode == code && !d.approved {
			d.approved = true
			// Store the raw token (no "Bearer " prefix) — the frontend adds it
			// back when calling graph/source on the user's behalf.
			d.token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
			d.user = *id
			writeJSON(w, 200, map[string]any{"ok": true, "user": id})
			return
		}
	}
	writeJSON(w, 404, map[string]any{"error": "code not found or already linked"})
}

// validateUser verifies the user's token against the gateway and returns their
// profile. We use /api/auth/me (just needs the user's bearer) rather than the
// internal validate-token endpoint, because the latter returned empty identity
// headers — /api/auth/me carries the real profile (uuid, name, email).
func validateUser(authHeader string) (*identity, error) {
	req, _ := http.NewRequest("GET", gatewayBase()+"/api/auth/me", nil)
	req.Header.Set("Authorization", authHeader)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errUnauthorized
	}
	var body struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			UUID      string `json:"uuid"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
			Username  string `json:"username"`
			Email     string `json:"email"`
			OrgID     string `json:"org_id"`
		} `json:"user"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || !body.Authenticated {
		return nil, errUnauthorized
	}
	name := strings.TrimSpace(body.User.FirstName + " " + body.User.LastName)
	if name == "" {
		name = body.User.Username
	}
	return &identity{
		UserID: body.User.UUID,
		Email:  body.User.Email,
		Name:   name,
		OrgID:  body.User.OrgID,
	}, nil
}

var errUnauthorized = &simpleErr{"unauthorized"}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

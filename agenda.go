// Calendar agenda tile — TV-native, reads space-calendar's graph.
//
//	GET /api/agenda?from=<ISO>&to=<ISO>
//
// Returns the linked user's calendar events overlapping [from, to] (the TV
// passes its local day; defaults to now .. now+24h). We read space-calendar's
// `event` model over the Construct graph GraphQL endpoint, authenticating with
// the same Construct token the TV stored at device-link time (forwarded as
// Authorization). The org partition is resolved server-side from that identity.
//
// We speak the graph wire format directly (one POST, fixed query) instead of
// pulling in the JS SDK — mirroring @construct-space/graph's find(): a
// `query { events(where,orderBy,limit) { ... } }` against `${graphUrl}/graphql`
// with `X-Space-ID: calendar`.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func graphURL() string {
	return strings.TrimRight(env("GRAPH_URL", "https://graph.lisaos.dev"), "/")
}

type agendaEvent struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Location string `json:"location,omitempty"`
	Start    string `json:"start"`
	End      string `json:"end"`
	AllDay   bool   `json:"all_day"`
	Status   string `json:"status,omitempty"`
}

func handleAgenda(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		writeJSON(w, 401, map[string]any{"error": "not linked"})
		return
	}

	now := time.Now().UTC()
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	if from == "" {
		from = now.Format(time.RFC3339)
	}
	if to == "" {
		to = now.Add(24 * time.Hour).Format(time.RFC3339)
	}

	// Events overlapping [from, to]: start on/before the window end AND end
	// on/after the window start. Same range operators space-calendar uses.
	const query = `query($where: JSON, $orderBy: JSON, $limit: Int) { events(where: $where, orderBy: $orderBy, limit: $limit) { id title location start_date end_date all_day status } }`
	reqBody, _ := json.Marshal(map[string]any{
		"query": query,
		"variables": map[string]any{
			"where": map[string]any{
				"start_date": map[string]any{"$lte": to},
				"end_date":   map[string]any{"$gte": from},
			},
			"orderBy": map[string]any{"start_date": "asc"},
			"limit":   50,
		},
	})

	req, _ := http.NewRequestWithContext(r.Context(), "POST", graphURL()+"/graphql", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-Space-ID", "calendar")

	resp, err := httpClient.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "graph unreachable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	if resp.StatusCode != 200 {
		writeJSON(w, 502, map[string]any{"error": "graph error"})
		return
	}

	var parsed struct {
		Data struct {
			Events []struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				Location  string `json:"location"`
				StartDate string `json:"start_date"`
				EndDate   string `json:"end_date"`
				AllDay    bool   `json:"all_day"`
				Status    string `json:"status"`
			} `json:"events"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		writeJSON(w, 502, map[string]any{"error": "bad graph response"})
		return
	}
	if len(parsed.Errors) > 0 {
		writeJSON(w, 502, map[string]any{"error": parsed.Errors[0].Message})
		return
	}

	out := make([]agendaEvent, 0, len(parsed.Data.Events))
	for _, e := range parsed.Data.Events {
		if e.Status == "cancelled" {
			continue
		}
		out = append(out, agendaEvent{
			ID: e.ID, Title: e.Title, Location: e.Location,
			Start: e.StartDate, End: e.EndDate, AllDay: e.AllDay, Status: e.Status,
		})
	}
	writeJSON(w, 200, map[string]any{"events": out, "from": from, "to": to})
}

package sidecar

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type API struct {
	provider *SiriusProvider
	baseURL  string
}

func NewAPI(p *SiriusProvider, baseURL string) *API {
	return &API{provider: p, baseURL: strings.TrimRight(baseURL, "/")}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, 405, "method not allowed")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/stations", a.stations)
	mux.HandleFunc("/api/stations/", a.station)
	return mux
}
func (a *API) stations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	if r.URL.Path != "/api/stations" {
		writeError(w, 404, "not found")
		return
	}
	writeJSON(w, 200, a.provider.Stations())
}
func (a *API) station(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/stations/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, 404, "station not found")
		return
	}
	id := parts[0]
	var found bool
	for _, s := range a.provider.Stations() {
		if s.ID == id {
			found = true
			break
		}
	}
	if !found {
		writeError(w, 404, "station not found")
		return
	}
	switch {
	case len(parts) == 1:
		np, _ := a.provider.NowPlaying(id)
		name := ""
		for _, s := range a.provider.Stations() {
			if s.ID == id {
				name = s.Name
				break
			}
		}
		writeJSON(w, 200, map[string]any{"id": id, "name": name, "stream_url": a.baseURL + "/api/stations/" + id + "/stream", "content_type": "audio/mpeg", "title": np.Title})
	case len(parts) == 2 && parts[1] == "now-playing":
		np, err := a.provider.NowPlaying(id)
		if err != nil {
			writeError(w, 503, err.Error())
			return
		}
		writeJSON(w, 200, np)
	case len(parts) == 2 && parts[1] == "stream":
		if err := a.provider.Stream(id, w, r); err != nil && w.Header().Get("Content-Type") == "" {
			writeError(w, 502, err.Error())
		}
	default:
		writeError(w, 404, "not found")
	}
}
func ValidateBaseURL(v string) error {
	if strings.TrimSpace(v) == "" || strings.ContainsAny(v, " \t\r\n") {
		return fmt.Errorf("invalid base URL")
	}
	return nil
}

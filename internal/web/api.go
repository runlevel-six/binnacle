package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/runlevel-six/binnacle/internal/fleet"
)

// fleetResponse is the JSON shape of GET /api/v1/fleet and the SSE payload.
type fleetResponse struct {
	Clusters []fleet.ClusterView `json:"clusters"`
	Storage  fleet.Storage       `json:"storage"`
}

// handleAPIFleet returns the full fleet view as JSON.
func (s *Server) handleAPIFleet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, fleetResponse{
		Clusters: s.fleet.View(),
		Storage:  s.fleet.Storage(),
	})
}

// handleAPICluster returns one cluster's detail as JSON.
func (s *Server) handleAPICluster(w http.ResponseWriter, r *http.Request) {
	d, ok := s.fleet.Cluster(r.PathValue("namespace"), r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, d)
}

// handleAPIStorage returns the datacenter storage layer as JSON.
func (s *Server) handleAPIStorage(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.fleet.Storage())
}

// handleAPIEvents streams fleet state as JSON over Server-Sent Events.
//
// The first frame goes out immediately, so a client that reconnects after a
// network blip does not sit on nothing until the next change. A keepalive
// prevents an idle stream from being reaped by a proxy.
func (s *Server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() bool {
		resp := fleetResponse{
			Clusters: s.fleet.View(),
			Storage:  s.fleet.Storage(),
		}
		data, err := json.Marshal(resp)
		if err != nil {
			return false
		}
		fmt.Fprintf(w, "event: fleet\ndata: %s\n\n", data)
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.fleet.Changed():
			if !send() {
				return
			}
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// writeJSON encodes v as JSON and writes it, or an error — never half of each.
func writeJSON(w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf.Bytes())
}

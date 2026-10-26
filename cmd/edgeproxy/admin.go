package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"edgeproxy/internal/backend"
	"edgeproxy/internal/metrics"
)

// newAdminServer serves operational endpoints on their own port, so metrics
// and backend details are never exposed on the public listener. backends
// returns the backends currently in rotation; reload re-reads the config file.
func newAdminServer(addr string, m *metrics.Metrics, backends func() []*backend.Backend, draining *atomic.Bool, reload func() error) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())

	// Re-read and apply the config file. Same as sending SIGHUP, but works on
	// every OS and reports whether the new config was accepted.
	mux.HandleFunc("POST /-/reload", func(w http.ResponseWriter, _ *http.Request) {
		if err := reload(); err != nil {
			http.Error(w, "reload failed, still running the previous config: "+err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("reloaded\n"))
	})

	// Liveness: the process is up. Restart it if this fails.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	// Readiness: this instance should receive traffic right now.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		if !slices.ContainsFunc(backends(), (*backend.Backend).Alive) {
			http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})

	mux.HandleFunc("GET /backends", func(w http.ResponseWriter, _ *http.Request) {
		type status struct {
			URL      string `json:"url"`
			Alive    bool   `json:"alive"`
			Outlier  string `json:"outlier"`
			Inflight int64  `json:"inflight"`
			Weight   int    `json:"weight"`
		}
		now := time.Now()
		bs := backends()
		out := make([]status, len(bs))
		for i, b := range bs {
			out[i] = status{
				URL:      b.URL.String(),
				Alive:    b.Alive(),
				Outlier:  b.Outlier.Mode(now),
				Inflight: b.Inflight(),
				Weight:   b.Weight(),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	})

	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

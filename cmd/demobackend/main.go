// Command demobackend is a toy upstream server for trying edgeproxy locally.
// It can inject latency and errors, and its health can be toggled at runtime.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", ":9001", "listen address")
	name := flag.String("name", "", "name reported in responses (defaults to the listen address)")
	latency := flag.Duration("latency", 0, "delay added to every response, e.g. 50ms")
	failRate := flag.Float64("fail-rate", 0, "fraction of requests (0-1) answered with a 500")
	flag.Parse()
	if *name == "" {
		*name = *addr
	}

	var healthy atomic.Bool
	healthy.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	// POST /toggle-health flips the health check result, to watch edgeproxy
	// take this backend out of rotation and bring it back.
	mux.HandleFunc("POST /toggle-health", func(w http.ResponseWriter, _ *http.Request) {
		now := !healthy.Load()
		healthy.Store(now)
		fmt.Fprintf(w, "healthy=%v\n", now)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(*latency)
		if rand.Float64() < *failRate {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"backend":         *name,
			"method":          r.Method,
			"path":            r.URL.Path,
			"x_forwarded_for": r.Header.Get("X-Forwarded-For"),
			"request_id":      r.Header.Get("X-Request-ID"),
			"traceparent":     r.Header.Get("traceparent"),
		})
	})

	log.Printf("demobackend %s listening on %s", *name, *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

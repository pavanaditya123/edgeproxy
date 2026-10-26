package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"edgeproxy/internal/backend"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCheckerAppliesThresholds(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	b, err := backend.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{
		Path: "/healthz", Interval: time.Hour, Timeout: time.Second,
		HealthyThreshold: 2, UnhealthyThreshold: 3,
	}, []*backend.Backend{b}, discardLogger())
	ctx := context.Background()

	c.CheckAll(ctx)
	if !b.Alive() {
		t.Fatal("first passing probe should mark the backend up immediately")
	}

	healthy.Store(false)
	c.CheckAll(ctx)
	c.CheckAll(ctx)
	if !b.Alive() {
		t.Fatal("backend marked down before 3 consecutive failures")
	}
	c.CheckAll(ctx)
	if b.Alive() {
		t.Fatal("backend still up after 3 consecutive failures")
	}

	healthy.Store(true)
	c.CheckAll(ctx)
	if b.Alive() {
		t.Fatal("backend marked up after 1 success, want 2")
	}
	c.CheckAll(ctx)
	if !b.Alive() {
		t.Fatal("backend still down after 2 consecutive successes")
	}
}

func TestCheckerTimesOutHungBackend(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	b, err := backend.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{
		Path: "/healthz", Interval: time.Hour, Timeout: 100 * time.Millisecond,
		HealthyThreshold: 1, UnhealthyThreshold: 1,
	}, []*backend.Backend{b}, discardLogger())

	start := time.Now()
	c.CheckAll(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probe took %v, want it cut off near the 100ms timeout", elapsed)
	}
	if b.Alive() {
		t.Fatal("hung backend should be down")
	}
}

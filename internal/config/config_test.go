package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"backends": ["http://127.0.0.1:9001"], "health_check": {"interval": "10s"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Strategy != "round_robin" || cfg.MaxRetries != 2 {
		t.Errorf("defaults not applied: strategy=%q max_retries=%d", cfg.Strategy, cfg.MaxRetries)
	}
	if cfg.HealthCheck.Interval.Std() != 10*time.Second {
		t.Errorf("interval = %v, want 10s from the file", cfg.HealthCheck.Interval.Std())
	}
	if cfg.HealthCheck.Path != "/healthz" {
		t.Errorf("health_check.path = %q, want the default kept alongside the override", cfg.HealthCheck.Path)
	}
}

func TestBackendsAcceptStringsAndObjects(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"backends": ["http://a:1", {"url": "http://b:2", "weight": 3}, {"url": "http://c:3"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []BackendSpec{{"http://a:1", 1}, {"http://b:2", 3}, {"http://c:3", 1}}
	for i, w := range want {
		if cfg.Backends[i] != w {
			t.Errorf("backend %d = %+v, want %+v", i, cfg.Backends[i], w)
		}
	}
}

func TestBackendObjectRejectsUnknownFields(t *testing.T) {
	_, err := Load(writeConfig(t, `{"backends": [{"url": "http://b:2", "wieght": 3}]}`))
	if err == nil || !strings.Contains(err.Error(), "wieght") {
		t.Fatalf("err = %v, want it to name the misspelled field", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load(writeConfig(t, `{"backends": ["http://127.0.0.1:9001"], "max_retires": 3}`))
	if err == nil || !strings.Contains(err.Error(), "max_retires") {
		t.Fatalf("err = %v, want it to name the misspelled field", err)
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	_, err := Load(writeConfig(t, `{"strategy": "random", "max_retries": 50}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"strategy", "max_retries", "backend"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

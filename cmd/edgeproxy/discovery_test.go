package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"edgeproxy/internal/config"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func urls(specs []config.BackendSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.URL
	}
	return out
}

func TestResolveBackendsMergesStaticAndDiscovered(t *testing.T) {
	target := config.DNSTarget{Host: "app.svc", Port: 8080, Scheme: "http"}
	cfg := &config.Config{
		Backends:  []config.BackendSpec{{URL: "http://static:80", Weight: 2}},
		Discovery: config.Discovery{DNS: []config.DNSTarget{target}},
	}
	lookup := func(context.Context, string) ([]string, error) {
		return []string{"10.0.0.9", "10.0.0.2", "fd00::7"}, nil // unsorted, with IPv6
	}
	specs, _ := resolveBackends(context.Background(), cfg, nil, lookup, quiet)

	want := []string{"http://static:80", "http://10.0.0.2:8080", "http://10.0.0.9:8080", "http://[fd00::7]:8080"}
	got := urls(specs)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestResolveBackendsKeepsPreviousAddressesOnLookupFailure(t *testing.T) {
	target := config.DNSTarget{Host: "app.svc", Port: 8080, Scheme: "http"}
	cfg := &config.Config{Discovery: config.Discovery{DNS: []config.DNSTarget{target}}}

	ok := func(context.Context, string) ([]string, error) { return []string{"10.0.0.1"}, nil }
	_, prev := resolveBackends(context.Background(), cfg, nil, ok, quiet)

	broken := func(context.Context, string) ([]string, error) { return nil, errors.New("SERVFAIL") }
	specs, next := resolveBackends(context.Background(), cfg, prev, broken, quiet)
	if got := urls(specs); len(got) != 1 || got[0] != "http://10.0.0.1:8080" {
		t.Fatalf("after a failed lookup got %v, want the previous address kept", got)
	}
	if len(next[target]) != 1 {
		t.Fatal("the kept addresses should carry over to the next round")
	}
}

func TestBackendSetKeyChangesWithMembership(t *testing.T) {
	a := []config.BackendSpec{{URL: "http://a", Weight: 1}, {URL: "http://b", Weight: 1}}
	b := []config.BackendSpec{{URL: "http://a", Weight: 1}}
	if backendSetKey(a) == backendSetKey(b) {
		t.Fatal("different backend sets must have different keys")
	}
}

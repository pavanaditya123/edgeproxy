package main

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"edgeproxy/internal/config"
)

// lookupFunc resolves a host name to IP addresses; net.Resolver.LookupHost in production.
type lookupFunc func(ctx context.Context, host string) ([]string, error)

// discovered maps each DNS target to the backends it last resolved to.
type discovered map[config.DNSTarget][]config.BackendSpec

// resolveBackends returns cfg's static backends plus one backend per address
// each discovery name resolves to. If a lookup fails, that name keeps the
// addresses it had in prev rather than dropping out of rotation over a DNS blip.
func resolveBackends(ctx context.Context, cfg *config.Config, prev discovered, lookup lookupFunc, logger *slog.Logger) ([]config.BackendSpec, discovered) {
	specs := slices.Clone(cfg.Backends)
	next := make(discovered, len(cfg.Discovery.DNS))
	for _, target := range cfg.Discovery.DNS {
		lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		addrs, err := lookup(lookupCtx, target.Host)
		cancel()
		if err != nil {
			logger.Warn("DNS discovery lookup failed; keeping the previous addresses",
				"host", target.Host, "kept", len(prev[target]), "error", err)
			next[target] = prev[target]
		} else {
			// DNS servers rotate the order of answers; sort so the backend list
			// (and the consistent-hash ring built from it) stays stable.
			slices.Sort(addrs)
			found := make([]config.BackendSpec, 0, len(addrs))
			for _, addr := range addrs {
				found = append(found, config.BackendSpec{
					URL:    target.Scheme + "://" + net.JoinHostPort(addr, strconv.Itoa(target.Port)),
					Weight: 1,
				})
			}
			next[target] = found
		}
		for _, s := range next[target] {
			if !slices.ContainsFunc(specs, func(o config.BackendSpec) bool { return o.URL == s.URL }) {
				specs = append(specs, s)
			}
		}
	}
	return specs, next
}

// backendSetKey identifies a backend list, to tell whether discovery changed it.
func backendSetKey(specs []config.BackendSpec) string {
	parts := make([]string, len(specs))
	for i, s := range specs {
		parts[i] = s.URL + "*" + strconv.Itoa(s.Weight)
	}
	return strings.Join(parts, ",")
}

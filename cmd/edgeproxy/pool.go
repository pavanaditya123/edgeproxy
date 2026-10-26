package main

import (
	"edgeproxy/internal/backend"
	"edgeproxy/internal/config"
	"edgeproxy/internal/outlier"
)

// buildBackends creates the backends listed in cfg, reusing any in prev with
// the same URL so their health and outlier state carry over.
func buildBackends(cfg *config.Config, prev []*backend.Backend) ([]*backend.Backend, error) {
	existing := make(map[string]*backend.Backend, len(prev))
	for _, b := range prev {
		existing[b.URL.String()] = b
	}
	backends := make([]*backend.Backend, 0, len(cfg.Backends))
	for _, raw := range cfg.Backends {
		b, err := backend.New(raw)
		if err != nil {
			return nil, err
		}
		if old, ok := existing[b.URL.String()]; ok {
			b = old
		}
		b.SetMaxInflight(cfg.MaxInflightPerBackend)
		backends = append(backends, b)
	}
	return backends, nil
}

func newDetector(cfg *config.Config, backends []*backend.Backend) *outlier.Detector {
	states := make([]*outlier.State, len(backends))
	for i, b := range backends {
		states[i] = &b.Outlier
	}
	od := cfg.OutlierDetection
	return outlier.NewDetector(outlier.Policy{
		ConsecutiveFailures: od.ConsecutiveFailures,
		BaseEjection:        od.BaseEjectionTime.Std(),
		MaxEjection:         od.MaxEjectionTime.Std(),
		MaxEjectionPercent:  od.MaxEjectionPercent,
	}, states)
}

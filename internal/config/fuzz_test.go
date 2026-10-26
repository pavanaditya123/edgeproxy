package config

import (
	"os"
	"testing"
)

// FuzzParse feeds Parse random mutations of real configs. Whatever the input,
// Parse must not panic, and anything it accepts must pass validation again.
//
//	go test -fuzz=FuzzParse -fuzztime=30s ./internal/config/
func FuzzParse(f *testing.F) {
	if local, err := os.ReadFile("../../configs/local.json"); err == nil {
		f.Add(local)
	}
	for _, seed := range []string{
		`{}`,
		`{"backends": ["http://127.0.0.1:9001"]}`,
		`{"backends": [{"url": "http://a:1", "weight": 3}], "strategy": "weighted_round_robin"}`,
		`{"backends": ["http://a:1"], "health_check": {"interval": "1s", "timeout": "2s"}}`,
		`{"backends": ["http://a:1"], "outlier_detection": {"consecutive_failures": 3, "base_ejection_time": "1h", "max_ejection_time": "1s"}}`,
		`{"backends": ["http://a:1"]} {"extra": true}`,
		`[1, 2, 3]`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := Parse(data)
		if err != nil {
			return
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Parse accepted a config that fails validation: %v", err)
		}
		if len(cfg.Backends) == 0 {
			t.Fatal("Parse accepted a config with no backends")
		}
	})
}

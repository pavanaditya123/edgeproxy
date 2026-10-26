// Package config loads and validates edgeproxy's JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

type Config struct {
	Listen      string      `json:"listen"`
	AdminListen string      `json:"admin_listen"`
	Strategy    string      `json:"strategy"`
	HashKey     string      `json:"hash_key"`
	MaxRetries  int         `json:"max_retries"`
	Backends    []string    `json:"backends"`
	HealthCheck HealthCheck `json:"health_check"`
	Timeouts    Timeouts    `json:"timeouts"`
}

type HealthCheck struct {
	Path               string   `json:"path"`
	Interval           Duration `json:"interval"`
	Timeout            Duration `json:"timeout"`
	HealthyThreshold   int      `json:"healthy_threshold"`
	UnhealthyThreshold int      `json:"unhealthy_threshold"`
}

type Timeouts struct {
	// Dial bounds the TCP connect to a backend.
	Dial Duration `json:"dial"`
	// ReadHeader bounds how long a client may take to send request headers (slowloris protection).
	ReadHeader Duration `json:"read_header"`
	// ResponseHeader bounds how long a backend may take to start responding.
	ResponseHeader Duration `json:"response_header"`
	// IdleConn is how long an unused keep-alive connection to a backend is kept in the pool.
	IdleConn Duration `json:"idle_conn"`
	// DrainDelay is how long to keep serving after SIGTERM while /readyz reports 503,
	// giving an upstream load balancer time to stop sending new traffic.
	DrainDelay Duration `json:"drain_delay"`
	// ShutdownGrace bounds how long in-flight requests get to finish during shutdown.
	ShutdownGrace Duration `json:"shutdown_grace"`
}

// Duration is a time.Duration written in JSON as a string such as "5s" or "250ms".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`duration must be a string such as "5s": %w`, err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Default returns the configuration that fields missing from the file fall back to.
func Default() *Config {
	return &Config{
		Listen:      ":8080",
		AdminListen: ":9090",
		Strategy:    "round_robin",
		HashKey:     "client_ip",
		MaxRetries:  2,
		HealthCheck: HealthCheck{
			Path:               "/healthz",
			Interval:           Duration(5 * time.Second),
			Timeout:            Duration(time.Second),
			HealthyThreshold:   2,
			UnhealthyThreshold: 3,
		},
		Timeouts: Timeouts{
			Dial:           Duration(2 * time.Second),
			ReadHeader:     Duration(5 * time.Second),
			ResponseHeader: Duration(10 * time.Second),
			IdleConn:       Duration(90 * time.Second),
			DrainDelay:     0,
			ShutdownGrace:  Duration(15 * time.Second),
		},
	}
}

// Load reads the config file at path on top of Default and validates it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	// Reject unknown fields so a typo like "max_retires" fails loudly instead of being ignored.
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports every problem with the config at once rather than stopping at the first.
func (c *Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	if c.AdminListen == "" {
		errs = append(errs, errors.New("admin_listen is required"))
	}
	if c.Listen == c.AdminListen {
		errs = append(errs, errors.New("listen and admin_listen must differ"))
	}
	switch c.Strategy {
	case "round_robin", "least_conn", "consistent_hash":
	default:
		errs = append(errs, fmt.Errorf("strategy %q must be round_robin, least_conn or consistent_hash", c.Strategy))
	}
	if c.MaxRetries < 0 || c.MaxRetries > 10 {
		errs = append(errs, fmt.Errorf("max_retries %d must be between 0 and 10", c.MaxRetries))
	}
	if len(c.Backends) == 0 {
		errs = append(errs, errors.New("at least one backend is required"))
	}
	seen := make(map[string]bool, len(c.Backends))
	for _, b := range c.Backends {
		if seen[b] {
			errs = append(errs, fmt.Errorf("backend %q is listed twice", b))
		}
		seen[b] = true
	}

	hc := c.HealthCheck
	if hc.Interval <= 0 || hc.Timeout <= 0 {
		errs = append(errs, errors.New("health_check interval and timeout must be positive"))
	}
	if hc.Timeout > hc.Interval {
		errs = append(errs, errors.New("health_check timeout must not exceed interval"))
	}
	if hc.HealthyThreshold < 1 || hc.UnhealthyThreshold < 1 {
		errs = append(errs, errors.New("health_check thresholds must be at least 1"))
	}

	t := c.Timeouts
	if t.Dial <= 0 || t.ReadHeader <= 0 || t.ResponseHeader <= 0 || t.IdleConn <= 0 || t.ShutdownGrace <= 0 {
		errs = append(errs, errors.New("timeouts dial, read_header, response_header, idle_conn and shutdown_grace must be positive"))
	}
	if t.DrainDelay < 0 {
		errs = append(errs, errors.New("timeouts.drain_delay must not be negative"))
	}
	return errors.Join(errs...)
}

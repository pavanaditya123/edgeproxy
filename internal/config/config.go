// Package config loads and validates edgeproxy's JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"edgeproxy/internal/reqkey"
)

type Config struct {
	Listen      string        `json:"listen"`
	AdminListen string        `json:"admin_listen"`
	Strategy    string        `json:"strategy"`
	HashKey     string        `json:"hash_key"`
	MaxRetries  int           `json:"max_retries"`
	Backends    []BackendSpec `json:"backends"`
	HealthCheck HealthCheck   `json:"health_check"`
	Timeouts    Timeouts      `json:"timeouts"`

	// MaxInflightPerBackend caps concurrent requests to any one backend; 0 means no cap.
	MaxInflightPerBackend int              `json:"max_inflight_per_backend"`
	OutlierDetection      OutlierDetection `json:"outlier_detection"`
	RetryBudget           RetryBudget      `json:"retry_budget"`

	// ReloadInterval is how often the config file is checked for changes; 0 disables
	// file watching (SIGHUP and POST /-/reload still work).
	ReloadInterval Duration `json:"reload_interval"`

	RateLimit RateLimit `json:"rate_limit"`

	// AccessLog writes one JSON log line per request.
	AccessLog bool    `json:"access_log"`
	Tracing   Tracing `json:"tracing"`

	Discovery Discovery `json:"discovery"`
}

// Discovery adds backends found through DNS to the static list. Each name is
// re-resolved every Interval, and every address it returns becomes a backend;
// a Kubernetes headless Service resolves to all of its ready pods this way.
type Discovery struct {
	DNS      []DNSTarget `json:"dns"`
	Interval Duration    `json:"interval"`
}

type DNSTarget struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Scheme is "http" (the default) or "https".
	Scheme string `json:"scheme"`
}

// Tracing exports OpenTelemetry traces over OTLP/HTTP.
type Tracing struct {
	Enabled bool `json:"enabled"`
	// Endpoint is the collector's host:port, e.g. "localhost:4318".
	Endpoint    string  `json:"endpoint"`
	Insecure    bool    `json:"insecure"`
	SampleRatio float64 `json:"sample_ratio"`
	ServiceName string  `json:"service_name"`
}

// RateLimit configures request rate limiting. With no rules nothing is limited.
type RateLimit struct {
	// Redis, if addr is set, shares limits across every proxy instance.
	Redis Redis `json:"redis"`
	// OnRedisError is "local" (enforce per instance from memory), "allow" or "deny".
	OnRedisError string `json:"on_redis_error"`
	// MaxKeys bounds how many distinct clients the in-memory limiter tracks.
	MaxKeys int `json:"max_keys"`
	// Rules are checked in order; the first that matches a request applies.
	Rules []RateLimitRule `json:"rules"`
}

type Redis struct {
	Addr string `json:"addr"`
	// PasswordEnv names the environment variable holding the password, so it
	// never has to be written into the config file.
	PasswordEnv string   `json:"password_env"`
	DB          int      `json:"db"`
	Timeout     Duration `json:"timeout"`
	KeyPrefix   string   `json:"key_prefix"`
}

type RateLimitRule struct {
	Name       string `json:"name"`
	PathPrefix string `json:"path_prefix"`
	// Methods restricts the rule to these HTTP methods; empty means all.
	Methods []string `json:"methods"`
	Limit   int      `json:"limit"`
	Window  Duration `json:"window"`
	// Key is who the limit applies to: "client_ip" or "header:<Name>".
	Key string `json:"key"`
}

// RetryBudget caps retries over the last 10 seconds at Ratio of requests, or
// MinRetriesPerSecond per second if that's larger.
type RetryBudget struct {
	Ratio               float64 `json:"ratio"`
	MinRetriesPerSecond int     `json:"min_retries_per_second"`
}

// BackendSpec is one entry in "backends": either a URL string, or an object
// such as {"url": "http://10.0.0.1:8080", "weight": 3}.
type BackendSpec struct {
	URL    string `json:"url"`
	Weight int    `json:"weight,omitempty"`
}

func (b *BackendSpec) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*b = BackendSpec{URL: s, Weight: 1}
		return nil
	}
	type plain BackendSpec // no UnmarshalJSON method, so no recursion
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf(`backend must be a URL string or {"url": ..., "weight": ...}: %w`, err)
	}
	if p.Weight == 0 {
		p.Weight = 1
	}
	*b = BackendSpec(p)
	return nil
}

// OutlierDetection configures passive health checking (see package outlier).
type OutlierDetection struct {
	// ConsecutiveFailures ejects a backend after this many failed requests in a row; 0 disables it.
	ConsecutiveFailures int      `json:"consecutive_failures"`
	BaseEjectionTime    Duration `json:"base_ejection_time"`
	MaxEjectionTime     Duration `json:"max_ejection_time"`
	MaxEjectionPercent  int      `json:"max_ejection_percent"`
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

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

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
		OutlierDetection: OutlierDetection{
			ConsecutiveFailures: 5,
			BaseEjectionTime:    Duration(10 * time.Second),
			MaxEjectionTime:     Duration(5 * time.Minute),
			MaxEjectionPercent:  50,
		},
		RetryBudget:    RetryBudget{Ratio: 0.2, MinRetriesPerSecond: 10},
		ReloadInterval: Duration(2 * time.Second),
		RateLimit: RateLimit{
			Redis:        Redis{Timeout: Duration(50 * time.Millisecond), KeyPrefix: "edgeproxy:rl:"},
			OnRedisError: "local",
			MaxKeys:      100_000,
		},
		AccessLog: true,
		Tracing: Tracing{
			Endpoint:    "localhost:4318",
			Insecure:    true,
			SampleRatio: 1,
			ServiceName: "edgeproxy",
		},
		Discovery: Discovery{Interval: Duration(10 * time.Second)},
	}
}

// Load reads the config file at path on top of Default and validates it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes JSON config on top of Default and validates it.
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	// Reject unknown fields so a typo like "max_retires" fails loudly instead of being ignored.
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse: unexpected data after the config object")
	}
	for i := range cfg.Discovery.DNS {
		if cfg.Discovery.DNS[i].Scheme == "" {
			cfg.Discovery.DNS[i].Scheme = "http"
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
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
	case "round_robin", "weighted_round_robin", "least_conn", "p2c", "consistent_hash":
	default:
		errs = append(errs, fmt.Errorf("strategy %q must be round_robin, weighted_round_robin, least_conn, p2c or consistent_hash", c.Strategy))
	}
	if c.MaxRetries < 0 || c.MaxRetries > 10 {
		errs = append(errs, fmt.Errorf("max_retries %d must be between 0 and 10", c.MaxRetries))
	}
	if len(c.Backends) == 0 && len(c.Discovery.DNS) == 0 {
		errs = append(errs, errors.New("at least one backend (or a discovery.dns entry) is required"))
	}
	for i, d := range c.Discovery.DNS {
		if d.Host == "" || d.Port < 1 || d.Port > 65535 || (d.Scheme != "http" && d.Scheme != "https") {
			errs = append(errs, fmt.Errorf("discovery.dns[%d] needs a host, a port between 1 and 65535, and scheme http or https", i))
		}
	}
	if len(c.Discovery.DNS) > 0 && c.Discovery.Interval < Duration(time.Second) {
		errs = append(errs, errors.New("discovery.interval must be at least 1s"))
	}
	seen := make(map[string]bool, len(c.Backends))
	for _, b := range c.Backends {
		if b.URL == "" {
			errs = append(errs, errors.New("every backend needs a url"))
		}
		if seen[b.URL] {
			errs = append(errs, fmt.Errorf("backend %q is listed twice", b.URL))
		}
		seen[b.URL] = true
		if b.Weight < 1 || b.Weight > 1000 {
			errs = append(errs, fmt.Errorf("backend %q: weight %d must be between 1 and 1000", b.URL, b.Weight))
		}
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

	if c.MaxInflightPerBackend < 0 {
		errs = append(errs, errors.New("max_inflight_per_backend must not be negative"))
	}
	od := c.OutlierDetection
	switch {
	case od.ConsecutiveFailures < 0:
		errs = append(errs, errors.New("outlier_detection.consecutive_failures must not be negative"))
	case od.ConsecutiveFailures > 0:
		if od.BaseEjectionTime <= 0 || od.MaxEjectionTime < od.BaseEjectionTime {
			errs = append(errs, errors.New("outlier_detection needs base_ejection_time > 0 and max_ejection_time >= base_ejection_time"))
		}
		if od.MaxEjectionPercent < 1 || od.MaxEjectionPercent > 100 {
			errs = append(errs, errors.New("outlier_detection.max_ejection_percent must be between 1 and 100"))
		}
	}
	if rb := c.RetryBudget; rb.Ratio < 0 || rb.Ratio > 1 || rb.MinRetriesPerSecond < 0 {
		errs = append(errs, errors.New("retry_budget needs 0 <= ratio <= 1 and min_retries_per_second >= 0"))
	}
	if c.ReloadInterval < 0 {
		errs = append(errs, errors.New("reload_interval must not be negative"))
	}
	errs = append(errs, c.RateLimit.validate()...)
	if tr := c.Tracing; tr.Enabled {
		if tr.Endpoint == "" || tr.ServiceName == "" {
			errs = append(errs, errors.New("tracing needs an endpoint and a service_name"))
		}
		if tr.SampleRatio < 0 || tr.SampleRatio > 1 {
			errs = append(errs, errors.New("tracing.sample_ratio must be between 0 and 1"))
		}
	}
	return errors.Join(errs...)
}

func (rl *RateLimit) validate() []error {
	var errs []error
	switch rl.OnRedisError {
	case "local", "allow", "deny":
	default:
		errs = append(errs, fmt.Errorf("rate_limit.on_redis_error %q must be local, allow or deny", rl.OnRedisError))
	}
	if rl.MaxKeys < 1 {
		errs = append(errs, errors.New("rate_limit.max_keys must be at least 1"))
	}
	if rl.Redis.Addr != "" && rl.Redis.Timeout <= 0 {
		errs = append(errs, errors.New("rate_limit.redis.timeout must be positive"))
	}
	names := make(map[string]bool, len(rl.Rules))
	for i, r := range rl.Rules {
		where := fmt.Sprintf("rate_limit.rules[%d]", i)
		if r.Name == "" || names[r.Name] {
			errs = append(errs, fmt.Errorf("%s: name must be set and unique", where))
		}
		names[r.Name] = true
		if !strings.HasPrefix(r.PathPrefix, "/") {
			errs = append(errs, fmt.Errorf("%s: path_prefix must start with /", where))
		}
		if r.Limit < 1 {
			errs = append(errs, fmt.Errorf("%s: limit must be at least 1", where))
		}
		if r.Window < Duration(100*time.Millisecond) {
			errs = append(errs, fmt.Errorf("%s: window must be at least 100ms", where))
		}
		if _, err := reqkey.Parse(r.Key); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		for _, m := range r.Methods {
			if m == "" || strings.ToUpper(m) != m {
				errs = append(errs, fmt.Errorf("%s: method %q must be upper case, like GET", where, m))
			}
		}
	}
	return errs
}

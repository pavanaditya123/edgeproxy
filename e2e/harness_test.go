//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Second}

// process is a child process whose output is captured and printed if the test fails.
type process struct {
	label string
	cmd   *exec.Cmd
	out   *syncBuffer
	done  chan struct{}
	err   error // exit status; only valid once done is closed
}

func start(t *testing.T, label, bin string, args ...string) *process {
	t.Helper()
	p := &process{label: label, out: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command(bin, args...)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", label, err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.kill()
		if strings.Contains(p.out.String(), "DATA RACE") {
			t.Errorf("%s reported a data race", label)
		}
		if t.Failed() {
			t.Logf("--- output of %s ---\n%s", label, p.out)
		}
	})
	return p
}

func (p *process) kill() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	<-p.done
}

type demoBackend struct {
	*process
	name string
	url  string
}

// startBackend runs a demobackend named name; extra flags such as
// "-latency", "300ms" are passed through.
func startBackend(t *testing.T, name string, extra ...string) *demoBackend {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	args := append([]string{"-addr", addr, "-name", name}, extra...)
	b := &demoBackend{
		process: start(t, "backend "+name, binPath("demobackend"), args...),
		name:    name,
		url:     "http://" + addr,
	}
	waitFor(t, 10*time.Second, "backend "+name+" to listen", func() bool {
		code, _ := get(b.url + "/healthz")
		return code == http.StatusOK
	})
	return b
}

// toggleHealth flips the backend's /healthz between passing and failing.
func (b *demoBackend) toggleHealth(t *testing.T) {
	t.Helper()
	resp, err := client.Post(b.url+"/toggle-health", "text/plain", nil)
	if err != nil {
		t.Fatalf("toggle health of %s: %v", b.name, err)
	}
	resp.Body.Close()
}

type proxyOpts struct {
	strategy       string
	hashKey        string
	healthInterval time.Duration
	drainDelay     time.Duration
}

type edgeProxy struct {
	*process
	url      string
	adminURL string
}

// startProxy runs edgeproxy in front of backends and waits until it is ready.
func startProxy(t *testing.T, opts proxyOpts, backends ...*demoBackend) *edgeProxy {
	t.Helper()
	if opts.strategy == "" {
		opts.strategy = "round_robin"
	}
	if opts.healthInterval == 0 {
		opts.healthInterval = 200 * time.Millisecond
	}
	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	admin := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	urls := make([]string, len(backends))
	for i, b := range backends {
		urls[i] = b.url
	}
	cfg := map[string]any{
		"listen":       listen,
		"admin_listen": admin,
		"strategy":     opts.strategy,
		"max_retries":  2,
		"backends":     urls,
		"health_check": map[string]any{
			"path":                "/healthz",
			"interval":            opts.healthInterval.String(),
			"timeout":             "100ms",
			"healthy_threshold":   2,
			"unhealthy_threshold": 2,
		},
		"timeouts": map[string]any{
			"drain_delay":    opts.drainDelay.String(),
			"shutdown_grace": "5s",
		},
	}
	if opts.hashKey != "" {
		cfg["hash_key"] = opts.hashKey
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	ep := &edgeProxy{
		process:  start(t, "edgeproxy", binPath("edgeproxy"), "-config", path),
		url:      "http://" + listen,
		adminURL: "http://" + admin,
	}
	waitFor(t, 10*time.Second, "edgeproxy to become ready", func() bool {
		code, _ := get(ep.adminURL + "/readyz")
		return code == http.StatusOK
	})
	return ep
}

// echo is the JSON body demobackend answers with.
type echo struct {
	Backend       string `json:"backend"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	XForwardedFor string `json:"x_forwarded_for"`
}

// fetch sends a request through the proxy. The echo is only decoded for a 200.
// It doesn't take a *testing.T so it can be called from worker goroutines.
func (ep *edgeProxy) fetch(method, path string, headers map[string]string, body string) (int, echo, error) {
	var e echo
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ep.url+path, rdr)
	if err != nil {
		return 0, e, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, e, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, e, err
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &e); err != nil {
			return resp.StatusCode, e, fmt.Errorf("decode %q: %w", raw, err)
		}
	}
	return resp.StatusCode, e, nil
}

// whoAnswers sends a GET and returns the name of the backend that served it.
func (ep *edgeProxy) whoAnswers(t *testing.T, headers map[string]string) string {
	t.Helper()
	code, e, err := ep.fetch(http.MethodGet, "/", headers, "")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("GET through proxy: status %d, want 200", code)
	}
	return e.Backend
}

// backendAlive reports what the proxy's /backends endpoint says about b.
func (ep *edgeProxy) backendAlive(t *testing.T, b *demoBackend) bool {
	t.Helper()
	code, body := get(ep.adminURL + "/backends")
	if code != http.StatusOK {
		t.Fatalf("GET /backends: status %d", code)
	}
	var list []struct {
		URL   string `json:"url"`
		Alive bool   `json:"alive"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode /backends: %v", err)
	}
	for _, s := range list {
		if s.URL == b.url {
			return s.Alive
		}
	}
	t.Fatalf("/backends does not list %s", b.url)
	return false
}

// metric sums every series of a Prometheus metric, e.g. all
// edgeproxy_requests_total{backend=...,code=...} lines.
func (ep *edgeProxy) metric(t *testing.T, name string) float64 {
	t.Helper()
	code, body := get(ep.adminURL + "/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", code)
	}
	var sum float64
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+" ") && !strings.HasPrefix(line, name+"{") {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse metric line %q: %v", line, err)
		}
		sum += v
	}
	return sum
}

// get returns the status and body of a GET, or status 0 if the request failed.
func get(url string) (int, string) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// syncBuffer is a bytes.Buffer safe for a child's stdout and stderr to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

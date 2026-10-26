//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestIDTiesClientBackendAndLogTogether(t *testing.T) {
	a := startBackend(t, "a")
	ep := startProxy(t, proxyOpts{}, a)

	// No ID sent: the proxy makes one and shows it to both sides.
	req, _ := http.NewRequest(http.MethodGet, ep.url+"/hello", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var e echo
	json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	id := resp.Header.Get("X-Request-ID")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) || e.RequestID != id {
		t.Fatalf("client got ID %q, backend got %q; want the same generated 32-hex ID", id, e.RequestID)
	}

	// The access log has a line for it with the outcome.
	waitFor(t, 5*time.Second, "an access-log line for "+id, func() bool {
		line := findLogLine(ep, id)
		return line != nil && line["status"] == float64(200) && line["path"] == "/hello" && line["backend"] != ""
	})

	// A caller's own ID is kept end to end.
	_, e2, err := ep.fetch(http.MethodGet, "/", map[string]string{"X-Request-ID": "checkout-42"}, "")
	if err != nil || e2.RequestID != "checkout-42" {
		t.Errorf("backend saw request ID %q (%v), want the caller's checkout-42", e2.RequestID, err)
	}
}

// findLogLine returns the proxy's access-log entry for a request ID, if any.
func findLogLine(ep *edgeProxy, requestID string) map[string]any {
	sc := bufio.NewScanner(strings.NewReader(ep.out.String()))
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) == nil && line["msg"] == "request" && line["request_id"] == requestID {
			return line
		}
	}
	return nil
}

func TestTracingContinuesTheTraceAndExportsSpans(t *testing.T) {
	collector, exports := fakeCollector(t)
	a := startBackend(t, "a")
	ep := startProxy(t, proxyOpts{extra: map[string]any{
		"tracing": map[string]any{"enabled": true, "endpoint": collector, "insecure": true},
	}}, a)

	const traceID, clientSpan = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	_, e, err := ep.fetch(http.MethodGet, "/", map[string]string{
		"traceparent": "00-" + traceID + "-" + clientSpan + "-01",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(e.Traceparent, "-")
	if len(parts) != 4 || parts[1] != traceID || parts[2] == clientSpan {
		t.Fatalf("backend got traceparent %q; want trace %s with the proxy's own span as parent", e.Traceparent, traceID)
	}
	waitFor(t, 10*time.Second, "spans to reach the collector", func() bool { return exports.Load() > 0 })

	line := findLogLineByTrace(ep, traceID)
	if line == nil {
		t.Error("the access log should record the trace ID so logs and traces can be joined")
	}
}

func findLogLineByTrace(ep *edgeProxy, traceID string) map[string]any {
	sc := bufio.NewScanner(strings.NewReader(ep.out.String()))
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) == nil && line["msg"] == "request" && line["trace_id"] == traceID {
			return line
		}
	}
	return nil
}

// fakeCollector accepts OTLP/HTTP trace exports and counts them. It returns
// the host:port to configure as the tracing endpoint.
func fakeCollector(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	var exports atomic.Int64
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/traces" {
			http.NotFound(w, r)
			return
		}
		exports.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK) // an empty body is a valid, empty export response
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprintf("127.0.0.1:%d", l.Addr().(*net.TCPAddr).Port), &exports
}

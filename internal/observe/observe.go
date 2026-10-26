// Package observe makes each request traceable: it gives every request an ID,
// writes one structured access-log line per request, and records
// OpenTelemetry traces that follow a request through the proxy to a backend.
package observe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// Info is filled in as a request moves through the proxy, for the access log
// and trace to report. Only the request's own goroutine touches it.
type Info struct {
	RequestID string
	// Backend is the backend that served the request (the last one tried).
	Backend string
	// Attempts is how many backends were tried.
	Attempts int
}

type infoKey struct{}

// InfoFrom returns the request's Info. Outside the middleware (in tests, say)
// it returns a throwaway Info, so callers never need a nil check.
func InfoFrom(ctx context.Context) *Info {
	if info, ok := ctx.Value(infoKey{}).(*Info); ok {
		return info
	}
	return &Info{}
}

// RequestIDHeader carries the request ID to the backend and back to the client.
const RequestIDHeader = "X-Request-ID"

// RequestID makes sure every request has an ID that appears in the access log,
// the trace, the backend's view of the request and the response, so one ID
// ties together everything that happened to a request. A client-supplied ID
// is kept if it's well-formed, so IDs can span several services.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
			r = r.Clone(r.Context())
			r.Header.Set(RequestIDHeader, id)
		}
		w.Header().Set(RequestIDHeader, id)
		info := &Info{RequestID: id}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
	})
}

// validRequestID accepts short IDs of safe characters only, so a client can't
// inject fake log lines or huge values through the header.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == ':'
		if !ok {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// recorder captures the status and size of the response.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (rec *recorder) WriteHeader(code int) {
	if !rec.wroteHeader && (code >= 200 || code == http.StatusSwitchingProtocols) {
		rec.status = code
		rec.wroteHeader = true
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(b []byte) (int, error) {
	rec.wroteHeader = true
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer for
// flushing and WebSocket hijacking.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

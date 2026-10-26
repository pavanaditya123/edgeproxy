// Package reqkey extracts the value that identifies who sent a request. It's
// used to pin users to backends (consistent hashing) and to count requests
// per user (rate limiting).
package reqkey

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Func extracts a request's key.
type Func func(*http.Request) string

// Parse builds the extractor for a key spec: "client_ip" uses the TCP peer
// address; "header:<Name>" uses that request header, falling back to the
// client IP when the header is absent.
func Parse(spec string) (Func, error) {
	switch {
	case spec == "" || spec == "client_ip":
		return ClientIP, nil
	case strings.HasPrefix(spec, "header:"):
		name := strings.TrimSpace(strings.TrimPrefix(spec, "header:"))
		if name == "" {
			return nil, errors.New(`key "header:" needs a header name`)
		}
		return func(r *http.Request) string {
			if v := r.Header.Get(name); v != "" {
				return v
			}
			return ClientIP(r)
		}, nil
	}
	return nil, fmt.Errorf(`unknown key %q (want "client_ip" or "header:<Name>")`, spec)
}

// ClientIP returns the TCP peer's IP. It deliberately ignores X-Forwarded-For,
// which any client can forge.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

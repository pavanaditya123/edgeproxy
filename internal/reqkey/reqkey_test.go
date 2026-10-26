package reqkey

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPIgnoresForwardedFor(t *testing.T) {
	keyFn, err := Parse("client_ip")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:51234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := keyFn(r); got != "203.0.113.7" {
		t.Errorf("key = %q, want the TCP peer IP 203.0.113.7", got)
	}
}

func TestHeaderKeyFallsBackToClientIP(t *testing.T) {
	keyFn, err := Parse("header:X-API-Key")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:51234"
	if got := keyFn(r); got != "203.0.113.7" {
		t.Errorf("without the header: key = %q, want the client IP", got)
	}
	r.Header.Set("X-API-Key", "team-blue")
	if got := keyFn(r); got != "team-blue" {
		t.Errorf("with the header: key = %q, want team-blue", got)
	}
}

func TestParseRejectsBadSpecs(t *testing.T) {
	for _, spec := range []string{"header:", "header:  ", "cookie:session", "ip"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", spec)
		}
	}
}

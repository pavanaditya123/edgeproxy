//go:build e2e

package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"edgeproxy/internal/tlsutil"
)

func writeTestCert(t *testing.T, dir, commonName string) *x509.CertPool {
	t.Helper()
	certPEM, keyPEM, err := tlsutil.SelfSigned(commonName)
	if err != nil {
		t.Fatal(err)
	}
	// Write the key first and the cert last, as a cert manager would.
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return pool
}

func TestTLSTerminationHTTP2RedirectAndCertRotation(t *testing.T) {
	dir := t.TempDir()
	pool := writeTestCert(t, dir, "first")
	a := startBackend(t, "a")
	redirect := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ep := startProxy(t, proxyOpts{extra: map[string]any{
		"tls": map[string]any{
			"cert_file":       filepath.Join(dir, "tls.crt"),
			"key_file":        filepath.Join(dir, "tls.key"),
			"redirect_listen": redirect,
		},
	}}, a)
	httpsURL := strings.Replace(ep.url, "http://", "https://", 1)

	// HTTPS with HTTP/2.
	h2 := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true,
	}}
	resp, err := h2.Get(httpsURL + "/secure")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
		t.Fatalf("HTTPS request: %d over %s, want 200 over HTTP/2", resp.StatusCode, resp.Proto)
	}

	// Plain HTTP on the redirect listener points at the HTTPS listener.
	noFollow := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = noFollow.Post("http://"+redirect+"/login?next=/home", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, httpsPort, _ := strings.Cut(strings.TrimPrefix(httpsURL, "https://"), ":")
	want := "https://127.0.0.1:" + httpsPort + "/login?next=/home"
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != want {
		t.Errorf("redirect: %d %q, want 308 %q", resp.StatusCode, resp.Header.Get("Location"), want)
	}

	// Rotate the certificate on disk; new connections get it without a restart.
	writeTestCert(t, dir, "second")
	waitFor(t, 5*time.Second, "the rotated certificate to be served", func() bool {
		conn, err := tls.Dial("tcp", strings.TrimPrefix(httpsURL, "https://"), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return false
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].Subject.CommonName == "second"
	})
}

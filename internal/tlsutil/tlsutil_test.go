package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeCert(t *testing.T, dir, name string) (certFile, keyFile string) {
	t.Helper()
	certPEM, keyPEM, err := SelfSigned(name)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func servedName(t *testing.T, r *CertReloader) string {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

func TestCertReloaderRotatesWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, "first")
	r, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if changed, _ := r.Reload(); changed {
		t.Error("reloading unchanged files should report no change")
	}

	writeCert(t, dir, "second")
	if changed, err := r.Reload(); err != nil || !changed {
		t.Fatalf("Reload after rotation: changed=%v err=%v", changed, err)
	}
	if got := servedName(t, r); got != "second" {
		t.Errorf("serving %q, want the rotated certificate", got)
	}
}

func TestCertReloaderKeepsServingWhenNewFilesAreBroken(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, "good")
	r, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reload(); err == nil {
		t.Fatal("a broken key should be reported")
	}
	if got := servedName(t, r); got != "good" {
		t.Errorf("serving %q, want the last good certificate", got)
	}
}

func TestServerNegotiatesHTTP2OverTLS(t *testing.T) {
	certFile, keyFile := writeCert(t, t.TempDir(), "localhost")
	r, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	// A plain http.Server, served the way main does (httptest.StartTLS would
	// substitute its own certificate).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{TLSConfig: ServerConfig(r, "1.2"), Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.Proto))
	})}
	go srv.ServeTLS(l, "", "")
	defer srv.Close()
	srvURL := "https://" + l.Addr().String()

	pemBytes, _ := os.ReadFile(certFile)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: true,
	}}
	resp, err := client.Get(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Errorf("negotiated %s, want HTTP/2", resp.Proto)
	}

	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, MaxVersion: tls.VersionTLS11,
	}}}
	if _, err := old.Get(srvURL); err == nil {
		t.Error("TLS 1.1 should be refused")
	}
}

func TestRedirectToHTTPS(t *testing.T) {
	for _, tc := range []struct{ host, port, want string }{
		{"example.com", "443", "https://example.com/a?b=1"},
		{"example.com:8080", "8443", "https://example.com:8443/a?b=1"},
		{"[::1]:80", "443", "https://[::1]/a?b=1"},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/a?b=1", nil)
		req.Host = tc.host
		rec := httptest.NewRecorder()
		RedirectToHTTPS(tc.port).ServeHTTP(rec, req)
		if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != tc.want {
			t.Errorf("host %s: %d %s, want 308 %s", tc.host, rec.Code, rec.Header().Get("Location"), tc.want)
		}
	}
}

// Package tlsutil terminates TLS for the public listener, with certificates
// that can be rotated on disk without a restart.
package tlsutil

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// CertReloader serves the certificate in certFile/keyFile and picks up new
// files when Reload is called, so a renewed certificate (from cert-manager,
// certbot, ...) takes effect without dropping connections. If new files are
// broken, it keeps serving the previous certificate.
type CertReloader struct {
	certFile, keyFile string

	mu   sync.RWMutex
	cert *tls.Certificate
	sum  [sha256.Size]byte
}

// NewCertReloader loads the certificate, failing if the files are unusable.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if _, err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads the files and reports whether the certificate changed.
func (r *CertReloader) Reload() (bool, error) {
	certPEM, err := os.ReadFile(r.certFile)
	if err != nil {
		return false, err
	}
	keyPEM, err := os.ReadFile(r.keyFile)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(append(append([]byte{}, certPEM...), keyPEM...))

	r.mu.RLock()
	unchanged := r.cert != nil && bytes.Equal(sum[:], r.sum[:])
	r.mu.RUnlock()
	if unchanged {
		return false, nil
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, fmt.Errorf("load TLS certificate %s: %w", r.certFile, err)
	}
	r.mu.Lock()
	r.cert, r.sum = &cert, sum
	r.mu.Unlock()
	return true, nil
}

// GetCertificate is a tls.Config.GetCertificate callback serving the current certificate.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// ServerConfig is the TLS configuration for the public listener. HTTP/2 is
// negotiated automatically when net/http serves with it.
func ServerConfig(r *CertReloader, minVersion string) *tls.Config {
	cfg := &tls.Config{GetCertificate: r.GetCertificate, MinVersion: tls.VersionTLS12}
	if minVersion == "1.3" {
		cfg.MinVersion = tls.VersionTLS13
	}
	return cfg
}

// RedirectToHTTPS answers every plain-HTTP request with a permanent redirect
// to the same URL over HTTPS on httpsPort ("443" gives a URL with no port).
func RedirectToHTTPS(httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		target := host
		if strings.Contains(host, ":") {
			target = "[" + host + "]" // a bare IPv6 address
		}
		if httpsPort != "443" && httpsPort != "" {
			target = net.JoinHostPort(host, httpsPort)
		}
		// 308 rather than 301 so clients keep the method and body.
		http.Redirect(w, r, "https://"+target+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

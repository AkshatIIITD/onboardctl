// Package httpx builds the *http.Client shared by the Harbor and Keycloak
// clients, so TLS settings (internal CA, timeouts) are configured in one place.
package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Options controls how the client talks TLS.
type Options struct {
	CAFile   string        // optional PEM file with your internal CA certificate(s)
	Insecure bool          // skip TLS verification (avoid; prefer CAFile)
	Timeout  time.Duration // per-request timeout; defaults to 30s
}

// NewClient returns an HTTP client with a timeout and the requested TLS setup.
func NewClient(opts Options) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.Insecure {
		tlsCfg.InsecureSkipVerify = true // #nosec G402 -- explicit opt-in flag
	}

	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no valid certificates found in %s", opts.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	// Clone the default transport to keep its sane defaults, including
	// HTTP(S)_PROXY / NO_PROXY handling (useful behind a corporate proxy).
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Always set a timeout: the zero-value http.Client waits forever.
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

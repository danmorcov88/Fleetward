package api

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
)

// newRequestID returns a random identifier used to correlate a request's log records with the error
// body returned to the client.
func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// Correlation is a diagnostic aid; failing a request because the entropy source hiccuped
		// would be a poor trade.
		return "unknown"
	}
	return hex.EncodeToString(buf)
}

// loadCertPool reads a PEM bundle for client certificate verification.
func loadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // G304: operator-configured CA bundle path
	if err != nil {
		return nil, fmt.Errorf("tls: read CA bundle %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tls: no certificates found in %q", path)
	}
	return pool, nil
}

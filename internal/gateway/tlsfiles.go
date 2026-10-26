package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// fileCert serves an operator-provided certificate, reloading it when the
// files change (rotation without a daemon restart).
type fileCert struct {
	certFile, keyFile string

	mu      sync.Mutex
	mod     time.Time
	cert    *tls.Certificate
	checked time.Time
}

// newFileCert loads and checks the pair once so a bad configuration fails
// the daemon at start rather than at the first handshake.
func newFileCert(certFile, keyFile string) (*fileCert, error) {
	f := &fileCert{certFile: certFile, keyFile: keyFile}
	if _, err := f.get(); err != nil {
		return nil, err
	}
	return f, nil
}

func modTime(paths ...string) (time.Time, error) {
	var latest time.Time
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return time.Time{}, err
		}
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest, nil
}

// get returns the current certificate, re-reading the files at most once a
// second when their modification time changed.
func (f *fileCert) get() (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cert != nil && time.Since(f.checked) < time.Second {
		return f.cert, nil
	}
	f.checked = time.Now()
	mod, err := modTime(f.certFile, f.keyFile)
	if err != nil {
		if f.cert != nil {
			return f.cert, nil // keep serving the last good pair
		}
		return nil, fmt.Errorf("gateway: tls: %w", err)
	}
	if f.cert != nil && mod.Equal(f.mod) {
		return f.cert, nil
	}
	c, err := LoadCertificate(f.certFile, f.keyFile)
	if err != nil {
		if f.cert != nil {
			return f.cert, nil
		}
		return nil, err
	}
	f.cert, f.mod = c, mod
	return c, nil
}

// LoadCertificate loads a PEM certificate chain and key and rejects a pair
// that is unusable now (mismatched, not yet valid or expired).
func LoadCertificate(certFile, keyFile string) (*tls.Certificate, error) {
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("gateway: tls certificate %s / key %s: %w", certFile, keyFile, err)
	}
	if len(c.Certificate) == 0 {
		return nil, errors.New("gateway: tls: empty certificate chain")
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("gateway: tls: %w", err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("gateway: tls certificate %s is valid %s to %s, not now", certFile,
			leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	c.Leaf = leaf
	return &c, nil
}

func (f *fileCert) listen(ln net.Listener) net.Listener {
	return tls.NewListener(ln, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return f.get()
		},
	})
}

package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePair writes a self-signed certificate for cn valid from nb to na.
func writePair(t *testing.T, dir, cn string, nb, na time.Time) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		DNSNames: []string{cn}, NotBefore: nb, NotAfter: na}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	cf, kf := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(cf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600)
	return cf, kf
}

func TestLoadCertificateRejectsUnusable(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	cf, kf := writePair(t, dir, "ok.example", now.Add(-time.Hour), now.Add(time.Hour))
	c, err := LoadCertificate(cf, kf)
	if err != nil || c.Leaf == nil || c.Leaf.Subject.CommonName != "ok.example" {
		t.Fatalf("valid pair: %v", err)
	}

	other := t.TempDir()
	_, otherKey := writePair(t, other, "other.example", now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := LoadCertificate(cf, otherKey); err == nil {
		t.Fatal("mismatched key accepted")
	}
	expired := t.TempDir()
	ecf, ekf := writePair(t, expired, "old.example", now.Add(-2*time.Hour), now.Add(-time.Hour))
	if _, err := LoadCertificate(ecf, ekf); err == nil || !strings.Contains(err.Error(), "not now") {
		t.Fatalf("expired pair: %v", err)
	}
	if _, err := LoadCertificate(filepath.Join(dir, "missing.pem"), kf); err == nil {
		t.Fatal("missing file accepted")
	}
}

// TestFileCertReloads: a rotated pair is served without a restart; a broken
// rotation keeps the last good pair.
func TestFileCertReloads(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	cf, kf := writePair(t, dir, "one.example", now.Add(-time.Hour), now.Add(time.Hour))
	fc, err := newFileCert(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tl := fc.listen(ln)
	defer tl.Close()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	served := func() string {
		t.Helper()
		c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.ConnectionState().PeerCertificates[0].Subject.CommonName
	}
	if got := served(); got != "one.example" {
		t.Fatalf("served %q", got)
	}

	writePair(t, dir, "two.example", now.Add(-time.Hour), now.Add(time.Hour))
	later := now.Add(time.Minute)
	os.Chtimes(cf, later, later)
	os.Chtimes(kf, later, later)
	fc.mu.Lock()
	fc.checked = time.Time{} // skip the once-a-second throttle
	fc.mu.Unlock()
	if got := served(); got != "two.example" {
		t.Fatalf("after rotation served %q", got)
	}

	os.WriteFile(kf, []byte("garbage"), 0o600)
	evenLater := later.Add(time.Minute)
	os.Chtimes(kf, evenLater, evenLater)
	fc.mu.Lock()
	fc.checked = time.Time{}
	fc.mu.Unlock()
	if got := served(); got != "two.example" {
		t.Fatalf("after a broken rotation served %q, want the last good pair", got)
	}

	if _, err := newFileCert(cf, kf); err == nil {
		t.Fatal("a broken pair must fail at start")
	}
}

func TestStatusRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), StatusFile)
	if _, err := ReadStatus(p); !os.IsNotExist(err) {
		t.Fatalf("missing report: %v", err)
	}
	want := Status{State: EnrollUp, Hostname: "h", FQDN: "h.ts.net", APIURL: apiURL("h.ts.net", TLSFiles), TLS: TLSFiles, PID: 7}
	if err := WriteStatus(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStatus(p)
	if err != nil || got.APIURL != "https://h.ts.net" || got.State != EnrollUp || got.PID != 7 {
		t.Fatalf("read %+v %v", got, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if apiURL("h.ts.net", TLSOff) != "http://h.ts.net" || apiURL("", TLSFiles) != "" {
		t.Fatal("apiURL")
	}
	os.WriteFile(p, []byte(`{}`), 0o600)
	if _, err := ReadStatus(p); err == nil {
		t.Fatal("report without a state accepted")
	}
}

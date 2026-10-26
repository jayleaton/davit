package preflight

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/internal/gateway"
)

func TestGatewayChecks(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	o := Options{TailnetEnabled: true, TailnetStateDir: dir}
	write := func(s gateway.Status) {
		t.Helper()
		if err := gateway.WriteStatus(filepath.Join(dir, gateway.StatusFile), s); err != nil {
			t.Fatal(err)
		}
	}
	enroll := func() Check { return GatewayChecks(o, now)[0] }
	me := os.Getpid()

	if c := GatewayChecks(Options{}, now); c[0].Status != Skip || c[1].Status != Skip {
		t.Fatalf("disabled: %+v", c)
	}
	if c := enroll(); c.Status != Warn || !strings.Contains(c.Detail, "no enrollment report") {
		t.Fatalf("no report: %+v", c)
	}
	for _, tc := range []struct {
		s      gateway.Status
		status Status
		hint   string
	}{
		{gateway.Status{State: gateway.EnrollUp, FQDN: "h.ts.net", APIURL: "https://h.ts.net", PID: me}, Pass, ""},
		{gateway.Status{State: gateway.EnrollUp, FQDN: "h.ts.net", PID: 1 << 30}, Warn, ""}, // stale: daemon gone
		{gateway.Status{State: gateway.EnrollNeedsLogin, LoginURL: "https://login.example/a", PID: me}, Fail, "https://login.example/a"},
		{gateway.Status{State: gateway.EnrollNeedsLogin, PID: me}, Fail, "--auth-key-stdin"},
		{gateway.Status{State: gateway.EnrollNeedsApproval, Hostname: "h", PID: me}, Fail, "approve h"},
		{gateway.Status{State: gateway.EnrollStarting, PID: me, UpdatedAt: now}, Warn, "wait"},
		{gateway.Status{State: gateway.EnrollStarting, PID: me, UpdatedAt: now.Add(-5 * time.Minute)}, Fail, "controlURL"},
		{gateway.Status{State: gateway.EnrollError, Error: "invalid key: unable to validate API key", PID: me}, Fail, "auth key"},
		{gateway.Status{State: gateway.EnrollError, Error: "api listen: address in use", PID: me}, Fail, "hostname"},
	} {
		write(tc.s)
		c := enroll()
		if c.Status != tc.status || !strings.Contains(c.Hint, tc.hint) {
			t.Errorf("%+v: got %s %q / %q", tc.s, c.Status, c.Detail, c.Hint)
		}
	}
	if c := enroll(); !strings.Contains(c.Detail, "address in use") {
		t.Errorf("error detail: %q", c.Detail)
	}
}

func TestGatewayTLSCheck(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	o := Options{TailnetEnabled: true, TailnetStateDir: dir}
	if c := GatewayChecks(o, now)[1]; c.Status != Warn || !strings.Contains(c.Hint, "tailnet.https") {
		t.Fatalf("plain http: %+v", c)
	}
	o.TailnetHTTPS = true
	if c := GatewayChecks(o, now)[1]; c.Status != Pass {
		t.Fatalf("tailscale certs: %+v", c)
	}
	o.TLSCertFile, o.TLSKeyFile = writeSelfSigned(t, dir, "h.ts.net", now.Add(30*24*time.Hour))
	if c := GatewayChecks(o, now)[1]; c.Status != Pass {
		t.Fatalf("operator cert: %+v", c)
	}
	// Close to expiry: renew soon.
	if c := GatewayChecks(o, now.Add(25*24*time.Hour))[1]; c.Status != Warn || !strings.Contains(c.Hint, "renew") {
		t.Fatalf("near expiry: %+v", c)
	}
	// The registered name is not on the certificate.
	gateway.WriteStatus(filepath.Join(dir, gateway.StatusFile), gateway.Status{State: gateway.EnrollUp, FQDN: "other.ts.net", PID: os.Getpid()})
	if c := GatewayChecks(o, now)[1]; c.Status != Fail || !strings.Contains(c.Hint, "other.ts.net") {
		t.Fatalf("name mismatch: %+v", c)
	}
	os.WriteFile(o.TLSKeyFile, []byte("x"), 0o600)
	if c := GatewayChecks(o, now)[1]; c.Status != Fail {
		t.Fatalf("broken key: %+v", c)
	}
}

func writeSelfSigned(t *testing.T, dir, name string, notAfter time.Time) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
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

package pki

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStoreLayout(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}

	wantCert := filepath.Join(s.dir, "ca", "apiserver-ca.crt")
	wantKey := filepath.Join(s.dir, "ca", "apiserver-ca.key")
	if ca.CertPath() != wantCert {
		t.Errorf("CertPath = %q, want %q", ca.CertPath(), wantCert)
	}
	if s.CACertPath(APIServer) != wantCert {
		t.Errorf("CACertPath = %q, want %q", s.CACertPath(APIServer), wantCert)
	}
	for _, f := range []string{wantCert, wantKey} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected %s on disk: %v", f, err)
		}
	}

	l := s.Leaf("etcd-serving")
	if l.CertPath() != filepath.Join(s.dir, "leaf", "etcd-serving.crt") {
		t.Errorf("Leaf CertPath = %q", l.CertPath())
	}
	if l.KeyPath() != filepath.Join(s.dir, "leaf", "etcd-serving.key") {
		t.Errorf("Leaf KeyPath = %q", l.KeyPath())
	}
}

func TestCAIsCached(t *testing.T) {
	s := newTestStore(t)
	ca1, err := s.CA("c")
	if err != nil {
		t.Fatal(err)
	}
	ca2, err := s.CA("c")
	if err != nil {
		t.Fatal(err)
	}
	if ca1 != ca2 {
		t.Error("CA not cached across calls")
	}
}

func TestCAIdempotentReuse(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}
	serial := ca.Cert().SerialNumber.String()
	keyD := ca.key.(*ecdsa.PrivateKey).D.String()

	again, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}
	if again.Cert().SerialNumber.String() != serial {
		t.Error("CA certificate was regenerated on second request")
	}
	if again.key.(*ecdsa.PrivateKey).D.String() != keyD {
		t.Error("CA key was regenerated on second request")
	}
}

func TestCAFreshStoreRegeneratesAfterDelete(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CA(APIServer); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.dir, "ca", "apiserver-ca.crt")); err != nil {
		t.Fatal(err)
	}
	// A new store instance over the same dir must repair the missing cert.
	s2 := NewStore(s.dir)
	if _, err := s2.CA(APIServer); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "ca", "apiserver-ca.crt")); err != nil {
		t.Errorf("CA cert not regenerated: %v", err)
	}
}

func TestIssueSignsLeaf(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}

	ip := netip.MustParseAddr("10.0.0.1")
	l, err := ca.Issue("system-admin", Request{
		CN:       "system:system-admin",
		Org:      []string{"system:masters"},
		DNSNames: []string{"admin.example.com"},
		IPAddrs:  []netip.Addr{ip},
		Usages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		t.Fatal(err)
	}

	cert, err := l.Cert()
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "system:system-admin" {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "system:masters" {
		t.Errorf("Org = %v", cert.Subject.Organization)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "admin.example.com" {
		t.Errorf("DNSNames = %v", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(ip.AsSlice()) {
		t.Errorf("IPAddresses = %v", cert.IPAddresses)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v", cert.ExtKeyUsage)
	}
	if err := cert.CheckSignatureFrom(ca.Cert()); err != nil {
		t.Errorf("leaf not signed by CA: %v", err)
	}
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("leaf missing KeyUsageDigitalSignature")
	}

	// Key on disk matches the cert public key.
	key, err := readKey(l.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatal("leaf cert public key is not ECDSA")
	}
	leafPub := &key.(*ecdsa.PrivateKey).PublicKey
	if leafPub.Curve != pub.Curve || leafPub.X.Cmp(pub.X) != 0 || leafPub.Y.Cmp(pub.Y) != 0 {
		t.Error("on-disk key does not match cert public key")
	}
}

func TestIssueIdempotent(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}

	req := Request{CN: "a", Usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	l1, err := ca.Issue("a", req)
	if err != nil {
		t.Fatal(err)
	}
	c1, err := l1.Cert()
	if err != nil {
		t.Fatal(err)
	}
	l2, err := ca.Issue("a", req)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := l2.Cert()
	if err != nil {
		t.Fatal(err)
	}

	if c1.SerialNumber.Cmp(c2.SerialNumber) != 0 {
		t.Error("leaf regenerated on second Issue")
	}
	if !c1.NotBefore.Equal(c2.NotBefore) {
		t.Error("leaf NotBefore changed on second Issue")
	}
}

func TestIssueKeepsKeyWhenCertMissing(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{CN: "x", Usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	l, err := ca.Issue("x", req)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := readKey(l.KeyPath())
	keyD := key.(*ecdsa.PrivateKey).D.String()

	if err := os.Remove(l.CertPath()); err != nil {
		t.Fatal(err)
	}
	l2, err := ca.Issue("x", req)
	if err != nil {
		t.Fatal(err)
	}
	key2, _ := readKey(l2.KeyPath())
	if key2.(*ecdsa.PrivateKey).D.String() != keyD {
		t.Error("key regenerated although cert was missing")
	}
}

func TestIssueRegeneratesExpired(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{CN: "x", Lifetime: time.Hour, Usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	l, err := ca.Issue("x", req)
	if err != nil {
		t.Fatal(err)
	}

	// Re-issue an expired version of the same cert (same public key,
	// backdated validity window).
	orig, _ := l.Cert()
	tmpl := *orig
	tmpl.NotBefore = time.Now().Add(-48 * time.Hour)
	tmpl.NotAfter = time.Now().Add(-24 * time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, ca.Cert(), orig.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(l.CertPath(), pemBytes, 0600); err != nil {
		t.Fatal(err)
	}

	l2, err := ca.Issue("x", req)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := l2.Cert()
	if err != nil {
		t.Fatal(err)
	}
	if renewed.NotAfter.Before(time.Now()) {
		t.Error("expired leaf was not regenerated")
	}
}

func TestExpiredCAIsRegenerated(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the stored CA cert to be expired.
	ca.cert.NotAfter = time.Now().Add(-time.Hour)
	if err := writeFiles(ca.certPath, ca.keyPath, ca.cert, ca.key); err != nil {
		t.Fatal(err)
	}

	s2 := NewStore(s.dir)
	ca2, err := s2.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}
	if got := ca2.Cert().NotAfter; got.Before(time.Now()) {
		t.Errorf("expired CA cert not regenerated (NotAfter %v)", got)
	}
}

func TestDefaults(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}

	l, err := ca.Issue("defaults", Request{CN: "d"})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := l.Cert()
	if err != nil {
		t.Fatal(err)
	}

	if len(cert.ExtKeyUsage) != 2 ||
		cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth ||
		cert.ExtKeyUsage[1] != x509.ExtKeyUsageClientAuth {
		t.Errorf("default ExtKeyUsage = %v", cert.ExtKeyUsage)
	}
	// Default lifetime: 1 day.
	floor := time.Now().Add(24*time.Hour - time.Minute)
	ceil := time.Now().Add(24*time.Hour + time.Minute)
	if cert.NotAfter.Before(floor) || cert.NotAfter.After(ceil) {
		t.Errorf("NotAfter = %v, want ~1d", cert.NotAfter)
	}
}

func TestLifetimeClamp(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, defaultLifetime},
		{90 * day, maxLifetime},         // above max → 7d
		{30 * time.Minute, minLifetime}, // below min → 1h
		{3 * day, 3 * day},              // in range
	}
	for _, tc := range cases {
		got := (&Request{Lifetime: tc.in}).lifetime()
		if got != tc.want {
			t.Errorf("lifetime(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBase64Accessors(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ca.Issue("b64", Request{})
	if err != nil {
		t.Fatal(err)
	}

	wantCert := base64.URLEncoding.EncodeToString(mustRead(t, l.CertPath()))
	if got := l.CertBase64(); got != wantCert {
		t.Error("CertBase64 mismatch")
	}
	if got := ca.CertBase64(); got == "" {
		t.Error("CA CertBase64 is empty")
	}
	// The CA base64 must match the on-disk PEM bytes.
	wantCA := base64.URLEncoding.EncodeToString(mustRead(t, ca.CertPath()))
	if got := ca.CertBase64(); got != wantCA {
		t.Error("CA CertBase64 mismatch")
	}

	// Missing files yield "".
	if got := s.Leaf("missing").CertBase64(); got != "" {
		t.Errorf("missing leaf CertBase64 = %q, want \"\"", got)
	}
}

func TestTLSCertificate(t *testing.T) {
	s := newTestStore(t)
	ca, err := s.CA(APIServer)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ca.Issue("tls", Request{DNSNames: []string{"server.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	tc, err := l.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Certificate) == 0 {
		t.Error("TLSCertificate returned no cert chain")
	}

	if _, err := s.Leaf("nope").TLSCertificate(); err == nil {
		t.Error("expected error for missing leaf")
	}
}

func TestLeafAccessorsErrorWhenMissing(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Leaf("nope").Cert(); err == nil {
		t.Error("expected Cert error, got nil")
	}
	if _, err := s.Leaf("nope").CertPEM(); err == nil {
		t.Error("expected CertPEM error, got nil")
	}
}

func TestCorruptFilesError(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.dir, "ca"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "ca", "bad-ca.crt"), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CA("bad"); err == nil {
		t.Error("expected error for corrupt CA cert, got nil")
	}

	if err := os.WriteFile(filepath.Join(s.dir, "ca", "bad-ca.key"), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CA("bad"); err == nil {
		t.Error("expected error for corrupt CA key, got nil")
	}
}

func TestErrorMessageIncludesPath(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CA(APIServer); err != nil {
		t.Fatal(err)
	}
	// Corrupt the CA cert on disk and reload through a fresh store.
	bad := filepath.Join(s.dir, "ca", "apiserver-ca.crt")
	if err := os.WriteFile(bad, []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(s.dir).CA(APIServer)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "apiserver-ca.crt") {
		t.Errorf("error does not mention the bad file: %v", err)
	}
}

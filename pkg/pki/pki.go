// Package pki implements a small on-disk certificate authority and
// certificate issuance store.
//
// The store owns the full on-disk layout under one root directory:
//
//	<dir>/ca/<name>-ca.crt     CA certificate (PEM)
//	<dir>/ca/<name>-ca.key     CA private key (EC P-384)
//	<dir>/leaf/<name>.crt      issued certificate (PEM)
//	<dir>/leaf/<name>.key      issued private key (EC P-384)
//
// Access is idempotent: keys and certificates are only generated when
// they are missing (or expired), and existing valid material is reused,
// so restarts do not churn certificates.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Well-known CA names. These double as file name prefixes on disk
// (ca/<name>-ca.crt), so renaming a constant invalidates existing state.
const (
	APIServer          = "apiserver"
	APIClient          = "apiclient"
	Etcd               = "etcd"
	ExtensionAPIServer = "extensionapiserver"
	RequestHeader      = "requestheader"
	KubeletServer      = "kubeletserver"
	WebhookServer      = "webhookserver"
)

const (
	minLifetime     = 1 * time.Hour
	defaultLifetime = 1 * 24 * time.Hour
	maxLifetime     = 7 * 24 * time.Hour
)

// Request describes a leaf certificate to issue. Zero values get safe
// defaults: a zero Lifetime becomes defaultLifetime (clamped to
// [minLifetime, maxLifetime]), and a nil Usages yields
// ServerAuth|ClientAuth.
type Request struct {
	CN       string
	Org      []string
	DNSNames []string
	IPAddrs  []netip.Addr
	Usages   []x509.ExtKeyUsage
	Lifetime time.Duration
}

func (r *Request) usages() []x509.ExtKeyUsage {
	if len(r.Usages) == 0 {
		return []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}
	return r.Usages
}

func (r *Request) lifetime() time.Duration {
	l := r.Lifetime
	if l == 0 {
		l = defaultLifetime
	}
	if l > maxLifetime {
		l = maxLifetime
	}
	if l < minLifetime {
		l = minLifetime
	}
	return l
}

// Store is the root of an on-disk PKI. Create it with NewStore.
type Store struct {
	dir string
	ca  map[string]*CA
	mu  sync.Mutex
}

// NewStore returns a store rooted at dir. Nothing is created on disk
// until a CA is requested.
func NewStore(dir string) *Store {
	return &Store{dir: dir, ca: map[string]*CA{}}
}

// CA returns the certificate authority with the given name, creating it
// (EC P-384 key, self-signed certificate) on first use. Valid on-disk
// material is reused; missing or expired material is regenerated.
// Results are cached; the returned CA is a pointer into the store.
func (s *Store) CA(name string) (*CA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ca, ok := s.ca[name]; ok {
		return ca, nil
	}

	ca := &CA{
		name:     name,
		certPath: filepath.Join(s.dir, "ca", name+"-ca.crt"),
		keyPath:  filepath.Join(s.dir, "ca", name+"-ca.key"),
		store:    s,
	}

	generated := false
	if cert, err := readCertificate(ca.certPath); err != nil {
		return nil, err
	} else {
		ca.cert = cert
	}
	if key, err := readKey(ca.keyPath); err != nil {
		return nil, err
	} else {
		ca.key = key
	}
	if ca.key == nil {
		key, err := generateKey()
		if err != nil {
			return nil, fmt.Errorf("pki: generate %s CA key: %w", name, err)
		}
		ca.key = key
		generated = true
	}
	if ca.cert == nil {
		if err := ca.selfSign(); err != nil {
			return nil, err
		}
		generated = true
	}
	if generated {
		if err := writeFiles(ca.certPath, ca.keyPath, ca.cert, ca.key); err != nil {
			return nil, err
		}
	}

	s.ca[name] = ca
	return ca, nil
}

// CACertPath returns the on-disk path of the CA certificate for name.
// The path is deterministically derived from the store root and the CA
// name; the CA need not exist yet.
func (s *Store) CACertPath(name string) string {
	return filepath.Join(s.dir, "ca", name+"-ca.crt")
}

// Leaf returns the value for the issued certificate with the given name.
// The returned value carries the deterministic on-disk paths and reads
// files lazily in its accessors; it never creates anything itself.
func (s *Store) Leaf(name string) *Leaf {
	return &Leaf{
		name:     name,
		certPath: filepath.Join(s.dir, "leaf", name+".crt"),
		keyPath:  filepath.Join(s.dir, "leaf", name+".key"),
	}
}

// CA is a certificate authority: a private key and a self-signed
// certificate on disk. Obtain one from Store.CA.
type CA struct {
	name     string
	certPath string
	keyPath  string
	cert     *x509.Certificate
	key      crypto.Signer
	store    *Store
}

// Name returns the CA name, which is also its on-disk file name prefix.
func (ca *CA) Name() string { return ca.name }

// CertPath returns the path of the CA certificate on disk.
func (ca *CA) CertPath() string { return ca.certPath }

// Cert returns the parsed CA certificate.
func (ca *CA) Cert() *x509.Certificate { return ca.cert }

// CertPEM returns the CA certificate as PEM.
func (ca *CA) CertPEM() ([]byte, error) {
	if ca.cert == nil {
		return nil, fmt.Errorf("pki: %s: CA certificate not loaded", ca.certPath)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), nil
}

// CertBase64 returns the CA certificate as a URL-safe base64 string of
// its PEM encoding, or "" if it cannot be encoded.
func (ca *CA) CertBase64() string {
	d, err := ca.CertPEM()
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(d)
}

// Issue returns the leaf certificate with the given name, issuing it if
// it does not exist or is expired. Existing material is reused in
// idempotent fashion: a valid leaf is returned as-is, a leaf with a
// valid key but missing or expired certificate gets re-signed with the
// same key, and a missing key is generated fresh.
func (ca *CA) Issue(name string, req Request) (*Leaf, error) {
	l := ca.store.Leaf(name)

	generated := false
	cert, err := readCertificate(l.certPath)
	if err != nil {
		return nil, err
	}
	key, err := readKey(l.keyPath)
	if err != nil {
		return nil, err
	}
	if key == nil {
		key, err = generateKey()
		if err != nil {
			return nil, fmt.Errorf("pki: generate %s key: %w", name, err)
		}
		generated = true
	}
	if cert == nil {
		cert, err = ca.sign(req, key)
		if err != nil {
			return nil, err
		}
		generated = true
	}
	if generated {
		if err := writeFiles(l.certPath, l.keyPath, cert, key); err != nil {
			return nil, err
		}
	}

	return l, nil
}

func (ca *CA) selfSign() error {
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject: pkix.Name{
			CommonName: ca.name,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(defaultLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, ca.key.Public(), ca.key)
	if err != nil {
		return fmt.Errorf("pki: create %s CA certificate: %w", ca.name, err)
	}
	cert, err := x509.ParseCertificate(certBytes)
	if err != nil {
		return fmt.Errorf("pki: parse %s CA certificate: %w", ca.name, err)
	}
	ca.cert = cert
	return nil
}

func (ca *CA) sign(req Request, key crypto.Signer) (*x509.Certificate, error) {
	caCert := ca.cert
	if caCert == nil || ca.key == nil {
		return nil, fmt.Errorf("pki: CA %s certificate or key not loaded", ca.name)
	}

	ips := make([]net.IP, 0, len(req.IPAddrs))
	for _, a := range req.IPAddrs {
		ips = append(ips, a.AsSlice())
	}

	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject: pkix.Name{
			CommonName:   req.CN,
			Organization: req.Org,
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(req.lifetime()),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: req.usages(),
		DNSNames:    req.DNSNames,
		IPAddresses: ips,
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, key.Public(), ca.key)
	if err != nil {
		return nil, fmt.Errorf("pki: sign certificate %q: %w", req.CN, err)
	}
	cert, err := x509.ParseCertificate(certBytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse signed certificate %q: %w", req.CN, err)
	}
	return cert, nil
}

// Leaf is the value for one issued certificate/key pair. Paths are
// deterministic from the store root and the leaf name; data accessors
// read from disk on each call and return errors when the files are
// missing. Nothing here creates files.
type Leaf struct {
	name     string
	certPath string
	keyPath  string
}

// Name returns the leaf name.
func (l *Leaf) Name() string { return l.name }

// CertPath returns the on-disk path of the leaf certificate.
func (l *Leaf) CertPath() string { return l.certPath }

// KeyPath returns the on-disk path of the leaf private key.
func (l *Leaf) KeyPath() string { return l.keyPath }

// CertPEM returns the leaf certificate as PEM.
func (l *Leaf) CertPEM() ([]byte, error) { return os.ReadFile(l.certPath) }

// KeyPEM returns the leaf private key as PEM.
func (l *Leaf) KeyPEM() ([]byte, error) { return os.ReadFile(l.keyPath) }

// Cert returns the parsed leaf certificate, or an error when the file
// is missing or invalid.
func (l *Leaf) Cert() (*x509.Certificate, error) {
	cert, err := readCertificate(l.certPath)
	if err != nil {
		return nil, err
	}
	if cert == nil {
		return nil, fmt.Errorf("pki: %s: no valid certificate", l.certPath)
	}
	return cert, nil
}

// CertBase64 returns the leaf certificate as a URL-safe base64 string
// of its PEM encoding, or "" when the file is missing.
func (l *Leaf) CertBase64() string {
	d, err := os.ReadFile(l.certPath)
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(d)
}

// KeyBase64 returns the leaf private key as a URL-safe base64 string of
// its PEM encoding, or "" when the file is missing.
func (l *Leaf) KeyBase64() string {
	d, err := os.ReadFile(l.keyPath)
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(d)
}

// TLSCertificate loads the leaf certificate and key as a
// tls.Certificate, suitable for use as a server or client certificate.
func (l *Leaf) TLSCertificate() (tls.Certificate, error) {
	return tls.LoadX509KeyPair(l.certPath, l.keyPath)
}

func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		// crypto/rand.Read fails only if the entropy source is broken;
		// treat that as unrecoverable.
		panic(fmt.Errorf("pki: generate serial: %w", err))
	}
	return n
}

func generateKey() (crypto.Signer, error) {
	return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
}

// readCertificate reads and parses a PEM certificate file. A missing
// file returns (nil, nil); an expired or not-yet-valid certificate is
// logged and also returns (nil, nil) so it can be regenerated.
func readCertificate(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pki: read %s: %w", path, err)
	}

	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("pki: %s: no CERTIFICATE block", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: parse certificate: %w", path, err)
	}

	now := time.Now().UTC()
	if now.After(cert.NotAfter) || now.Before(cert.NotBefore) {
		slog.Warn("pki: certificate outside validity window, regenerating",
			"path", path, "not_before", cert.NotBefore, "not_after", cert.NotAfter, "now", now)
		return nil, nil
	}
	return cert, nil
}

// readKey reads and parses an EC private key PEM file. A missing file
// returns (nil, nil).
func readKey(path string) (crypto.Signer, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pki: read %s: %w", path, err)
	}

	block, _ := pem.Decode(b)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("pki: %s: no EC PRIVATE KEY block", path)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: parse private key: %w", path, err)
	}
	return key, nil
}

func writeFiles(certPath, keyPath string, cert *x509.Certificate, key crypto.Signer) error {
	dir := filepath.Dir(certPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("pki: create directory %s: %w", dir, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return fmt.Errorf("pki: write %s: %w", certPath, err)
	}

	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("pki: write %s: %w", keyPath, err)
	}
	return nil
}

func encodeKey(key crypto.Signer) ([]byte, error) {
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("pki: unsupported key type %T", key)
	}
	der, err := x509.MarshalECPrivateKey(ec)
	if err != nil {
		return nil, fmt.Errorf("pki: marshal EC private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

# pkg/pki — certificate authority & issuance library

A self-contained, on-disk certificate authority and certificate issuance
store, used by runkube for its control-plane PKI. It depends only on the
stdlib (`crypto/*`, `net`, `netip`, `time`).

## Design

1. **Idempotent load-or-create.** CAs and issued certificates are
   loaded from disk and only generated when missing or expired; restarts
   do not churn certificates.
2. **Explicit errors.** No nil-returning accessors; failures are
   returned as `error`.
3. **The package owns the on-disk layout.** Callers hand it one root
   directory; path assembly, directory creation, and PEM encoding are
   its job.
4. **Issued artifacts are values, not bags.** A `*Leaf` exposes
   read-only accessors (paths, PEM, base64, parsed cert, TLS key pair).

## On-disk layout

```
<dir>/
  ca/<name>-ca.crt        CA certificate (PEM)
  ca/<name>-ca.key        CA private key (EC P-384)
  leaf/<name>.crt         issued certificate (PEM)
  leaf/<name>.key         issued private key (EC P-384)
```

The well-known CA names (`pki.APIServer`, `pki.Etcd`, ...) are package
constants, since they are embedded in the file names.

## Usage

```go
s := pki.NewStore(filepath.Join(root, "etc/kubernetes/pki"))

// A CA is created on first use and cached.
etcdCA, err := s.CA(pki.Etcd)

// Issue a leaf, reusing valid on-disk material.
l, err := etcdCA.Issue("etcd-serving", pki.Request{
    CN:       "etcd-serving.example",
    DNSNames: []string{"etcd.sock"},
    Usages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
})

l.CertPath()                  // component CLI flags
l.CertBase64() / l.KeyBase64() // kubeconfigs
l.TLSCertificate()            // tls.Certificate for servers/clients
ca.CertPEM()                  // CA bundles
ca.CertBase64()               // certificate-authority-data
```

Path-only accessors (`Store.CACertPath`, `Store.Leaf`) do no I/O, so
they work before any certificate exists.

## Public API

```go
func NewStore(dir string) *Store

func (s *Store) CA(name string) (*CA, error)   // load-or-bootstrap, cached
func (s *Store) CACertPath(name string) string  // path only, no I/O
func (s *Store) Leaf(name string) *Leaf         // path-only value; reads lazily

type Request struct {
    CN       string
    Org      []string
    DNSNames []string
    IPAddrs  []netip.Addr
    Usages   []x509.ExtKeyUsage // nil → ServerAuth | ClientAuth
    Lifetime time.Duration      // 0 → 1d, clamped to [1h, 7d]
}

type CA struct{ /* unexported */ }
func (ca *CA) Name() string
func (ca *CA) CertPath() string
func (ca *CA) CertPEM() ([]byte, error)
func (ca *CA) CertBase64() string            // URL-safe base64 of CertPEM
func (ca *CA) Cert() *x509.Certificate
func (ca *CA) Issue(name string, req Request) (*Leaf, error)

type Leaf struct{ /* unexported */ }
func (l *Leaf) Name() string
func (l *Leaf) CertPath() string
func (l *Leaf) KeyPath() string
func (l *Leaf) CertPEM() ([]byte, error)
func (l *Leaf) KeyPEM() ([]byte, error)
func (l *Leaf) CertBase64() string
func (l *Leaf) KeyBase64() string
func (l *Leaf) Cert() (*x509.Certificate, error)
func (l *Leaf) TLSCertificate() (tls.Certificate, error)
```

## Issuance semantics

- Keys: ECDSA P-384 (`elliptic.P384()`).
- CA cert: self-signed, `IsCA`, `KeyUsage CertSign|DigitalSignature`,
  `ExtKeyUsage ServerAuth|ClientAuth`, default 1d lifetime.
- Leaf cert: Subject (CN/Org) from `Request`, `KeyUsage
  DigitalSignature|KeyEncipherment`, `ExtKeyUsage` from `Request`
  (nil → `ServerAuth|ClientAuth`), DNS/IP SANs copied verbatim, random
  128-bit serial, `NotBefore` now / `NotAfter` = now +
  `Request.Lifetime` clamped to `[1h, 7d]` (0 → 1d).
- Lifetimes are deliberately short so certificates rotate frequently;
  raise per-request via `Request.Lifetime` (up to the 7d max).
- Expired or missing certificates are regenerated (logged at WARN): a
  leaf keeps its key when only its certificate is missing/expired, and
  a CA keeps its key when its certificate expires.
- Base64 accessors return "" when the file is missing; binary accessors
  return an error.
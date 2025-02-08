package runkube

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"
)

// CAName represents the name of a Certificate Authority.
type CAName string

const (
	// APIServer is the CA for the Kubernetes API server.
	APIServer = CAName("apiserver")
	// APIClient is the CA for Kubernetes API clients.
	APIClient = CAName("apiclient")
	// Etcd is the CA for etcd.
	Etcd = CAName("etcd")
	// ExtensionAPIServer is the CA for extension API servers.
	ExtensionAPIServer = CAName("extensionapiserver")
	// RequestHeader is the CA for request header authentication.
	RequestHeader = CAName("requestheader")
	// KubeletServer is the CA for Kubelet servers.
	KubeletServer = CAName("kubeletserver")
	// WebhookServer is the CA for webhook servers.
	WebhookServer = CAName("webhookserver")
)

// CA represents a Certificate Authority.
type CA struct {
	name     string
	cn       string
	lifetime time.Duration
	p        *pkiRef
}

// Name returns the name of the CA.
func (ca *CA) Name() string {
	return ca.name
}

const certLifetimeMin = time.Hour * 24 * 7
const certLifetimeMax = time.Hour * 24 * 180

func clamp(l, floor, ceil time.Duration) time.Duration {
	if l > ceil {
		return ceil
	}

	if l < floor {
		return floor
	}
	return l
}

func (ca *CA) certificateForRequest(req *Request, csr *x509.CertificateRequest) (*x509.Certificate, error) {
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("invalid request self signature")
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	return &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      csr.Subject,
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(clamp(req.lifetime, certLifetimeMin, certLifetimeMax)),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  req.usages,
		DNSNames:     csr.DNSNames,
		IPAddresses:  csr.IPAddresses,
	}, nil
}

// Sign signs a certificate request and returns the certificate.
func (ca *CA) Sign(req *Request, csr *x509.CertificateRequest) ([]byte, error) {
	if ca.p.cert == nil || ca.p.key == nil {
		return nil, fmt.Errorf("ca certificate or key not loaded")
	}

	tmpl, err := ca.certificateForRequest(req, csr)
	if err != nil {
		return nil, err
	}

	return x509.CreateCertificate(rand.Reader, tmpl, ca.p.cert, csr.PublicKey, ca.p.key)
}

// Init initializes the CA with the provided pkiRef.
func (ca *CA) Init(p *pkiRef) error {
	logger := slog.Default().With("cacert", p.certPath)
	ca.p = p

	if err := p.Read(); err != nil {
		return err
	}

	var err error
	if ca.p.key == nil {
		logger.Debug("req", "stage", "generating key")
		ca.p.key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			return fmt.Errorf("failed to generate private key: %w", err)
		}
	}

	if ca.p.cert == nil {
		serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return fmt.Errorf("failed to generate serial number: %w", err)
		}

		caTmpl := &x509.Certificate{
			SerialNumber: serialNumber,
			Subject: pkix.Name{
				CommonName: ca.name,
			},
			NotBefore:             time.Now(),
			NotAfter:              time.Now().Add(clamp(ca.lifetime, certLifetimeMin, certLifetimeMax)),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{
				x509.ExtKeyUsageServerAuth,
				x509.ExtKeyUsageClientAuth,
			},
		}

		certBytes, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, ca.p.key.Public(), ca.p.key)
		if err != nil {
			return fmt.Errorf("failed to create certificate: %w", err)
		}

		ca.p.cert, err = x509.ParseCertificate(certBytes)
		if err != nil {
			return fmt.Errorf("failed to parse generated certificate: %w", err)
		}

		return ca.p.Write()
	}

	return nil
}

// Request represents a certificate signing request.
type Request struct {
	name     string
	signer   CAName
	usages   []x509.ExtKeyUsage
	lifetime time.Duration
	cn       string
	o        []string
	san      []string
	ip       []netip.Addr
}

// CA returns the Certificate Authority with the given name.
func (c *ControlPlane) CA(caName CAName) *CA {
	if c.caMap == nil {
		c.caMap = map[CAName]*CA{}
	}

	if c, ok := c.caMap[caName]; ok {
		return c
	}

	ca := &CA{name: string(caName)}
	if err := ca.Init(c.caRef(caName)); err != nil {
		return nil
	}

	c.caMap[caName] = ca
	return ca
}

type pkiRef struct {
	certPath string
	keyPath  string
	cert     *x509.Certificate
	key      crypto.Signer
}

func (p *pkiRef) TLSCertificate() func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(p.certPath, p.keyPath)
	return func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return &cert, err
	}
}

func (p *pkiRef) errorf(msg string, args ...any) error {
	return errors.Join(errors.New(p.certPath), fmt.Errorf(msg, args...))
}

func (p *pkiRef) CertBase64String() string {
	d, err := os.ReadFile(p.certPath)
	if err != nil {
		return ""
	}

	return base64.URLEncoding.EncodeToString(d)
}

func (p *pkiRef) KeyBase64String() string {
	d, err := os.ReadFile(p.keyPath)
	if err != nil {
		return ""
	}

	return base64.URLEncoding.EncodeToString(d)
}

func (p *pkiRef) Read() error {
	certBytes, err := os.ReadFile(p.certPath)
	if err == nil {
		block, _ := pem.Decode(certBytes)
		if block == nil {
			return p.errorf("no block set %v", certBytes)
		}
		if block.Type != "CERTIFICATE" {
			return p.errorf("invalid block type(%s)", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return p.errorf("parse certificate: %w", err)
		}

		now := time.Now().UTC()
		if now.After(cert.NotAfter) || now.Before(cert.NotBefore) {
			slog.Warn("expired", "cert", cert.Subject, "before", cert.NotBefore, "now", now, "after", cert.NotAfter)
			return nil
		}
	}

	keyBytes, err := os.ReadFile(p.keyPath)
	if err == nil {
		block, _ := pem.Decode(keyBytes)
		if block == nil {
			return p.errorf("no block set %v", keyBytes)
		}
		if block.Type != "EC PRIVATE KEY" {
			return p.errorf("invalid block type(%s)", block.Type)
		}
		priv, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return p.errorf("parse private key: %w", err)
		}

		p.key = priv
	}

	return nil
}

func (p *pkiRef) CertData() ([]byte, error) {
	if p.cert == nil {
		return nil, p.errorf("cannot PEM encode nil cert")
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: p.cert.Raw,
	}), nil
}

func (p *pkiRef) KeyData() ([]byte, error) {
	if p.key == nil {
		return nil, p.errorf("cannot PEM encode nil key")
	}

	privKeyBytes, err := x509.MarshalECPrivateKey(p.key.(*ecdsa.PrivateKey))
	if err != nil {
		return nil, p.errorf("cannot encode ec private key")
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: privKeyBytes,
	}), nil
}

func (p *pkiRef) Write() error {
	if err := os.MkdirAll(filepath.Dir(p.certPath), 0700); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	certPEM, err := p.CertData()
	if err != nil {
		return err
	}

	if err := os.WriteFile(p.certPath, certPEM, 0600); err != nil {
		return p.errorf("failed to write certificate: %w", err)
	}

	keyPEM, err := p.KeyData()
	if err != nil {
		return err
	}

	if err := os.WriteFile(p.keyPath, keyPEM, 0600); err != nil {
		return p.errorf("failed to write private key: %w", err)
	}

	return nil
}

func fingerprint(k crypto.PublicKey) string {
	p, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return ":unknown-key-fingerprint:"
	}
	w := crypto.SHA224.New()
	io.Copy(w, bytes.NewReader(p))
	return hex.EncodeToString(w.Sum(nil))
}

// Certificate generates and writes a certificate and key if they do not exist.
func (ca *CA) Certificate(p *pkiRef, req *Request) error {
	logger := slog.Default().With("ca", ca.name)
	if err := p.Read(); err != nil {
		return err
	}

	if p.key == nil {
		priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			return fmt.Errorf("failed to generate private key: %w", err)
		}
		p.key = priv
		logger.Debug("req", "key.fingerprint", fingerprint(p.key.Public()))
	}

	if p.cert == nil {
		ips := []net.IP{}
		for _, a := range req.ip {
			ips = append(ips, net.ParseIP(a.String()))
		}
		logger.Debug("generating new cert")
		reqTmpl := &x509.CertificateRequest{
			Subject: pkix.Name{
				CommonName:   req.cn,
				Organization: req.o,
			},
			SignatureAlgorithm: x509.ECDSAWithSHA384,
			IPAddresses:        ips,
			DNSNames:           req.san,
		}
		logger.Debug("req", "key.fingerprint", fingerprint(p.key.Public()))
		csrBytes, err := x509.CreateCertificateRequest(rand.Reader, reqTmpl, p.key)
		if err != nil {
			return err
		}
		csr, err := x509.ParseCertificateRequest(csrBytes)
		if err != nil {
			return err
		}

		logger.Debug("req", "pubkey", fingerprint(csr.PublicKey))
		certBytes, err := ca.Sign(req, csr)
		if err != nil {
			return err
		}
		p.cert, err = x509.ParseCertificate(certBytes)
		if err != nil {
			return err
		}
	}

	return p.Write()
}

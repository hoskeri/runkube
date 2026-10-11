package runkube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func testControlPlane(t *testing.T) *ControlPlane {
	t.Helper()
	return &ControlPlane{
		Name:             "test",
		Root:             t.TempDir(),
		ExternalHostname: "test.runkube.local",
		Nodes:            []string{},
	}
}

func TestInitWritesPKI(t *testing.T) {
	c := testControlPlane(t)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	pkiDir := filepath.Join(c.Root, "etc/kubernetes/pki")
	for _, name := range []string{"apiserver", "apiclient", "etcd", "extensionapiserver", "requestheader", "kubeletserver", "webhookserver"} {
		for _, ext := range []string{"crt", "key"} {
			p := filepath.Join(pkiDir, "ca", name+"-ca."+ext)
			if _, err := os.Stat(p); err != nil {
				t.Errorf("missing %s: %v", p, err)
			}
		}
	}

	leaves := []string{
		"etcd-serving", "etcd-peer-client", "apiserver-etcd-client", "webhook-server",
		"apiserver-serving", "apiserver-kubelet-client", "apiserver-extension-client",
		"requestheader-client", "kubelet-server-unused", "system-admin",
		"controller-manager-client", "controller-manager-authnz", "controller-manager-server",
		"scheduler-client", "scheduler-server", "scheduler-authnz",
	}
	for _, name := range leaves {
		for _, ext := range []string{"crt", "key"} {
			p := filepath.Join(pkiDir, "leaf", name+"."+ext)
			if _, err := os.Stat(p); err != nil {
				t.Errorf("missing %s: %v", p, err)
			}
		}
	}

	// The apiserver serving cert must be issued by the apiserver CA.
	apiserverCA := mustLoadCA(t, filepath.Join(pkiDir, "ca", "apiserver-ca.crt"))
	serving := mustLoadCert(t, filepath.Join(pkiDir, "leaf", "apiserver-serving.crt"))
	if err := serving.CheckSignatureFrom(apiserverCA); err != nil {
		t.Errorf("apiserver-serving not signed by apiserver CA: %v", err)
	}

	// The webhook cert must be loadable as a TLS key pair.
	if _, err := tls.LoadX509KeyPair(
		filepath.Join(pkiDir, "leaf", "webhook-server.crt"),
		filepath.Join(pkiDir, "leaf", "webhook-server.key"),
	); err != nil {
		t.Errorf("webhook-server not a valid TLS pair: %v", err)
	}

	// Kubeconfigs for APIClient-signed leaves must exist.
	for _, name := range []string{"system-admin", "scheduler-client"} {
		p := c.kubeConfigRef(name).Path
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing kubeconfig %s: %v", p, err)
		}
	}
}

func TestInitIdempotent(t *testing.T) {
	c := testControlPlane(t)
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	servingPath := filepath.Join(c.Root, "etc/kubernetes/pki", "leaf", "apiserver-serving.crt")
	before := mustLoadCert(t, servingPath)

	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := mustLoadCert(t, servingPath)

	if before.SerialNumber.Cmp(after.SerialNumber) != 0 {
		t.Error("second Init regenerated apiserver-serving certificate")
	}
	if !before.NotBefore.Equal(after.NotBefore) {
		t.Error("second Init changed apiserver-serving NotBefore")
	}
}

func mustLoadCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := parsePEMCert(b)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func mustLoadCA(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	return mustLoadCert(t, path)
}

func parsePEMCert(b []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE block")
	}
	return x509.ParseCertificate(block.Bytes)
}

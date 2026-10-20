package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureCert_GeneratesValidPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if err := EnsureCert(certPath, keyPath, "192.168.1.50"); err != nil {
		t.Fatalf("EnsureCert: %v", err)
	}

	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("generated pair is not a valid TLS keypair: %v", err)
	}

	// Security properties (M3): a plain server leaf, not a 10-year CA.
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("reading cert: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing cert: %v", err)
	}
	if cert.IsCA {
		t.Errorf("cert must not be a CA")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Errorf("cert must not carry KeyUsageCertSign")
	}
	if cert.NotAfter.After(time.Now().AddDate(2, 0, 1)) {
		t.Errorf("cert validity should be ~2 years, got NotAfter=%v", cert.NotAfter)
	}
	var hasLAN bool
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.ParseIP("192.168.1.50")) {
			hasLAN = true
		}
	}
	if !hasLAN {
		t.Errorf("expected the LAN IP as a SAN, got %v", cert.IPAddresses)
	}
}

func TestEnsureCert_DoesNotOverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if err := EnsureCert(certPath, keyPath, "192.168.1.50"); err != nil {
		t.Fatalf("EnsureCert (first call): %v", err)
	}
	original, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("reading generated cert: %v", err)
	}

	if err := EnsureCert(certPath, keyPath, "192.168.1.50"); err != nil {
		t.Fatalf("EnsureCert (second call): %v", err)
	}
	again, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("reading cert after second call: %v", err)
	}

	if string(original) != string(again) {
		t.Fatalf("EnsureCert overwrote an existing cert (operator-supplied certs must take precedence)")
	}
}

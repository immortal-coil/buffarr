// Package tlscert generates a self-signed TLS certificate on first boot so
// the backend can serve HTTPS without an operator-supplied cert (KTD17).
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// EnsureCert returns cert/key file paths the backend can pass to
// http.ListenAndServeTLS. If both files already exist, it leaves them
// untouched (an operator-supplied cert takes precedence). Otherwise it
// generates a self-signed ECDSA leaf cert valid for the local LAN and writes
// it to certPath/keyPath. lanIP, when a valid address, is added as a SAN so the
// cert also matches direct HTTPS access at the server's own LAN IP.
func EnsureCert(certPath, keyPath, lanIP string) error {
	if fileExists(certPath) && fileExists(keyPath) {
		return nil
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generating serial number: %w", err)
	}

	// A plain TLS server leaf: only DigitalSignature + ServerAuth, not a CA.
	// (A self-signed cert with IsCA+CertSign would let a leaked key sign other
	// certs; this serves HTTPS and nothing else.)
	ips := []net.IP{net.IPv4(127, 0, 0, 1)}
	if ip := net.ParseIP(lanIP); ip != nil {
		ips = append(ips, ip)
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "buffarr"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("creating certificate: %w", err)
	}

	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return err
	}

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshaling private key: %w", err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyBytes, 0o600); err != nil {
		return err
	}

	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}

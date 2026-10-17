package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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

// ensureTLSConfig loads the self-signed cert/key from dataDir/tls, generating
// them on first use (design architecture §8). The key and cert are written 0600
// so the private key stays operator-only; on Windows the surrounding data dir is
// already ACL-restricted. Any failure is returned so the caller can skip the TLS
// listener rather than failing to serve: plain HTTP is independent of it.
func ensureTLSConfig(dataDir string) (*tls.Config, error) {
	tlsDir := filepath.Join(dataDir, "tls")
	certPath := filepath.Join(tlsDir, "cert.pem")
	keyPath := filepath.Join(tlsDir, "key.pem")

	if !fileExists(certPath) || !fileExists(keyPath) {
		if err := generateSelfSigned(tlsDir, certPath, keyPath); err != nil {
			return nil, fmt.Errorf("generate self-signed cert: %w", err)
		}
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load key pair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// generateSelfSigned writes a fresh P-256 self-signed cert valid for localhost,
// the loopback IPs, and this host's name.
func generateSelfSigned(dir, certPath, keyPath string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "asiair-commander"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, hostname)
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	if err := writePEM(certPath, "CERTIFICATE", der); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	return writePEM(keyPath, "EC PRIVATE KEY", keyDER)
}

// writePEM atomically writes a single PEM block to path with 0600 permissions: it
// encodes into a temp file in the same directory and renames it over the target.
// The rename (rather than an in-place O_TRUNC) means a regenerated cert/key can
// replace a prior one even when that file's ACL would deny a direct write — the
// same failure mode that crash-looped a config .bak refresh under the
// least-privilege Windows service account.
func writePEM(path, blockType string, der []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pem-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := pem.Encode(tmp, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

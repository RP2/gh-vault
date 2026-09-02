package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certFileName  = "cert.pem"
	keyFileName   = "key.pem"
	keyFPFileName = "key.fp"
)

// EnsureSelfSignedCert checks for an existing TLS certificate and key in
// <dataDir>/tls. If either is missing or invalid, it generates a new ECDSA
// P-256 self-signed certificate valid for 1 year and writes both files atomically.
// A SHA-256 fingerprint of the private key PEM is written to key.fp so that a
// substituted key can be detected on subsequent starts.
//
// The certificate includes SANs for localhost, 127.0.0.1, ::1, and the
// container hostname when it is a valid DNS name.
func EnsureSelfSignedCert(dataDir string) (certPath string, keyPath string, err error) {
	tlsDir := filepath.Join(dataDir, "tls")
	certPath = filepath.Join(tlsDir, certFileName)
	keyPath = filepath.Join(tlsDir, keyFileName)

	if certKeyValid(certPath, keyPath) {
		return certPath, keyPath, nil
	}

	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		return "", "", fmt.Errorf("create tls dir: %w", err)
	}

	slog.Info("generating self-signed TLS certificate", "dir", tlsDir)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate ecdsa key: %w", err)
	}

	commonName := "localhost"
	hostname, err := os.Hostname()
	if err == nil && hostname != "" && isValidDNSName(hostname) {
		commonName = hostname
	}

	dnsNames := []string{"localhost"}
	if commonName != "localhost" {
		dnsNames = append(dnsNames, commonName)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", fmt.Errorf("generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"gh-vault"},
			CommonName:   commonName,
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return "", "", fmt.Errorf("create certificate: %w", err)
	}

	if err := writePEMAtomic(certPath+".tmp", certPath, "CERTIFICATE", certDER, 0644); err != nil {
		return "", "", fmt.Errorf("write cert file: %w", err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", fmt.Errorf("marshal private key: %w", err)
	}
	if err := writePEMAtomic(keyPath+".tmp", keyPath, "PRIVATE KEY", privBytes, 0600); err != nil {
		return "", "", fmt.Errorf("write key file: %w", err)
	}

	fpPath := filepath.Join(tlsDir, keyFPFileName)
	if err := writeKeyFingerprint(keyPath, fpPath); err != nil {
		return "", "", fmt.Errorf("write key fingerprint: %w", err)
	}

	slog.Info("wrote self-signed TLS certificate", "cert", certPath, "key", keyPath)
	return certPath, keyPath, nil
}

// writePEMAtomic writes PEM-encoded data to a temporary file and renames it to
// finalPath after the file is closed successfully. On any error the temporary
// file is removed.
func writePEMAtomic(tmpPath, finalPath, blockType string, bytes []byte, perm os.FileMode) error {
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: bytes}); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// keyFingerprint returns the lowercase hex SHA-256 digest of keyPEM.
func keyFingerprint(keyPEM []byte) string {
	sum := sha256.Sum256(keyPEM)
	return hex.EncodeToString(sum[:])
}

// writeKeyFingerprint reads the private key at keyPath and writes its
// SHA-256 fingerprint to fpPath.
func writeKeyFingerprint(keyPath, fpPath string) error {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	return os.WriteFile(fpPath, []byte(keyFingerprint(keyPEM)), 0600)
}

// certKeyValid reports whether both certificate and key files exist, are
// well-formed PEM, parse successfully, have matching ECDSA public/private keys,
// match the recorded key fingerprint, are currently within their validity
// window, and still cover the current container hostname.
func certKeyValid(certPath, keyPath string) bool {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return false
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return false
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return false
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return false
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return false
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return false
	}
	ecPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	if ecPub.X.Cmp(ecKey.X) != 0 || ecPub.Y.Cmp(ecKey.Y) != 0 {
		return false
	}

	fpPath := filepath.Join(filepath.Dir(keyPath), keyFPFileName)
	storedFP, err := os.ReadFile(fpPath)
	if err != nil {
		return false
	}
	if string(storedFP) != keyFingerprint(keyPEM) {
		return false
	}

	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return false
	}

	hostname, _ := os.Hostname()
	if !hostnameCovered(hostname, cert.DNSNames, cert.IPAddresses) {
		return false
	}

	return true
}

// hostnameCovered reports whether hostname is present in the certificate's
// subject alternative names. localhost and empty hostnames are always
// considered covered. Invalid DNS names that could not have been included in
// the certificate are also treated as covered to avoid forcing regeneration
// for hostnames such as Docker default names containing underscores.
func hostnameCovered(hostname string, dnsNames []string, ipAddrs []net.IP) bool {
	if hostname == "" || hostname == "localhost" {
		return true
	}
	if ip := net.ParseIP(hostname); ip != nil {
		for _, certIP := range ipAddrs {
			if certIP.Equal(ip) {
				return true
			}
		}
		return false
	}
	if !isValidDNSName(hostname) {
		return true
	}
	for _, dns := range dnsNames {
		if dns == hostname {
			return true
		}
	}
	return false
}

// isValidDNSName reports whether name is a valid DNS name per RFC 952/1123.
func isValidDNSName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !isValidDNSLabel(label) {
			return false
		}
	}
	return true
}

func isValidDNSLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 {
		return false
	}
	for i, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(label)-1:
		default:
			return false
		}
	}
	return true
}

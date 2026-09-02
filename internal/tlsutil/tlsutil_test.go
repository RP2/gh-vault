package tlsutil

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureSelfSignedCert_GeneratesOnFirstRun(t *testing.T) {
	dataDir := t.TempDir()

	certPath, keyPath, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("EnsureSelfSignedCert failed: %v", err)
	}

	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("expected cert file at %s: %v", certPath, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("expected key file at %s: %v", keyPath, err)
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("failed to stat key file: %v", err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Errorf("key file permissions = %o, want %o", keyInfo.Mode().Perm(), 0600)
	}

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("failed to stat cert file: %v", err)
	}
	if certInfo.Mode().Perm() != 0644 {
		t.Errorf("cert file permissions = %o, want %o", certInfo.Mode().Perm(), 0644)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("failed to read cert file: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("failed to decode cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	if time.Until(cert.NotAfter) < 364*24*time.Hour {
		t.Errorf("certificate expires too soon: %s", cert.NotAfter)
	}
	if time.Until(cert.NotAfter) > 367*24*time.Hour {
		t.Errorf("certificate expires too far in the future: %s", cert.NotAfter)
	}

	hasLocalhost := false
	hasWildcard := false
	for _, dns := range cert.DNSNames {
		if dns == "localhost" {
			hasLocalhost = true
		}
		if dns == "*" {
			hasWildcard = true
		}
	}
	if !hasLocalhost {
		t.Errorf("expected localhost in DNSNames, got %v", cert.DNSNames)
	}
	if hasWildcard {
		t.Errorf("unexpected bare wildcard in DNSNames, got %v", cert.DNSNames)
	}

	has127 := false
	for _, ip := range cert.IPAddresses {
		if ip.String() == "127.0.0.1" {
			has127 = true
			break
		}
	}
	if !has127 {
		t.Errorf("expected 127.0.0.1 in IPAddresses, got %v", cert.IPAddresses)
	}
}

func TestEnsureSelfSignedCert_ReusesExisting(t *testing.T) {
	dataDir := t.TempDir()

	certPath1, keyPath1, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("first EnsureSelfSignedCert failed: %v", err)
	}

	certBefore, err := os.ReadFile(certPath1)
	if err != nil {
		t.Fatalf("failed to read cert before second call: %v", err)
	}
	keyBefore, err := os.ReadFile(keyPath1)
	if err != nil {
		t.Fatalf("failed to read key before second call: %v", err)
	}

	certPath2, keyPath2, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("second EnsureSelfSignedCert failed: %v", err)
	}
	if certPath1 != certPath2 || keyPath1 != keyPath2 {
		t.Fatalf("paths changed between calls: %s/%s vs %s/%s", certPath1, keyPath1, certPath2, keyPath2)
	}

	certAfter, err := os.ReadFile(certPath2)
	if err != nil {
		t.Fatalf("failed to read cert after second call: %v", err)
	}
	keyAfter, err := os.ReadFile(keyPath2)
	if err != nil {
		t.Fatalf("failed to read key after second call: %v", err)
	}

	if string(certBefore) != string(certAfter) {
		t.Errorf("cert changed between calls")
	}
	if string(keyBefore) != string(keyAfter) {
		t.Errorf("key changed between calls")
	}
}

func TestEnsureSelfSignedCert_RegeneratesWhenIncomplete(t *testing.T) {
	dataDir := t.TempDir()

	certPath, _, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("first EnsureSelfSignedCert failed: %v", err)
	}

	certBefore, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("failed to read cert before deletion: %v", err)
	}

	keyPath := filepath.Join(dataDir, "tls", keyFileName)
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("failed to remove key file: %v", err)
	}

	certPath2, keyPath2, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("second EnsureSelfSignedCert failed: %v", err)
	}
	if certPath != certPath2 || keyPath != keyPath2 {
		t.Fatalf("paths changed between calls: %s/%s vs %s/%s", certPath, keyPath, certPath2, keyPath2)
	}

	certAfter, err := os.ReadFile(certPath2)
	if err != nil {
		t.Fatalf("failed to read cert after regeneration: %v", err)
	}
	if string(certBefore) == string(certAfter) {
		t.Errorf("expected cert to be regenerated after key was removed")
	}
}

func TestHostnameCovered(t *testing.T) {
	cases := []struct {
		name     string
		hostname string
		dnsNames []string
		ipAddrs  []net.IP
		want     bool
	}{
		{"empty", "", []string{"localhost"}, nil, true},
		{"localhost", "localhost", []string{"localhost"}, nil, true},
		{"dns match", "truenas", []string{"localhost", "truenas"}, nil, true},
		{"dns mismatch", "old-host", []string{"localhost", "new-host"}, nil, false},
		{"ip match", "192.168.1.1", []string{"localhost"}, []net.IP{net.ParseIP("192.168.1.1")}, true},
		{"ip mismatch", "192.168.1.2", []string{"localhost"}, []net.IP{net.ParseIP("192.168.1.1")}, false},
		{"invalid dns skipped", "my_container", []string{"localhost"}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostnameCovered(tc.hostname, tc.dnsNames, tc.ipAddrs); got != tc.want {
				t.Errorf("hostnameCovered(%q, %v, %v) = %v, want %v", tc.hostname, tc.dnsNames, tc.ipAddrs, got, tc.want)
			}
		})
	}
}

func TestEnsureSelfSignedCert_RegeneratesWhenHostnameDrifts(t *testing.T) {
	dataDir := t.TempDir()
	tlsDir := filepath.Join(dataDir, "tls")
	certPath := filepath.Join(tlsDir, certFileName)
	keyPath := filepath.Join(tlsDir, keyFileName)

	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		t.Fatalf("failed to create tls dir: %v", err)
	}

	certPEM, keyPEM := generateCertForHostname(t, "old-host")
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}

	certPath2, _, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("EnsureSelfSignedCert failed: %v", err)
	}

	certAfter, err := os.ReadFile(certPath2)
	if err != nil {
		t.Fatalf("failed to read regenerated cert: %v", err)
	}
	if bytes.Equal(certPEM, certAfter) {
		t.Errorf("expected cert to be regenerated after hostname drift")
	}
}

func TestIsValidDNSName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"localhost", true},
		{"truenas-scale", true},
		{"truenas-scale.local", true},
		{"truenas_scale", false},
		{"-invalid", false},
		{"invalid-", false},
		{"", false},
		{"a.b.c", true},
		{"a..b", false},
		{"a.b.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isValidDNSName(tc.name); got != tc.want {
				t.Errorf("isValidDNSName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestEnsureSelfSignedCert_RegeneratesWhenCertCorrupt(t *testing.T) {
	dataDir := t.TempDir()

	certPath, _, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("first EnsureSelfSignedCert failed: %v", err)
	}

	certBefore, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("failed to read cert before corruption: %v", err)
	}

	if err := os.WriteFile(certPath, []byte("not a certificate"), 0644); err != nil {
		t.Fatalf("failed to corrupt cert file: %v", err)
	}

	certPath2, _, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("second EnsureSelfSignedCert failed: %v", err)
	}
	if certPath != certPath2 {
		t.Fatalf("cert path changed between calls: %s vs %s", certPath, certPath2)
	}

	certAfter, err := os.ReadFile(certPath2)
	if err != nil {
		t.Fatalf("failed to read cert after regeneration: %v", err)
	}
	if bytes.Equal(certBefore, certAfter) {
		t.Errorf("expected cert to be regenerated after corruption")
	}
}

func TestEnsureSelfSignedCert_RegeneratesWhenKeyCorrupt(t *testing.T) {
	dataDir := t.TempDir()

	_, keyPath, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("first EnsureSelfSignedCert failed: %v", err)
	}

	if err := os.WriteFile(keyPath, []byte("not a key"), 0600); err != nil {
		t.Fatalf("failed to corrupt key file: %v", err)
	}

	certPath2, keyPath2, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("second EnsureSelfSignedCert failed: %v", err)
	}
	if keyPath != keyPath2 {
		t.Fatalf("key path changed between calls: %s vs %s", keyPath, keyPath2)
	}

	certPEM, err := os.ReadFile(certPath2)
	if err != nil {
		t.Fatalf("failed to read regenerated cert: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("failed to decode regenerated cert PEM")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Errorf("regenerated cert is invalid: %v", err)
	}
}

func TestEnsureSelfSignedCert_RegeneratesWhenExpired(t *testing.T) {
	dataDir := t.TempDir()
	tlsDir := filepath.Join(dataDir, "tls")
	certPath := filepath.Join(tlsDir, certFileName)
	keyPath := filepath.Join(tlsDir, keyFileName)

	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		t.Fatalf("failed to create tls dir: %v", err)
	}

	certPEM, keyPEM := generateExpiredCert(t)
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		t.Fatalf("failed to write expired cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("failed to write expired key: %v", err)
	}

	certPath2, _, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("EnsureSelfSignedCert failed: %v", err)
	}

	certAfter, err := os.ReadFile(certPath2)
	if err != nil {
		t.Fatalf("failed to read regenerated cert: %v", err)
	}
	if bytes.Equal(certPEM, certAfter) {
		t.Errorf("expected expired cert to be regenerated")
	}
}

func TestEnsureSelfSignedCert_RegeneratesWhenFingerprintMismatch(t *testing.T) {
	dataDir := t.TempDir()

	certPath, keyPath, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("first EnsureSelfSignedCert failed: %v", err)
	}

	certBefore, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("failed to read cert before fingerprint mismatch: %v", err)
	}

	fpPath := filepath.Join(filepath.Dir(keyPath), keyFPFileName)
	if err := os.WriteFile(fpPath, []byte("deadbeef"), 0600); err != nil {
		t.Fatalf("failed to corrupt key fingerprint: %v", err)
	}

	certPath2, _, err := EnsureSelfSignedCert(dataDir)
	if err != nil {
		t.Fatalf("second EnsureSelfSignedCert failed: %v", err)
	}

	certAfter, err := os.ReadFile(certPath2)
	if err != nil {
		t.Fatalf("failed to read cert after regeneration: %v", err)
	}
	if bytes.Equal(certBefore, certAfter) {
		t.Errorf("expected cert to be regenerated when key fingerprint mismatches")
	}
}

func generateCertForHostname(t *testing.T, hostname string) (certPEM, keyPEM []byte) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate rsa key: %v", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate serial number: %v", err)
	}

	dnsNames := []string{"localhost"}
	if hostname != "localhost" {
		dnsNames = append(dnsNames, hostname)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"gh-vault"},
			CommonName:   hostname,
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal private key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	return certPEM, keyPEM
}

func generateExpiredCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate rsa key: %v", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate serial number: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"gh-vault"},
			CommonName:   "localhost",
		},
		NotBefore:             time.Now().Add(-2 * time.Hour),
		NotAfter:              time.Now().Add(-1 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal private key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	return certPEM, keyPEM
}

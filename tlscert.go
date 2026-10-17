package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// loadOrCreateCert lädt das angegebene Zertifikat oder erzeugt beim ersten
// Start ein selbstsigniertes (10 Jahre gültig) im Datenverzeichnis. Der
// Client merkt sich beim ersten Verbinden den Fingerabdruck (Trust on First
// Use) und warnt, falls er sich später ändert.
func loadOrCreateCert(certFile, keyFile, dataDir string, addresses []string) (tls.Certificate, error) {
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err == nil {
			logFingerprint(cert)
		}
		return cert, err
	}
	dir := filepath.Join(dataDir, "tls")
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if cert, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		logFingerprint(cert)
		return cert, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "MIRC Chatserver", Organization: []string{"MIRC"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	seen := map[string]bool{}
	for _, a := range addresses {
		host := a
		if h, _, err := net.SplitHostPort(a); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if seen[host] {
			continue
		}
		seen[host] = true
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	log.Printf("Selbstsigniertes TLS-Zertifikat erzeugt: %s", certFile)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err == nil {
		logFingerprint(cert)
	}
	return cert, err
}

func logFingerprint(cert tls.Certificate) {
	if len(cert.Certificate) == 0 {
		return
	}
	sum := sha256.Sum256(cert.Certificate[0])
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	log.Printf("TLS-Fingerabdruck (SHA-256): %s", strings.Join(parts, ":"))
}

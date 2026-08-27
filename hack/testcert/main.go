// Command testcert creates a short-lived CA and TLS certificate for local E2E
// infrastructure. It is deliberately a development helper, never server code.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	out := flag.String("out", "", "output directory")
	hosts := flag.String("hosts", "", "comma-separated DNS names")
	flag.Parse()
	if *out == "" || *hosts == "" {
		log.Fatal("-out and -hosts are required")
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		log.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	caKey := key()
	ca := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "donkey e2e package CA"},
		NotBefore: now, NotAfter: now.Add(48 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		log.Fatal(err)
	}
	leafKey := key()
	leaf := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: strings.Split(*hosts, ",")[0]},
		DNSNames: strings.Split(*hosts, ","), NotBefore: now, NotAfter: now.Add(48 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		log.Fatal(err)
	}
	write(filepath.Join(*out, "ca.crt"), "CERTIFICATE", caDER)
	write(filepath.Join(*out, "tls.crt"), "CERTIFICATE", leafDER)
	write(filepath.Join(*out, "tls.key"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(leafKey))
}

func key() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	return key
}

func serial() *big.Int {
	value, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		log.Fatal(err)
	}
	return value
}

func write(path, typ string, der []byte) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	if err := pem.Encode(file, &pem.Block{Type: typ, Bytes: der}); err != nil {
		_ = file.Close()
		log.Fatal(err)
	}
	if err := file.Close(); err != nil {
		log.Fatal(err)
	}
}

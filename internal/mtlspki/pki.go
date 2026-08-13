// Package mtlspki creates the offline Phase 1 worker mTLS hierarchy.
package mtlspki

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"time"
)

func NewRoot(now time.Time) (certPEM, keyPEM []byte, err error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "WatchTrace Phase 1 offline worker root"}, NotBefore: now.UTC().Add(-time.Minute), NotAfter: now.UTC().AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func Issue(rootCertPEM, rootKeyPEM []byte, poolID string, now time.Time, validity time.Duration) (certPEM, keyPEM []byte, serial string, err error) {
	if poolID == "" || validity <= 0 || validity > 30*24*time.Hour {
		return nil, nil, "", errors.New("invalid mTLS issuance")
	}
	rootBlock, _ := pem.Decode(rootCertPEM)
	keyBlock, _ := pem.Decode(rootKeyPEM)
	if rootBlock == nil || keyBlock == nil {
		return nil, nil, "", errors.New("invalid root material")
	}
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil {
		return nil, nil, "", err
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, "", err
	}
	rootKey, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, "", errors.New("invalid root key")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, "", err
	}
	number, err := randomSerial()
	if err != nil {
		return nil, nil, "", err
	}
	template := &x509.Certificate{SerialNumber: number, Subject: pkix.Name{CommonName: poolID}, NotBefore: now.UTC().Add(-time.Minute), NotAfter: now.UTC().Add(validity), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, DNSNames: []string{poolID}}
	der, err := x509.CreateCertificate(rand.Reader, template, root, public, rootKey)
	if err != nil {
		return nil, nil, "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), number.String(), nil
}
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

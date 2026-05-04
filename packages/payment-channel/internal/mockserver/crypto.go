package mockserver

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// marshalPKIXPublicKeyPEM returns the PEM-encoded SPKI form of pub, matching
// what real channels distribute to merchants.
func marshalPKIXPublicKeyPEM(pub *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return string(b), nil
}

// MarshalPKCS8PrivateKeyPEM is used by tests/fixtures that want to hand a
// private key to a merchant adapter config. Exported so cmd/mockserver can
// print it at startup.
func MarshalPKCS8PrivateKeyPEM(priv *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return string(b), nil
}

func rsaSignSHA256B64(priv *rsa.PrivateKey, buf []byte) (string, error) {
	if priv == nil {
		return "", fmt.Errorf("nil priv")
	}
	sum := sha256.Sum256(buf)
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

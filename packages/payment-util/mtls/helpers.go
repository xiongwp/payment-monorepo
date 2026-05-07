package mtls

import (
	"crypto/x509"
	"encoding/pem"
)

// buildCertPool parses PEM-encoded certificate data and returns a CertPool.
// Returns nil if parsing fails.
func buildCertPool(pemData []byte) *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil
	}
	return pool
}

// parseCertificates extracts X.509 certificates from PEM-encoded data.
func parseCertificates(pemData []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate

	for len(pemData) > 0 {
		var block *pem.Block
		block, pemData = pem.Decode(pemData)
		if block == nil {
			break
		}

		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, err
			}
			certs = append(certs, cert)
		}
	}

	return certs, nil
}

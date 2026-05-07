// Package mtls provides mTLS certificate loading and gRPC credential builders
// for production secure inter-service communication.
package mtls

import (
	"crypto/tls"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc/credentials"
)

// Config holds mTLS certificate paths and configuration.
type Config struct {
	ServerCertPath string // Path to server certificate (from MTLS_SERVER_CERT)
	ServerKeyPath  string // Path to server key (from MTLS_SERVER_KEY)
	CACertPath     string // Path to CA certificate (from MTLS_CA_CERT)

	// InsecureDev allows insecure mode ONLY when INSECURE_DIAL=1 and env != prod.
	// Strict fail-fast in production.
	InsecureDev bool
}

// LoadFromEnv loads mTLS configuration from environment variables.
// Fails hard if production (ENVIRONMENT=prod/production) and certs are missing.
func LoadFromEnv() (Config, error) {
	cfg := Config{
		ServerCertPath: os.Getenv("MTLS_SERVER_CERT"),
		ServerKeyPath:  os.Getenv("MTLS_SERVER_KEY"),
		CACertPath:     os.Getenv("MTLS_CA_CERT"),
		InsecureDev:    os.Getenv("INSECURE_DIAL") == "1",
	}

	env := os.Getenv("ENVIRONMENT")
	isProd := env == "prod" || env == "production"

	// Production fail-fast: mTLS certificates REQUIRED
	if isProd {
		if cfg.ServerCertPath == "" {
			return Config{}, fmt.Errorf("PROD-SAFETY: MTLS_SERVER_CERT environment variable is required in production")
		}
		if cfg.ServerKeyPath == "" {
			return Config{}, fmt.Errorf("PROD-SAFETY: MTLS_SERVER_KEY environment variable is required in production")
		}
		if cfg.CACertPath == "" {
			return Config{}, fmt.Errorf("PROD-SAFETY: MTLS_CA_CERT environment variable is required in production")
		}
		// Disable insecure mode in production
		cfg.InsecureDev = false
	}

	return cfg, nil
}

// ServerTLSConfig builds a tls.Config for use by gRPC servers with mTLS.
// Returns error if certificates cannot be loaded or are invalid (expired, etc).
func (c *Config) ServerTLSConfig() (*tls.Config, error) {
	if c.ServerCertPath == "" || c.ServerKeyPath == "" || c.CACertPath == "" {
		return nil, fmt.Errorf("mtls: incomplete certificate configuration (cert=%q, key=%q, ca=%q)",
			c.ServerCertPath, c.ServerKeyPath, c.CACertPath)
	}

	// Load server cert + key
	cert, err := tls.LoadX509KeyPair(c.ServerCertPath, c.ServerKeyPath)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server cert/key: %w", err)
	}

	// Verify cert expiration (fail-fast on expired certs)
	if err := verifyCertNotExpired(c.ServerCertPath); err != nil {
		return nil, fmt.Errorf("mtls: server cert validation: %w", err)
	}

	// Load CA cert for client verification
	caCertBytes, err := os.ReadFile(c.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA cert: %w", err)
	}

	caCertPool := buildCertPool(caCertBytes)
	if caCertPool == nil {
		return nil, fmt.Errorf("mtls: failed to parse CA cert")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caCertPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig builds a tls.Config for use by gRPC clients with mTLS.
// Returns error if certificates cannot be loaded or are invalid (expired, etc).
func (c *Config) ClientTLSConfig() (*tls.Config, error) {
	if c.ServerCertPath == "" || c.ServerKeyPath == "" || c.CACertPath == "" {
		return nil, fmt.Errorf("mtls: incomplete certificate configuration (cert=%q, key=%q, ca=%q)",
			c.ServerCertPath, c.ServerKeyPath, c.CACertPath)
	}

	// Load client cert + key
	cert, err := tls.LoadX509KeyPair(c.ServerCertPath, c.ServerKeyPath)
	if err != nil {
		return nil, fmt.Errorf("mtls: load client cert/key: %w", err)
	}

	// Verify cert expiration (fail-fast on expired certs)
	if err := verifyCertNotExpired(c.ServerCertPath); err != nil {
		return nil, fmt.Errorf("mtls: client cert validation: %w", err)
	}

	// Load server CA cert for server verification
	caCertBytes, err := os.ReadFile(c.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA cert: %w", err)
	}

	caCertPool := buildCertPool(caCertBytes)
	if caCertPool == nil {
		return nil, fmt.Errorf("mtls: failed to parse CA cert")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ServerCredentials returns gRPC credentials for use by servers.
// Fails hard if certificates are invalid or missing in production.
func (c *Config) ServerCredentials() (credentials.TransportCredentials, error) {
	tlsConfig, err := c.ServerTLSConfig()
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(tlsConfig), nil
}

// ClientCredentials returns gRPC credentials for use by clients.
// Fails hard if certificates are invalid or missing in production.
func (c *Config) ClientCredentials() (credentials.TransportCredentials, error) {
	tlsConfig, err := c.ClientTLSConfig()
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(tlsConfig), nil
}

// verifyCertNotExpired checks that a certificate file hasn't expired.
// This provides early fail-fast feedback for deployment issues.
func verifyCertNotExpired(certPath string) error {
	certBytes, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("read cert: %w", err)
	}

	certs, err := parseCertificates(certBytes)
	if err != nil {
		return fmt.Errorf("parse cert: %w", err)
	}

	if len(certs) == 0 {
		return fmt.Errorf("no certificates found in %s", certPath)
	}

	// Check first cert (main certificate) expiration
	cert := certs[0]
	if time.Now().After(cert.NotAfter) {
		return fmt.Errorf("certificate expired at %v (now: %v)", cert.NotAfter, time.Now())
	}

	// Warn if cert will expire soon (within 7 days)
	if time.Until(cert.NotAfter) < 7*24*time.Hour {
		fmt.Fprintf(os.Stderr, "WARNING: Certificate will expire soon at %v\n", cert.NotAfter)
	}

	return nil
}

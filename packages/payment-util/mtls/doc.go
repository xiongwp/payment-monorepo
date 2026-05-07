// Package mtls provides mTLS certificate management and gRPC credentials for
// production-grade inter-service communication.
//
// # Usage
//
// Load certificate configuration from environment:
//
//	cfg, err := mtls.LoadFromEnv()
//	if err != nil {
//		log.Fatalf("mtls config: %v", err)
//	}
//
// For gRPC servers:
//
//	creds, err := cfg.ServerCredentials()
//	if err != nil {
//		log.Fatalf("server credentials: %v", err)
//	}
//	server := grpc.NewServer(grpc.Creds(creds), ...)
//
// For gRPC clients:
//
//	creds, err := cfg.ClientCredentials()
//	if err != nil {
//		log.Fatalf("client credentials: %v", err)
//	}
//	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds), ...)
//
// # Environment Variables
//
// - MTLS_SERVER_CERT: Path to server certificate file (PEM format)
// - MTLS_SERVER_KEY: Path to server private key file (PEM format)
// - MTLS_CA_CERT: Path to CA certificate for verification (PEM format)
// - INSECURE_DIAL: Set to "1" to allow insecure mode in dev environments only
// - ENVIRONMENT: Set to "prod" or "production" to enforce strict mTLS validation
//
// # Production Safety
//
// In production (ENVIRONMENT=prod or production):
// - All three certificate paths (MTLS_SERVER_CERT, MTLS_SERVER_KEY, MTLS_CA_CERT)
//   are REQUIRED and LoadFromEnv will fail-fast if missing
// - INSECURE_DIAL is forcefully disabled
// - Certificate expiration is validated at startup
// - Expired certificates will cause startup to fail
//
// # Development
//
// In development, set INSECURE_DIAL=1 to use insecure connections (not recommended for sensitive data).
// Production deployments MUST NOT set INSECURE_DIAL=1 and MUST provide valid certificates.
package mtls

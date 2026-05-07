# mTLS Full Chain Path Deployment Guide

## Overview

This document describes the mTLS (mutual TLS) configuration required for production inter-service communication in the payment system.

**P0-5 Status**: mTLS is now enforced on all inter-service gRPC connections to prevent unauthorized access to critical payment paths (api-gateway ↔ user-merchant-core, order-core, payment-core, etc).

## Required Environment Variables

All services that dial other services must have the following environment variables configured:

```bash
# Certificate and key paths (required in production, optional in dev)
MTLS_SERVER_CERT=/path/to/server.crt    # Server certificate (PEM format)
MTLS_SERVER_KEY=/path/to/server.key     # Server private key (PEM format)
MTLS_CA_CERT=/path/to/ca.crt            # CA certificate for verification (PEM format)

# Production deployment
ENVIRONMENT=production                   # or "prod"

# Development only (strictly forbidden in production)
INSECURE_DIAL=0                         # Leave unset or set to 0; set to 1 ONLY in dev/test
```

## Service Configuration

### Services Requiring mTLS Client Credentials

The following services dial to other services and require mTLS configuration:

1. **payment-admin-web (BFF)**
   - Dials: order-core, payment-core, kms-manage, risk-manage, user-merchant-core
   - Config: Set all three MTLS_* env vars in production
   - File: `packages/payment-admin-web/backend/cmd/server/main.go`

2. **api-gateway**
   - Dials: user-merchant-core, order-core
   - Config: Set all three MTLS_* env vars in production
   - File: `packages/api-gateway/cmd/server/main.go`

3. **user-merchant-core**
   - Dials: risk-manage, accounting-system
   - Config: Set all three MTLS_* env vars in production
   - File: `packages/user-merchant-core/cmd/server/main.go`

4. **order-core**
   - Dials: payment-core, accounting-system, webhook-delivery
   - Config: Set all three MTLS_* env vars in production
   - File: `packages/order-core/cmd/server/main.go`

5. **payment-core**
   - Dials: kms-manage, payment-channel, risk-manage
   - Config: Set all three MTLS_* env vars in production

6. **payment-channel**
   - Dials: payment-core (internal coordination)
   - Config: Set all three MTLS_* env vars in production

### Services Serving gRPC (Not modified yet, planned for Phase 2)

All services that listen on gRPC ports should enforce mTLS in production (not yet implemented):
- order-core, payment-core, user-merchant-core, kms-manage, risk-manage, accounting-system, payment-channel

## Certificate Requirements

### Format
- **PEM (Privacy Enhanced Mail)** format required for all certificates
- Certificates must be valid X.509 certificates
- Private keys must be in PKCS#8 format (recommended) or PKCS#1 (traditional RSA format)

### Validity
- Certificates must not be expired at startup
- Services check certificate expiration at startup and fail-fast if expired
- Warning is logged if certificate expires within 7 days

### Generation Example

```bash
# Generate CA private key and certificate
openssl genrsa -out ca.key 2048
openssl req -new -x509 -days 3650 -key ca.key -out ca.crt \
  -subj "/CN=payment-system-ca"

# Generate server certificate signed by CA
openssl genrsa -out server.key 2048
openssl req -new -key server.key -out server.csr \
  -subj "/CN=order-core.internal"
openssl x509 -req -days 365 -in server.csr \
  -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -sha256

# Note: In production, use proper PKI infrastructure (Vault, cert-manager, etc)
```

## Kubernetes Deployment

### Secret Creation
```bash
# Create a secret containing mTLS certificates
kubectl create secret tls payment-mtls-certs \
  --cert=server.crt \
  --key=server.key \
  --dry-run=client -o yaml | kubectl apply -f -

# Store CA cert in a ConfigMap
kubectl create configmap payment-ca-cert \
  --from-file=ca.crt=ca.crt \
  --dry-run=client -o yaml | kubectl apply -f -
```

### Pod Configuration
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: order-core-pod
spec:
  containers:
  - name: order-core
    image: order-core:latest
    env:
    - name: ENVIRONMENT
      value: "production"
    - name: MTLS_SERVER_CERT
      value: /etc/payment-mtls/cert/server.crt
    - name: MTLS_SERVER_KEY
      value: /etc/payment-mtls/cert/server.key
    - name: MTLS_CA_CERT
      value: /etc/payment-mtls/ca/ca.crt
    volumeMounts:
    - name: mtls-certs
      mountPath: /etc/payment-mtls/cert
      readOnly: true
    - name: ca-cert
      mountPath: /etc/payment-mtls/ca
      readOnly: true
  volumes:
  - name: mtls-certs
    secret:
      secretName: payment-mtls-certs
  - name: ca-cert
    configMap:
      name: payment-ca-cert
```

## Docker Compose Development

For local development with docker-compose:

```yaml
version: '3.8'
services:
  order-core:
    image: order-core:dev
    environment:
      ENVIRONMENT: dev  # Not "production"
      # Leave MTLS_* variables unset OR set INSECURE_DIAL=1
      INSECURE_DIAL: "1"
    # No volume mounts needed for dev mode

  api-gateway:
    image: api-gateway:dev
    environment:
      ENVIRONMENT: dev
      INSECURE_DIAL: "1"
    depends_on:
      - order-core
```

## Fallback/Backward Compatibility

### Development Mode
- If MTLS_* variables are not set and ENVIRONMENT != "production": services use insecure connections
- Set INSECURE_DIAL=1 explicitly for clarity (optional in dev, but recommended)

### Production Mode (Fail-Fast)
- If any of MTLS_SERVER_CERT, MTLS_SERVER_KEY, or MTLS_CA_CERT are missing: **startup fails immediately**
- If certificates are expired: **startup fails immediately**
- INSECURE_DIAL=1 is forcefully ignored in production (env var is checked and disabled)

### Migration Path
1. **Phase 1** (Current): Client-side mTLS enforcement (BFF, API Gateway, service-to-service clients)
2. **Phase 2** (Planned): Server-side mTLS enforcement (gRPC servers require valid client certificates)
3. **Phase 3** (Optional): Mutual certificate rotation and audit

## Debugging

### Check Certificate Details
```bash
# View certificate details
openssl x509 -in server.crt -text -noout

# Verify certificate chain
openssl verify -CAfile ca.crt server.crt

# Check expiration date
openssl x509 -enddate -noout -in server.crt
```

### Service Logs
Look for:
- `mtls config:` errors → certificate path issues
- `load mTLS credentials:` errors → certificate parsing/validation failures
- `expired` messages → certificate expiration

### Production Monitoring
- Monitor certificate expiration dates (alert 30+ days before expiration)
- Monitor mTLS connection failures in logs
- Set up automated certificate rotation before expiration

## Implementation Details

### Code Changes
- **payment-util/mtls** package: Core mTLS credential loading and management
- **payment-util/serviceregistry/dial.go**: Updated with MTLSDialOptions helper
- **Service main.go files**: Updated to load mTLS config and use it in DialWithFallback calls

### Development-Only Usage
The following remain insecure and are protected by build tags or test-only contexts:
- `packages/*/testhelper/server.go` - test fixtures
- `packages/*/cmd/*test/main.go` - test utilities
- `packages/*/cmd/grpc-client/main.go` - manual testing tools

## Troubleshooting

### "PROD-SAFETY: MTLS_SERVER_CERT environment variable is required in production"
**Cause**: Production deployment missing certificate paths
**Fix**: Set all three MTLS_* env vars before starting service

### "certificate expired"
**Cause**: Certificate has passed its NotAfter date
**Fix**: Regenerate or renew certificate before deploying

### "parse cert: no certificates found in..."
**Cause**: Certificate file is empty or malformed
**Fix**: Verify certificate file exists and contains valid PEM data

### "failed to parse CA cert"
**Cause**: CA certificate file is invalid
**Fix**: Check CA cert file format (must be PEM), run `openssl verify`

### Services timeout trying to connect
**Cause**: Could be certificate mismatch, CN/SAN validation issues, or firewall
**Fix**: Check service logs for specific credential errors; verify CA cert matches

## Future Enhancements

1. **SAN (Subject Alternative Name) Validation**: Load allowed client CNs from config-center per service
2. **Automatic Rotation**: Integrate with cert-manager or Vault for automated rotation
3. **Audit Logging**: Log all mTLS connection attempts and failures
4. **Metrics**: Export Prometheus metrics for certificate expiration, connection failures

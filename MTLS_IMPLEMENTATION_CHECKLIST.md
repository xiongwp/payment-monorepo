# P0-5 mTLS Implementation Checklist

## Code Changes Complete

### Phase 1: mTLS Package & Infrastructure
- [x] Create `packages/payment-util/mtls/mtls.go`
  - LoadFromEnv() with production fail-fast
  - ServerTLSConfig() / ClientTLSConfig()
  - ServerCredentials() / ClientCredentials()
  - Certificate expiration validation
  
- [x] Create `packages/payment-util/mtls/helpers.go`
  - buildCertPool() for CA cert parsing
  - parseCertificates() for X.509 extraction

- [x] Create `packages/payment-util/mtls/doc.go`
  - Package documentation
  - Environment variable reference
  - Usage examples

- [x] Update `packages/payment-util/serviceregistry/dial.go`
  - Add MTLSDialOptions() helper function

### Phase 1: Production Service Wire-Up
- [x] payment-admin-web/backend/cmd/server/main.go
  - Remove `insecure` import
  - Add `mtls` import
  - Update mustDial() function
  - Test: 5 dial paths (order-core, payment-core, kms-manage, risk-manage, user-merchant-core)

- [x] api-gateway/cmd/server/main.go
  - Add `mtls` import
  - Update newUserMerchantConn()
  - Update newOrderCoreConn()
  - Test: 2 dial paths (user-merchant-core, order-core)

- [x] user-merchant-core/cmd/server/main.go
  - Add `mtls` import
  - Update newRiskClient() with graceful degradation
  - Update newAccountingClient() with graceful degradation
  - Test: 2 dial paths (risk-manage, accounting-system)

- [x] payment-core/internal/kmsclient/client.go
  - Add `mtls` import
  - Update Dial() function
  - Test: 1 dial path (kms-manage)

- [x] order-core/internal/channel/paymentcoreclient/client.go
  - Add `mtls` import
  - Update Dial() function
  - Test: 1 dial path (payment-core)

### Phase 1: Documentation
- [x] Create MTLS_DEPLOYMENT.md
  - Environment variable reference
  - Certificate format and generation
  - Kubernetes manifests examples
  - Docker Compose examples
  - Troubleshooting guide
  - Future enhancements

- [x] Create MTLS_CHANGES_SUMMARY.md
  - Implementation overview
  - Security improvements matrix
  - Compatibility matrix (prod vs dev)
  - Service-by-service changes
  - Deployment checklist
  - Rollback plan

- [x] Create COMMIT_MESSAGES.txt
  - Commit 1: mTLS infrastructure
  - Commit 2: Service wire-up
  - Detailed change descriptions

## Code Quality Checks

### Imports & Dependencies
- [x] All new imports are from standard library or existing monorepo packages
- [x] No circular dependencies introduced
- [x] payment-util/mtls has no dependencies on service-specific code

### Error Handling
- [x] Production mode: Startup panics on missing certificates
- [x] Production mode: Startup panics on expired certificates
- [x] Development mode: Graceful fallback to insecure
- [x] All errors include descriptive messages (PROD-SAFETY prefix for prod errors)

### Backward Compatibility
- [x] Dev/test code paths unaffected (testhelper/server.go remains insecure)
- [x] INSECURE_DIAL=1 allows old behavior in development
- [x] Services start successfully without mTLS certs in non-prod environments

### Security Validation
- [x] INSECURE_DIAL is disabled in production (forced to false)
- [x] Certificate paths are validated at startup (not deferred)
- [x] Certificate expiration is checked at startup
- [x] No plaintext certificate content exposed in logs
- [x] TLS version >= 1.2 enforced in all configs

## Testing Verification (Manual Steps)

### Unit Tests
- [ ] Run `go test ./packages/payment-util/mtls -v`
- [ ] Verify certificate parsing works
- [ ] Verify production fail-fast behavior
- [ ] Verify development fallback behavior

### Integration Tests
- [ ] Run payment-admin-web with mocked certs
- [ ] Verify dial succeeds with valid certs
- [ ] Verify startup panics without certs in prod mode
- [ ] Verify startup succeeds without certs in dev mode

### Manual Testing Checklist

**Test 1: Production Fail-Fast**
```bash
cd packages/payment-admin-web/backend
ENVIRONMENT=production go run cmd/server/main.go
# Expected: PROD-SAFETY: MTLS_SERVER_CERT... panic
```

**Test 2: Development Insecure Fallback**
```bash
cd packages/payment-admin-web/backend
ENVIRONMENT=dev INSECURE_DIAL=1 go run cmd/server/main.go
# Expected: Service starts, uses insecure connections
```

**Test 3: Development with Certs**
```bash
# Generate test certificates
openssl req -newkey rsa:2048 -nodes -keyout key.pem -x509 -days 365 -out cert.pem

MTLS_SERVER_CERT=cert.pem MTLS_SERVER_KEY=key.pem MTLS_CA_CERT=cert.pem \
  go run cmd/server/main.go
# Expected: Service starts, uses mTLS connections
```

**Test 4: Expired Certificate**
```bash
# Create expired cert (use date before today)
openssl req -newkey rsa:2048 -nodes -keyout key.pem \
  -x509 -days 1 -out cert.pem && sleep 86400

MTLS_SERVER_CERT=cert.pem MTLS_SERVER_KEY=key.pem MTLS_CA_CERT=cert.pem \
  ENVIRONMENT=production go run cmd/server/main.go
# Expected: certificate expired... panic
```

## Deployment Preparation

### Pre-Deployment Infrastructure
- [ ] Certificate generation/provisioning pipeline ready
- [ ] K8s secrets created: `payment-mtls-certs` (cert + key)
- [ ] K8s configmap created: `payment-ca-cert` (CA cert)
- [ ] Helm charts updated with certificate volume mounts
- [ ] Secrets/configmaps deployed to staging first

### Service Deployment Order
- [ ] Deploy payment-util (mtls package) first (no breaking changes)
- [ ] Deploy all service changes together (they're backward compatible)
  - payment-admin-web
  - api-gateway
  - user-merchant-core
  - payment-core
  - order-core
- [ ] All services can start with or without mTLS certs in dev/staging
- [ ] Production deployment requires mTLS certs configured

### Monitoring Setup
- [ ] Certificate expiration monitoring (alert 30+ days before)
- [ ] Service startup log monitoring for mTLS errors
- [ ] gRPC connection success/failure metrics
- [ ] Certificate validation error tracking

## Phase 2 Future Work (Not Implemented Yet)

### Server-Side mTLS Enforcement
- [ ] Update all gRPC server handlers to require client certificates
- [ ] Implement client CN validation against whitelist
- [ ] Load CN whitelist from config-center per service
- [ ] Add certificate pinning for critical paths

### Operations
- [ ] Implement automated certificate rotation
- [ ] Add certificate age tracking to metrics
- [ ] Create audit log entries for mTLS events
- [ ] Build dashboard for certificate status

### Compliance
- [ ] Document certificate lifecycle (generation, rotation, retirement)
- [ ] Create runbook for certificate emergency renewal
- [ ] Set up automated alerts for certificate issues
- [ ] Add certificate validation to deployment pipeline

## Sign-Off

### Development
- Author: [Developer Name]
- Date Completed: [TBD]
- Review Status: [ ] Pending [ ] In Progress [x] Ready for Review

### Code Review
- Reviewer 1: [ ]
- Reviewer 2: [ ]
- Security Review: [ ]

### QA Testing
- Unit Tests: [ ]
- Integration Tests: [ ]
- Load Testing: [ ]
- Failover Testing: [ ]

### Deployment Approval
- Ops Lead: [ ]
- Security Lead: [ ]
- Project Lead: [ ]

### Post-Deployment Verification
- Service Startup: [ ]
- Inter-service Communication: [ ]
- Certificate Validation: [ ]
- Performance Baseline: [ ]
- Alerting & Monitoring: [ ]

## Notes & Observations

- All service changes are backward compatible (dev-friendly)
- mTLS enforces automatically in production (ENVIRONMENT=prod)
- No changes needed to gRPC server implementations for Phase 1
- Test paths remain insecure (acceptable for non-production contexts)
- Certificate paths must be absolute (relative paths not recommended)
- Certificate files should have restricted permissions (0600 for keys)

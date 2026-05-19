# kitex_gen — user-merchant-core Kitex stubs

6 services (4 protos):
- merchant.proto → MerchantService + AuditService
- merchant_secret.proto → MerchantSecretService
- user.proto → UserService
- user_card.proto → UserCardService + UserCardInternalService

Generate: `./idl/generate.sh usermerchant`

Server: MultiService 模式注册 6 个 service 到同一端口 (见 internal/server/grpc.go).

Callers:
- order-core/internal/* — 调 MerchantService.AuthenticateByAPIKey + UserCardService
- payment-core — 调 MerchantService
- card-center — 调 UserCardService
- payment-admin-web — 调全部 6 个

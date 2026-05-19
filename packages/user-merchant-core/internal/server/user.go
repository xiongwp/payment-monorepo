package server

import (
	"context"
	"fmt"
	"strconv"

	usermerchantv1 "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1"
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/service"
)

// UserServer adapts *service.UserService onto the gRPC UserService.
type UserServer struct {
	usermerchantv1.UnimplementedUserServiceServer
	svc *service.UserService
}

// NewUserServer constructs the gRPC adapter for the User service.
func NewUserServer(svc *service.UserService) *UserServer { return &UserServer{svc: svc} }

// ─── conversions ─────────────────────────────────────────────────────────────

func pbUser(u *domain.User) *usermerchantv1.User {
	if u == nil {
		return nil
	}
	out := &usermerchantv1.User{
		Id:             strconv.FormatInt(u.ID, 10),
		Status:         pbUserStatus(u.Status),
		TotpEnabled:    u.TOTPEnabled,
		EmailVerified:  u.EmailVerified,
		PhoneVerified:  u.PhoneVerified,
		CreatedMs:      u.CreatedAt.UnixMilli(),
		UpdatedMs:      u.UpdatedAt.UnixMilli(),
	}
	if u.Username != nil {
		out.Username = *u.Username
	}
	if u.Email != nil {
		out.Email = *u.Email
	}
	if u.Phone != nil {
		out.Phone = *u.Phone
	}
	if u.LastLoginAt != nil {
		out.LastLoginMs = u.LastLoginAt.UnixMilli()
	}
	return out
}

func pbUserStatus(s int8) usermerchantv1.UserStatus {
	switch s {
	case domain.UserStatusActive:
		return usermerchantv1.UserStatus_USER_STATUS_ACTIVE
	case domain.UserStatusDisabled:
		return usermerchantv1.UserStatus_USER_STATUS_DISABLED
	case domain.UserStatusLocked:
		return usermerchantv1.UserStatus_USER_STATUS_LOCKED
	case domain.UserStatusDeleted:
		return usermerchantv1.UserStatus_USER_STATUS_DELETED
	default:
		return usermerchantv1.UserStatus_USER_STATUS_UNSPECIFIED
	}
}

// pbIdentityFromAuth: domain.UserAuth → wire Identity（password 是密码登录主表
// 冗余条目，外部展示不需要）。
func pbIdentityFromAuth(a *domain.UserAuth) *usermerchantv1.Identity {
	if a == nil {
		return nil
	}
	return &usermerchantv1.Identity{
		Type:       pbIdentityType(a.AuthType),
		Identifier: a.Identifier,
		Verified:   a.Verified,
		CreatedMs:  a.CreatedAt.UnixMilli(),
	}
}

func pbIdentityType(s string) usermerchantv1.IdentityType {
	switch s {
	case domain.AuthTypePassword:
		return usermerchantv1.IdentityType_IDENTITY_TYPE_PASSWORD
	case domain.AuthTypeEmail:
		return usermerchantv1.IdentityType_IDENTITY_TYPE_EMAIL
	case domain.AuthTypePhone:
		return usermerchantv1.IdentityType_IDENTITY_TYPE_PHONE
	case domain.AuthTypeWechat:
		return usermerchantv1.IdentityType_IDENTITY_TYPE_WECHAT
	case domain.AuthTypeGithub:
		return usermerchantv1.IdentityType_IDENTITY_TYPE_GITHUB
	case domain.AuthTypeGoogle:
		return usermerchantv1.IdentityType_IDENTITY_TYPE_GOOGLE
	case domain.AuthTypeApple:
		return usermerchantv1.IdentityType_IDENTITY_TYPE_APPLE
	default:
		return usermerchantv1.IdentityType_IDENTITY_TYPE_UNSPECIFIED
	}
}

func authTypeFromPB(t usermerchantv1.IdentityType) string {
	switch t {
	case usermerchantv1.IdentityType_IDENTITY_TYPE_PASSWORD:
		return domain.AuthTypePassword
	case usermerchantv1.IdentityType_IDENTITY_TYPE_EMAIL:
		return domain.AuthTypeEmail
	case usermerchantv1.IdentityType_IDENTITY_TYPE_PHONE:
		return domain.AuthTypePhone
	case usermerchantv1.IdentityType_IDENTITY_TYPE_WECHAT:
		return domain.AuthTypeWechat
	case usermerchantv1.IdentityType_IDENTITY_TYPE_GITHUB:
		return domain.AuthTypeGithub
	case usermerchantv1.IdentityType_IDENTITY_TYPE_GOOGLE:
		return domain.AuthTypeGoogle
	case usermerchantv1.IdentityType_IDENTITY_TYPE_APPLE:
		return domain.AuthTypeApple
	default:
		return ""
	}
}

func pbUserAccount(a *domain.UserAccount) *usermerchantv1.UserAccount {
	if a == nil {
		return nil
	}
	return &usermerchantv1.UserAccount{
		UserId:    strconv.FormatInt(a.UserID, 10),
		Currency:  a.Currency,
		AccountId: a.AccountID,
		Status:    a.Status,
		CreatedMs: a.CreatedAt.UnixMilli(),
	}
}

func parseUserID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: invalid user_id", domain.ErrValidation)
	}
	return id, nil
}

// ─── RPC handlers ────────────────────────────────────────────────────────────

func (h *UserServer) Register(ctx context.Context, req *usermerchantv1.RegisterRequest) (*usermerchantv1.RegisterResponse, error) {
	res, err := h.svc.Register(ctx, &service.RegisterInput{
		Username: req.GetUsername(), Email: req.GetEmail(),
		Phone: req.GetPhone(), Password: req.GetPassword(),
		Currency:        req.GetCurrency(),
		IPAddress:       req.GetIpAddress(),
		DeviceID:        req.GetDeviceId(),
		UserAgent:       req.GetUserAgent(),
		RiskSessionID:   req.GetRiskSessionId(),
		FingerprintHash: req.GetFingerprintHash(),
		UAHash:          req.GetUaHash(),
		Metadata:        req.GetMetadata(),
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.RegisterResponse{
		User: pbUser(res.User), Jwt: res.JWT, RiskReview: res.RiskReview,
		NeedsOtp: res.NeedsOTP, OtpChallenge: res.OTPChallenge,
	}, nil
}

func (h *UserServer) Login(ctx context.Context, req *usermerchantv1.LoginRequest) (*usermerchantv1.LoginResponse, error) {
	res, err := h.svc.Login(ctx, &service.LoginInput{
		LoginID: req.GetLoginId(), Password: req.GetPassword(),
		IPAddress:       req.GetIpAddress(),
		DeviceID:        req.GetDeviceId(),
		UserAgent:       req.GetUserAgent(),
		RiskSessionID:   req.GetRiskSessionId(),
		FingerprintHash: req.GetFingerprintHash(),
		Metadata:        req.GetMetadata(),
	})
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.LoginResponse{
		Jwt: res.JWT, User: pbUser(res.User),
		NeedsOtp: res.NeedsOTP, OtpChallenge: res.OTPChallenge,
	}, nil
}

func (h *UserServer) OAuthLogin(ctx context.Context, _ *usermerchantv1.OAuthLoginRequest) (*usermerchantv1.LoginResponse, error) {
	// 占位：OAuth 走 BindIdentity → Login 的两步，后续接入 Google/Wechat/GitHub
	// SDK 后再实现。返回 Unimplemented 让上游知道。
	return nil, grpcErr(fmt.Errorf("%w: OAuthLogin not yet wired", domain.ErrValidation))
}

func (h *UserServer) VerifyOTP(ctx context.Context, req *usermerchantv1.VerifyOTPRequest) (*usermerchantv1.LoginResponse, error) {
	res, err := h.svc.VerifyOTP(ctx, req.GetOtpChallenge(), req.GetCode())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.LoginResponse{Jwt: res.JWT, User: pbUser(res.User)}, nil
}

func (h *UserServer) SendCode(ctx context.Context, req *usermerchantv1.SendCodeRequest) (*usermerchantv1.SendCodeResponse, error) {
	if err := h.svc.SendCode(ctx, req.GetIdentifier(), req.GetPurpose()); err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.SendCodeResponse{Sent: true}, nil
}

func (h *UserServer) VerifyCode(ctx context.Context, req *usermerchantv1.VerifyCodeRequest) (*usermerchantv1.VerifyCodeResponse, error) {
	if err := h.svc.VerifyCode(ctx, req.GetIdentifier(), req.GetCode(), req.GetPurpose()); err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.VerifyCodeResponse{Ok: true}, nil
}

func (h *UserServer) BindIdentity(ctx context.Context, req *usermerchantv1.BindIdentityRequest) (*usermerchantv1.BindIdentityResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	a, err := h.svc.BindAuth(ctx, uid, authTypeFromPB(req.GetType()),
		req.GetIdentifier(), req.GetCredential(), req.GetEmailCode())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.BindIdentityResponse{Identity: pbIdentityFromAuth(a)}, nil
}

func (h *UserServer) UnbindIdentity(ctx context.Context, req *usermerchantv1.UnbindIdentityRequest) (*usermerchantv1.UnbindIdentityResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	if err := h.svc.UnbindAuth(ctx, uid, authTypeFromPB(req.GetType()), req.GetIdentifier()); err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.UnbindIdentityResponse{Ok: true}, nil
}

func (h *UserServer) ListIdentities(ctx context.Context, req *usermerchantv1.ListIdentitiesRequest) (*usermerchantv1.ListIdentitiesResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	auths, err := h.svc.ListAuths(ctx, uid)
	if err != nil {
		return nil, grpcErr(err)
	}
	out := make([]*usermerchantv1.Identity, 0, len(auths))
	for _, a := range auths {
		// password 类型的 auth 是密码登录冗余，不展示给前端
		if a.AuthType == domain.AuthTypePassword {
			continue
		}
		out = append(out, pbIdentityFromAuth(a))
	}
	return &usermerchantv1.ListIdentitiesResponse{Identities: out}, nil
}

func (h *UserServer) EnableTOTP(ctx context.Context, req *usermerchantv1.EnableTOTPRequest) (*usermerchantv1.EnableTOTPResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	secret, url, err := h.svc.EnableTOTP(ctx, uid)
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.EnableTOTPResponse{SecretBase32: secret, OtpauthUrl: url}, nil
}

func (h *UserServer) ChangePassword(ctx context.Context, req *usermerchantv1.ChangePasswordRequest) (*usermerchantv1.ChangePasswordResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	if err := h.svc.ChangePassword(ctx, uid, req.GetOldPassword(), req.GetEmailCode(), req.GetNewPassword()); err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.ChangePasswordResponse{Ok: true}, nil
}

func (h *UserServer) GetUser(ctx context.Context, req *usermerchantv1.GetUserRequest) (*usermerchantv1.User, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	u, err := h.svc.GetUser(ctx, uid)
	if err != nil {
		return nil, grpcErr(err)
	}
	return pbUser(u), nil
}

func (h *UserServer) BindUserAccount(ctx context.Context, req *usermerchantv1.BindUserAccountRequest) (*usermerchantv1.BindUserAccountResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	if err := h.svc.BindUserAccount(ctx, uid, req.GetCurrency(), req.GetAccountId()); err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.BindUserAccountResponse{Ok: true}, nil
}

func (h *UserServer) ListUserAccounts(ctx context.Context, req *usermerchantv1.ListUserAccountsRequest) (*usermerchantv1.ListUserAccountsResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	accs, err := h.svc.ListUserAccounts(ctx, uid)
	if err != nil {
		return nil, grpcErr(err)
	}
	out := make([]*usermerchantv1.UserAccount, 0, len(accs))
	for _, a := range accs {
		out = append(out, pbUserAccount(a))
	}
	return &usermerchantv1.ListUserAccountsResponse{Accounts: out}, nil
}

func (h *UserServer) IntrospectToken(ctx context.Context, req *usermerchantv1.IntrospectTokenRequest) (*usermerchantv1.IntrospectTokenResponse, error) {
	res, err := h.svc.IntrospectToken(ctx, req.GetJwt())
	if err != nil {
		return nil, grpcErr(err)
	}
	out := &usermerchantv1.IntrospectTokenResponse{
		Valid:         res.Valid,
		EmailVerified: res.EmailVerified,
		Scopes:        res.Scopes,
		Permissions:   res.Permissions,
	}
	if res.Valid {
		out.UserId = strconv.FormatInt(res.UserID, 10)
		out.ExpiresMs = res.ExpiresAt.UnixMilli()
	}
	return out, nil
}

func (h *UserServer) Logout(ctx context.Context, req *usermerchantv1.LogoutRequest) (*usermerchantv1.LogoutResponse, error) {
	if jwt := req.GetJwt(); jwt != "" {
		if err := h.svc.Logout(ctx, jwt); err != nil {
			return nil, grpcErr(err)
		}
		return &usermerchantv1.LogoutResponse{Ok: true}, nil
	}
	if req.GetUserId() != "" {
		uid, err := parseUserID(req.GetUserId())
		if err != nil {
			return nil, grpcErr(err)
		}
		if err := h.svc.LogoutAll(ctx, uid); err != nil {
			return nil, grpcErr(err)
		}
		return &usermerchantv1.LogoutResponse{Ok: true}, nil
	}
	return nil, grpcErr(fmt.Errorf("%w: jwt or user_id required", domain.ErrValidation))
}

func (h *UserServer) LogoutAll(ctx context.Context, req *usermerchantv1.LogoutAllRequest) (*usermerchantv1.LogoutResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	if err := h.svc.LogoutAll(ctx, uid); err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.LogoutResponse{Ok: true}, nil
}

func (h *UserServer) HasPermission(ctx context.Context, req *usermerchantv1.HasPermissionRequest) (*usermerchantv1.HasPermissionResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	ok, err := h.svc.HasPermission(ctx, uid, req.GetPermissionCode())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.HasPermissionResponse{Allowed: ok}, nil
}

func (h *UserServer) ListPermissions(ctx context.Context, req *usermerchantv1.ListPermissionsRequest) (*usermerchantv1.ListPermissionsResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	perms, err := h.svc.ListPermissions(ctx, uid)
	if err != nil {
		return nil, grpcErr(err)
	}
	codes := make([]string, 0, len(perms))
	for _, p := range perms {
		codes = append(codes, p.Code)
	}
	return &usermerchantv1.ListPermissionsResponse{Codes: codes}, nil
}

func (h *UserServer) DeleteUser(ctx context.Context, req *usermerchantv1.DeleteUserRequest) (*usermerchantv1.DeleteUserResponse, error) {
	uid, err := parseUserID(req.GetUserId())
	if err != nil {
		return nil, grpcErr(err)
	}
	if err := h.svc.DeleteUser(ctx, uid, req.GetReason()); err != nil {
		return nil, grpcErr(err)
	}
	return &usermerchantv1.DeleteUserResponse{Ok: true}, nil
}

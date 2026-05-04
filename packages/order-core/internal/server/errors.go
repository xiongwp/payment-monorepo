package server

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xiongwp/order-core/internal/domain"
)

// grpcErr maps domain-level errors to gRPC status codes so the admin-web gets
// clean 404/400/409 responses instead of opaque Internal.
func grpcErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrPaymentIntentNotFound),
		errors.Is(err, domain.ErrChargeNotFound),
		errors.Is(err, domain.ErrRefundNotFound),
		errors.Is(err, domain.ErrPayActionNotFound),
		errors.Is(err, domain.ErrLedgerAccountNotFound),
		errors.Is(err, domain.ErrDisputeNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, domain.ErrValidation):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrPayActionNotPending):
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
